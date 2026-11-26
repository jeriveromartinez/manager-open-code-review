package review

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/publish"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// isolatedChecks uses the real check starter for StartCheck and the real
// Canceller for CancelCheck, so the orphan-run test exercises the artifact
// lookup instead of a fake that never fails.
type isolatedChecks struct {
	start  CheckStarter
	cancel CheckStarter
}

func (c isolatedChecks) StartCheck(ctx context.Context, runID uint) error {
	return c.start.StartCheck(ctx, runID)
}

func (c isolatedChecks) CancelCheck(ctx context.Context, runID uint) error {
	return c.cancel.CancelCheck(ctx, runID)
}

// liveReviewJob seeds a River job as if a previous delivery had enqueued the
// run and the worker had not picked it up yet.
func (f *triggerFixture) liveReviewJob(runID uint) {
	f.t.Helper()
	args, err := json.Marshal(map[string]any{"run_id": runID})
	if err != nil {
		f.t.Fatalf("marshal review job args: %v", err)
	}
	if _, err := f.st.Pool.Exec(f.ctx, `
		INSERT INTO river_job
			(state, kind, args, max_attempts, priority, queue, attempted_at, scheduled_at)
		VALUES ('available', 'review_run', $1::jsonb, 3, 1, 'review', now(), now())`,
		string(args)); err != nil {
		f.t.Fatalf("insert live review job for run %d: %v", runID, err)
	}
}

// A delivery that failed before creating the run leaves the intent unfinished.
// The next identical delivery must complete it exactly once.
func TestTriggerRedeliveryCompletesFailedTrigger(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("retry", eligibleOpts())

	apps := f.trigger.Apps
	f.trigger.Apps = nil
	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err == nil {
		t.Fatal("first OnPullRequest() error = nil, want the missing app provider failure")
	}
	f.trigger.Apps = apps

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("redelivered OnPullRequest() error = %v", err)
	}

	runs := f.runsOf(pr)
	if len(runs) != 1 || runs[0].Status != store.RunQueued {
		t.Fatalf("runs = %+v, want exactly one queued run", runs)
	}
	if len(f.jobs.reviews) != 1 || f.jobs.reviews[0] != runs[0].ID {
		t.Errorf("EnqueueReview calls = %v, want [%d]", f.jobs.reviews, runs[0].ID)
	}
	if len(f.checks.started) != 1 || f.checks.started[0] != runs[0].ID {
		t.Errorf("StartCheck calls = %v, want [%d]", f.checks.started, runs[0].ID)
	}
}

// The previous head left a queued run whose check was never created. The new
// head must replace it: the missing artifact cannot block the supersede.
func TestTriggerNewHeadReplacesOrphanWithoutCheck(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("orphan", eligibleOpts())
	orphan := f.seedRun(pr, pr.HeadSHA, "main", "", store.RunQueued)

	f.trigger.Checks = isolatedChecks{
		start:  f.checks,
		cancel: &publish.Worker{Store: f.st, Apps: f.trigger.Apps},
	}

	const newHead = "sha-new-head"
	if err := f.st.DB.WithContext(f.ctx).Model(&store.PullRequest{}).
		Where("id = ?", pr.ID).
		Updates(map[string]any{"head_sha": newHead, "base_sha": "tip-2", "merge_base_sha": ""}).Error; err != nil {
		t.Fatalf("move the head: %v", err)
	}

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}

	old, err := f.st.ReviewRun(f.ctx, orphan.ID)
	if err != nil {
		t.Fatalf("ReviewRun(orphan): %v", err)
	}
	if old.Status != store.RunSuperseded {
		t.Errorf("orphan status = %q, want superseded", old.Status)
	}
	runs := f.runsOf(pr)
	if len(runs) != 2 {
		t.Fatalf("runs = %+v, want the orphan and the new head", runs)
	}
	if runs[1].HeadSHA != newHead || runs[1].Status != store.RunQueued {
		t.Errorf("new run = %+v, want queued at %q", runs[1], newHead)
	}
	if len(f.jobs.reviews) != 1 || f.jobs.reviews[0] != runs[1].ID {
		t.Errorf("EnqueueReview calls = %v, want [%d]", f.jobs.reviews, runs[1].ID)
	}
}

