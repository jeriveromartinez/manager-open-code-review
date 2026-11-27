package review

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/settings"
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

func (f *watchdogFixture) beat(t *testing.T, run store.ReviewRun, at time.Time) {
	t.Helper()
	if err := f.s.DB.WithContext(f.ctx).Model(&store.ReviewRun{}).
		Where("id = ?", run.ID).Update("heartbeat_at", at).Error; err != nil {
		t.Fatalf("set heartbeat of run %d: %v", run.ID, err)
	}
}

// TestWatchdogSweepPublishesInterruptedRuns proves the sweep fails orphaned
// runs and then publishes them through the same path as a terminal failure:
// browser events plus the publish job per run.
func TestWatchdogSweepPublishesInterruptedRuns(t *testing.T) {
	f := newWatchdogFixture(t)

	first := f.runningRun(t, 1, time.Now().UTC().Add(-3*time.Hour))
	second := f.runningRun(t, 2, time.Now().UTC().Add(-2*time.Hour))
	recent := f.runningRun(t, 3, time.Now().UTC().Add(-time.Minute))
	f.beat(t, first, time.Now().UTC().Add(-time.Minute)) // alive: it overran
	// second keeps a NULL heartbeat: its worker is gone.

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
		wantMessage := map[uint]string{
			first.ID:  "tiempo máximo",
			second.ID: "dejó de responder",
		}[run.ID]
		if !strings.Contains(stored.ErrorMessage, wantMessage) {
			t.Errorf("run %d message = %q, want %q", run.ID, stored.ErrorMessage, wantMessage)
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

// heartbeatAt returns the stored heartbeat of a run.
func (f *watchdogFixture) heartbeatAt(t *testing.T, run store.ReviewRun) *time.Time {
	t.Helper()

	return heartbeatAt(t, f.s, f.ctx, run.ID)
}

// waitHeartbeatAfter polls until the run records a beat later than after.
func (f *watchdogFixture) waitHeartbeatAfter(t *testing.T, run store.ReviewRun, after time.Time) {
	t.Helper()

	waitHeartbeatAfter(t, f.s, f.ctx, run.ID, after)
}

// TestHeartbeatOutlivesReviewDeadline pins #130: the heartbeat reports that the
// worker process is alive, not that the pipeline still has budget. Started on
// the pipeline context it keeps beating after the review deadline expires, so
// the sweep reports a run that overran as overrun instead of as a lost worker.
func TestHeartbeatOutlivesReviewDeadline(t *testing.T) {
	f := newWatchdogFixture(t)
	run := f.runningRun(t, 1, time.Now().UTC().Add(-2*time.Hour))

	runner := &Runner{Store: f.s, heartbeatEvery: 20 * time.Millisecond}
	log := newRunLogSink(f.s, f.events, run.ID, 0)

	pipelineCtx, cancelPipeline := context.WithCancel(f.ctx)
	defer cancelPipeline()
	runCtx, cancelReview := context.WithTimeout(pipelineCtx, 100*time.Millisecond)
	defer cancelReview()

	stop := runner.heartbeat(pipelineCtx, run.ID, "", log, cancelPipeline)
	<-runCtx.Done()
	deadline := time.Now().UTC()

	// runCtx is the deadline execute() gives the pipeline and the one the old
	// wiring gave the heartbeat: with it the run froze exactly here.
	f.waitHeartbeatAfter(t, run, deadline.Add(150*time.Millisecond))
	stop()

	if err := f.w.Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}
	stored, err := f.s.ReviewRun(f.ctx, run.ID)
	if err != nil {
		t.Fatalf("ReviewRun(%d): %v", run.ID, err)
	}
	if stored.Status != store.RunFailed || stored.ErrorCode != store.RunErrorInterrupted {
		t.Fatalf("run %d = %q/%q, want failed/%s",
			run.ID, stored.Status, stored.ErrorCode, store.RunErrorInterrupted)
	}
	if !strings.Contains(stored.ErrorMessage, "tiempo máximo") {
		t.Errorf("run %d message = %q, want the overrun message", run.ID, stored.ErrorMessage)
	}
	if strings.Contains(stored.ErrorMessage, "dejó de responder") {
		t.Errorf("run %d message = %q, reported a live worker as lost", run.ID, stored.ErrorMessage)
	}
}

// TestHeartbeatStopsWithReviewDeadlineContext keeps the regression visible: a
// heartbeat started on the review deadline context stops when the pipeline runs
// out of budget, which is why InterruptStaleRuns always found silence. It also
// proves the assertion above discriminates.
func TestHeartbeatStopsWithReviewDeadlineContext(t *testing.T) {
	f := newWatchdogFixture(t)
	run := f.runningRun(t, 1, time.Now().UTC().Add(-2*time.Hour))

	runner := &Runner{Store: f.s, heartbeatEvery: 20 * time.Millisecond}
	log := newRunLogSink(f.s, f.events, run.ID, 0)

	pipelineCtx, cancelPipeline := context.WithCancel(f.ctx)
	defer cancelPipeline()
	runCtx, cancelReview := context.WithTimeout(pipelineCtx, 200*time.Millisecond)
	defer cancelReview()

	stop := runner.heartbeat(runCtx, run.ID, "", log, cancelPipeline)
	f.waitHeartbeatAfter(t, run, time.Time{})
	<-runCtx.Done()

	// Let the beat that races the cancellation land before freezing the value.
	time.Sleep(300 * time.Millisecond)
	frozen := f.heartbeatAt(t, run)
	if frozen == nil {
		t.Fatal("run has no heartbeat_at after one beat")
	}
	time.Sleep(200 * time.Millisecond)
	moved := f.heartbeatAt(t, run)
	stop()
	if moved == nil {
		t.Fatal("run lost its heartbeat_at")
	}
	if !moved.Equal(*frozen) {
		t.Fatalf("heartbeat_at moved from %v to %v after the review deadline: the heartbeat outlived its context",
			frozen.UTC(), moved.UTC())
	}
}

// TestHeartbeatTimeoutFitsWatchdogMargin ties the heartbeat tolerance to the
// settings rule that decides when the sweep sees a run: review_stale_after
// exceeds review_timeout by at least settings.MinReviewStaleMargin, so a
// tolerance that large or larger could not tell a live worker that overran its
// deadline from a lost one.
func TestHeartbeatTimeoutFitsWatchdogMargin(t *testing.T) {
	if heartbeatTimeout >= settings.MinReviewStaleMargin {
		t.Fatalf("heartbeatTimeout = %s, want less than settings.MinReviewStaleMargin (%s)",
			heartbeatTimeout, settings.MinReviewStaleMargin)
	}
	if heartbeatInterval >= heartbeatTimeout {
		t.Fatalf("heartbeatInterval = %s, want less than heartbeatTimeout (%s)",
			heartbeatInterval, heartbeatTimeout)
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

// TestWatchdogSweepRecoversOrphanBeforeGlobalDeadline is the #228 window test:
// a run whose lease expired is re-queued for recovery instead of waiting for
// the global stale threshold that fails it.
func TestWatchdogSweepRecoversOrphanBeforeGlobalDeadline(t *testing.T) {
	f := newWatchdogFixture(t)

	orphan := f.runningRun(t, 1, time.Now().UTC().Add(-10*time.Minute))
	f.beat(t, orphan, time.Now().UTC().Add(-store.RunLeaseTTL-time.Minute))
	// Globally stale: the watchdog owns it, so recovery must not race it.
	overrun := f.runningRun(t, 2, time.Now().UTC().Add(-3*time.Hour))
	f.beat(t, overrun, time.Now().UTC().Add(-time.Minute))

	if err := f.w.Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}

	if got := f.jobs.reviews(); len(got) != 1 || got[0] != orphan.ID {
		t.Fatalf("recovery enqueues = %v, want only [%d]", got, orphan.ID)
	}
	stored, err := f.s.ReviewRun(f.ctx, orphan.ID)
	if err != nil {
		t.Fatalf("ReviewRun(orphan): %v", err)
	}
	if stored.Status != store.RunRunning {
		t.Errorf("orphan status = %q, want it left running for the recovery claim", stored.Status)
	}
	if got := f.jobs.published(); len(got) != 1 || got[0] != overrun.ID {
		t.Errorf("publish jobs = %v, want only the globally stale run [%d]", got, overrun.ID)
	}
}
