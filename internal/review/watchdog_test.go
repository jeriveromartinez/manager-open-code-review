package review

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

type watchdogFixture struct {
	s      *store.Store
	ctx    context.Context
	events *runTestEvents
	jobs   *runTestJobs
	w      *Watchdog
}

func newWatchdogFixture(t *testing.T) *watchdogFixture {
	t.Helper()

	s := testutil.NewStore(t)
	ctx := context.Background()
	installation := store.Installation{ID: 91, AccountLogin: "acme", AccountType: "Organization"}
	if err := s.UpsertInstallation(ctx, installation); err != nil {
		t.Fatalf("UpsertInstallation: %v", err)
	}
	repo := store.Repository{ID: 92, InstallationID: 91, FullName: "acme/app", DefaultBranch: "main", Enabled: true}
	if err := s.UpsertRepository(ctx, repo); err != nil {
		t.Fatalf("UpsertRepository: %v", err)
	}

	ev := &runTestEvents{}
	jobs := &runTestJobs{}
	return &watchdogFixture{
		s: s, ctx: ctx, events: ev, jobs: jobs,
		w: &Watchdog{Store: s, Events: ev, Jobs: jobs, StaleAfter: time.Hour},
	}
}

func (f *watchdogFixture) runningRun(t *testing.T, number int, startedAt time.Time) store.ReviewRun {
	t.Helper()

	pr := store.PullRequest{
		RepositoryID: 92, Number: number, Title: "PR", HeadSHA: "sha-" + strconv.Itoa(number),
		BaseRef: "main", State: "open", QAStatus: store.StatusReviewing,
		GitHubUpdatedAt: time.Now().UTC(),
	}
	if err := f.s.DB.WithContext(f.ctx).Create(&pr).Error; err != nil {
		t.Fatalf("create pull request: %v", err)
	}
	run, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, Status: store.RunRunning,
		StartedAt: &startedAt, CloneTokenEnc: "sealed-token",
	})
	if err != nil {
		t.Fatalf("create running run: %v", err)
	}
	return run
}

// TestWatchdogSweepPublishesInterruptedRuns proves the sweep fails orphaned
// runs and then publishes them through the same path as a terminal failure:
// browser events plus the publish job per run.
func TestWatchdogSweepPublishesInterruptedRuns(t *testing.T) {
	f := newWatchdogFixture(t)

	first := f.runningRun(t, 1, time.Now().UTC().Add(-3*time.Hour))
	second := f.runningRun(t, 2, time.Now().UTC().Add(-2*time.Hour))
	recent := f.runningRun(t, 3, time.Now().UTC().Add(-time.Minute))

	if err := f.w.Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}

	for _, run := range []store.ReviewRun{first, second} {
		stored, err := f.s.ReviewRun(f.ctx, run.ID)
		if err != nil {
			t.Fatalf("ReviewRun(%d): %v", run.ID, err)
		}
		if stored.Status != store.RunFailed || stored.ErrorCode != store.RunErrorInterrupted {
			t.Errorf("run %d = %q/%q, want failed/%s",
				run.ID, stored.Status, stored.ErrorCode, store.RunErrorInterrupted)
		}
		if stored.FinishedAt == nil {
			t.Errorf("run %d finished_at is nil", run.ID)
		}
		for _, ev := range []events.Event{
			{Type: "run.updated", ID: strconv.FormatUint(uint64(run.ID), 10)},
			{Type: "pr.updated", ID: strconv.FormatUint(uint64(run.PullRequestID), 10)},
		} {
			if !f.events.has(ev) {
				t.Errorf("run %d: missing event %+v; got %+v", run.ID, ev, f.events.items)
			}
		}
	}

	if got := f.jobs.published(); len(got) != 2 || got[0] != first.ID || got[1] != second.ID {
		t.Errorf("EnqueuePublish calls = %v, want [%d %d]", got, first.ID, second.ID)
	}

	for _, run := range []store.ReviewRun{first, second} {
		var pr store.PullRequest
		if err := f.s.DB.WithContext(f.ctx).Take(&pr, run.PullRequestID).Error; err != nil {
			t.Fatalf("load pull request of run %d: %v", run.ID, err)
		}
		if pr.QAStatus != store.StatusInconclusive {
			t.Errorf("pull request %d status = %q, want %q",
				pr.ID, pr.QAStatus, store.StatusInconclusive)
		}
	}

	untouched, err := f.s.ReviewRun(f.ctx, recent.ID)
	if err != nil {
		t.Fatalf("ReviewRun(recent): %v", err)
	}
	if untouched.Status != store.RunRunning || untouched.FinishedAt != nil {
		t.Errorf("recent run = %+v, want still running", untouched)
	}
	var recentPR store.PullRequest
	if err := f.s.DB.WithContext(f.ctx).Take(&recentPR, recent.PullRequestID).Error; err != nil {
		t.Fatalf("load pull request of recent run: %v", err)
	}
	if recentPR.QAStatus != store.StatusReviewing {
		t.Errorf("recent pull request status = %q, want %q",
			recentPR.QAStatus, store.StatusReviewing)
	}
	for _, ev := range []events.Event{
		{Type: "run.updated", ID: strconv.FormatUint(uint64(recent.ID), 10)},
	} {
		if f.events.has(ev) {
			t.Errorf("recent run published %+v, want nothing", ev)
		}
	}
	if got := f.jobs.published(); len(got) != 2 {
		t.Errorf("EnqueuePublish calls after a healthy run = %v, want none for it", got)
	}
}

// TestWatchdogSweepIdempotent verifies a second sweep publishes nothing: the
// runs are already terminal, so they are not reported twice.
func TestWatchdogSweepIdempotent(t *testing.T) {
	f := newWatchdogFixture(t)
	f.runningRun(t, 1, time.Now().UTC().Add(-3*time.Hour))

	if err := f.w.Sweep(f.ctx); err != nil {
		t.Fatalf("first Sweep() error = %v", err)
	}
	eventsAfterFirst := f.events.len()
	publishesAfterFirst := len(f.jobs.published())

	if err := f.w.Sweep(f.ctx); err != nil {
		t.Fatalf("second Sweep() error = %v", err)
	}
	if got := f.events.len(); got != eventsAfterFirst {
		t.Errorf("events after second sweep = %d, want %d", got, eventsAfterFirst)
	}
	if got := len(f.jobs.published()); got != publishesAfterFirst {
		t.Errorf("publish jobs after second sweep = %d, want %d", got, publishesAfterFirst)
	}
}