// A queued run with no River job and no check is invisible to PendingReviewPRs:
// the sweep must give it both without creating a second run.
func TestReconcileQueuedRunsRepairsMissingJobAndCheck(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("stranded", eligibleOpts())
	run := f.seedRun(pr, pr.HeadSHA, "main", "", store.RunQueued)

	repaired, err := f.trigger.ReconcileQueuedRuns(f.ctx, 50)
	if err != nil {
		t.Fatalf("ReconcileQueuedRuns() error = %v", err)
	}
	if repaired != 1 {
		t.Errorf("repaired = %d, want 1", repaired)
	}
	if len(f.jobs.reviews) != 1 || f.jobs.reviews[0] != run.ID {
		t.Errorf("EnqueueReview calls = %v, want [%d]", f.jobs.reviews, run.ID)
	}
	if len(f.checks.started) != 1 || f.checks.started[0] != run.ID {
		t.Errorf("StartCheck calls = %v, want [%d]", f.checks.started, run.ID)
	}
	if runs := f.runsOf(pr); len(runs) != 1 {
		t.Errorf("runs = %+v, want the original run only", runs)
	}
}

// A live job must not be duplicated by the sweep: only the missing check is
// recreated.
func TestReconcileQueuedRunsKeepsLiveJob(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("live", eligibleOpts())
	run := f.seedRun(pr, pr.HeadSHA, "main", "", store.RunQueued)
	f.liveReviewJob(run.ID)

	repaired, err := f.trigger.ReconcileQueuedRuns(f.ctx, 50)
	if err != nil {
		t.Fatalf("ReconcileQueuedRuns() error = %v", err)
	}
	if repaired != 1 {
		t.Errorf("repaired = %d, want 1 (the missing check)", repaired)
	}
	if len(f.jobs.reviews) != 0 {
		t.Errorf("EnqueueReview calls = %v, want none for a live job", f.jobs.reviews)
	}
	if len(f.checks.started) != 1 || f.checks.started[0] != run.ID {
		t.Errorf("StartCheck calls = %v, want [%d]", f.checks.started, run.ID)
	}
}

// A queued run that still has both its job and its check is healthy: the sweep
// must leave it alone.
func TestReconcileQueuedRunsLeavesCompleteRun(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("complete", eligibleOpts())
	run := f.seedRun(pr, pr.HeadSHA, "main", "", store.RunQueued)
	f.liveReviewJob(run.ID)
	if _, err := f.st.UpsertArtifact(f.ctx, store.GitHubArtifact{
		PullRequestID: pr.ID, Kind: store.ArtifactCheck, HeadSHA: run.HeadSHA, ExternalID: "555",
	}); err != nil {
		t.Fatalf("seed check artifact: %v", err)
	}

	repaired, err := f.trigger.ReconcileQueuedRuns(f.ctx, 50)
	if err != nil {
		t.Fatalf("ReconcileQueuedRuns() error = %v", err)
	}
	if repaired != 0 {
		t.Errorf("repaired = %d, want none", repaired)
	}
	if len(f.jobs.reviews) != 0 || len(f.checks.started) != 0 {
		t.Errorf("unexpected enqueues/checks: %v / %v", f.jobs.reviews, f.checks.started)
	}
}

// Closing, merging or converting a pull request to a draft must discard the
// active run instead of keeping it alive.
func TestReconcileQueuedRunsDiscardsIneligibleStates(t *testing.T) {
	cases := []struct {
		name  string
		state string
		draft bool
	}{
		{"closed", "closed", false},
		{"merged", "merged", false},
		{"draft", "open", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTriggerFixture(t)
			opts := eligibleOpts()
			opts.state = tc.state
			opts.draft = tc.draft
			pr := f.seed(tc.name, opts)
			run := f.seedRun(pr, pr.HeadSHA, "main", "", store.RunQueued)

			repaired, err := f.trigger.ReconcileQueuedRuns(f.ctx, 50)
			if err != nil {
				t.Fatalf("ReconcileQueuedRuns() error = %v", err)
			}
			if repaired != 1 {
				t.Errorf("repaired = %d, want 1", repaired)
			}
			updated, err := f.st.ReviewRun(f.ctx, run.ID)
			if err != nil {
				t.Fatalf("ReviewRun: %v", err)
			}
			if updated.Status != store.RunSuperseded {
				t.Errorf("run status = %q, want superseded", updated.Status)
			}
			if len(f.checks.cancelled) != 1 || f.checks.cancelled[0] != run.ID {
				t.Errorf("CancelCheck calls = %v, want [%d]", f.checks.cancelled, run.ID)
			}
		})
	}
}

// The sweep re-reads the pull request under the lock, so a close that lands
// after the query still discards the run.
func TestRepairQueuedRunLockedDiscardsStateChangedAfterSelection(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("recheck", eligibleOpts())
	run := f.seedRun(pr, pr.HeadSHA, "main", "", store.RunQueued)

	unlock, err := f.st.LockPR(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("LockPR: %v", err)
	}
	defer unlock()
	// The sweep selected the run while the PR was still open; the close lands
	// before the locked helper executes.
	if err := f.st.DB.WithContext(f.ctx).Model(&store.PullRequest{}).
		Where("id = ?", pr.ID).Update("state", "closed").Error; err != nil {
		t.Fatalf("close pull request: %v", err)
	}

	discarded, err := f.trigger.repairQueuedRunLocked(f.ctx, store.QueuedRunWork{
		RunID: run.ID, PullRequestID: pr.ID,
	})
	if err != nil {
		t.Fatalf("repairQueuedRunLocked: %v", err)
	}
	if !discarded {
		t.Fatal("repairQueuedRunLocked kept the run of a closed pull request")
	}
	updated, err := f.st.ReviewRun(f.ctx, run.ID)
	if err != nil {
		t.Fatalf("ReviewRun: %v", err)
	}
	if updated.Status != store.RunSuperseded {
		t.Errorf("run status = %q, want superseded", updated.Status)
	}
	if len(f.checks.cancelled) != 1 || f.checks.cancelled[0] != run.ID {
		t.Errorf("CancelCheck calls = %v, want [%d]", f.checks.cancelled, run.ID)
	}
}

// A queued run of an older head/base must not be re-enqueued: the sweep
// supersedes it so the current comparison can be scheduled.
func TestRepairQueuedRunLockedDiscardsStaleComparison(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("stale", eligibleOpts())
	stale := f.seedRun(pr, pr.HeadSHA, "main", "mb-old", store.RunQueued)
	if err := f.st.DB.WithContext(f.ctx).Model(&store.PullRequest{}).
		Where("id = ?", pr.ID).
		Updates(map[string]any{"head_sha": "sha-new", "base_sha": "tip-2", "merge_base_sha": ""}).Error; err != nil {
		t.Fatalf("move the head: %v", err)
	}

	unlock, err := f.st.LockPR(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("LockPR: %v", err)
	}
	defer unlock()

	discarded, err := f.trigger.repairQueuedRunLocked(f.ctx, store.QueuedRunWork{
		RunID: stale.ID, PullRequestID: pr.ID,
	})
	if err != nil {
		t.Fatalf("repairQueuedRunLocked: %v", err)
	}
	if !discarded {
		t.Fatal("repairQueuedRunLocked kept a run of an older comparison")
	}
	updated, err := f.st.ReviewRun(f.ctx, stale.ID)
	if err != nil {
		t.Fatalf("ReviewRun: %v", err)
	}
	if updated.Status != store.RunSuperseded {
		t.Errorf("stale run status = %q, want superseded", updated.Status)
	}
	if len(f.jobs.reviews) != 0 || len(f.checks.started) != 0 {
		t.Errorf("stale run repaired instead of superseded: jobs %v, checks %v", f.jobs.reviews, f.checks.started)
	}
	if len(f.checks.cancelled) != 1 || f.checks.cancelled[0] != stale.ID {
		t.Errorf("CancelCheck calls = %v, want [%d]", f.checks.cancelled, stale.ID)
	}
}

// A webhook delivery and the polling sweep can race for the same pull request.
// The advisory lock must collapse them into one run.
func TestTriggerReopenConcurrentWithPollingQueuesOnce(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("reopen", eligibleOpts())

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs <- f.trigger.OnPullRequest(f.ctx, pr.ID)
	}()
	go func() {
		defer wg.Done()
		_, err := f.trigger.EnqueuePending(f.ctx, 50)
		errs <- err
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent trigger/reconcile error = %v", err)
		}
	}

	runs := f.runsOf(pr)
	active := 0
	for _, run := range runs {
		if run.Status == store.RunQueued || run.Status == store.RunRunning {
			active++
		}
	}
	if len(runs) != 1 || active != 1 {
		t.Fatalf("runs = %+v, want exactly one active run", runs)
	}
}

// A full batch of recent contributions the external policy rejects must not
// consume the limit: the older eligible candidate is still scheduled and the
// rejected ones keep their recorded reason.
func TestEnqueuePendingDoesNotLetRejectedExternalPRsConsumeBatch(t *testing.T) {
	f := newTriggerFixture(t)
	opts := eligibleOpts()
	opts.reviewerLogin = "human-qa"
	eligible := f.seed("eligible", opts)

	old := time.Now().UTC().Add(-time.Hour)
	if err := f.st.DB.WithContext(f.ctx).Exec(
		`UPDATE pull_requests SET updated_at = ? WHERE id = ?`, old, eligible.ID,
	).Error; err != nil {
		t.Fatalf("age eligible pull request: %v", err)
	}
	var firstExternal store.PullRequest
	for i := range 51 {
		pr := store.PullRequest{
			RepositoryID: eligible.RepositoryID, Number: 100 + i,
			Title: fmt.Sprintf("external %d", i), AuthorLogin: "contributor",
			AuthorAssociation: "OWNER", HeadSHA: fmt.Sprintf("sha-external-%d", i),
			BaseRef: "main", HeadRef: "external", HTMLURL: fmt.Sprintf("https://github.com/acme/eligible/pull/%d", 100+i),
			IsFork: true, Origin: store.OriginCrossRepo, State: "open",
			QAStatus: store.StatusPending, GitHubUpdatedAt: time.Now().UTC(),
		}
		if err := f.st.DB.WithContext(f.ctx).Create(&pr).Error; err != nil {
			t.Fatalf("seed external pull request %d: %v", i, err)
		}
		if i == 0 {
			firstExternal = pr
		}
	}

	created, err := f.trigger.EnqueuePending(f.ctx, 50)
	if err != nil {
		t.Fatalf("EnqueuePending() error = %v", err)
	}
	if created != 1 {
		t.Fatalf("created = %d, want the eligible PR scheduled despite the full rejected batch", created)
	}
	if runs := f.runsOf(eligible); len(runs) != 1 {
		t.Errorf("eligible runs = %+v, want one", runs)
	}
	request, err := f.st.ReviewRequestFor(f.ctx, firstExternal.ID, firstExternal.HeadSHA)
	if err != nil {
		t.Fatalf("ReviewRequestFor: %v", err)
	}
	if request.Cause != store.ReviewRequestCauseFork {
		t.Errorf("rejected request cause = %q, want %q", request.Cause, store.ReviewRequestCauseFork)
	}
}
