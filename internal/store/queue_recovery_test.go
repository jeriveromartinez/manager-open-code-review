package store_test

import (
	"encoding/json"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func (f *fixture) liveReviewJob(runID uint) {
	f.t.Helper()
	args, err := json.Marshal(map[string]any{"run_id": runID})
	if err != nil {
		f.t.Fatalf("marshal review job args: %v", err)
	}
	if _, err := f.s.Pool.Exec(f.ctx, `
		INSERT INTO river_job
			(state, kind, args, max_attempts, priority, queue, attempted_at, scheduled_at)
		VALUES ('available', 'review_run', $1::jsonb, 3, 1, 'review', now(), now())`,
		string(args)); err != nil {
		f.t.Fatalf("insert live review job for run %d: %v", runID, err)
	}
}

func (f *fixture) checkArtifact(pr store.PullRequest, headSHA string) {
	f.t.Helper()
	if _, err := f.s.UpsertArtifact(f.ctx, store.GitHubArtifact{
		PullRequestID: pr.ID, Kind: store.ArtifactCheck, HeadSHA: headSHA, ExternalID: "555",
	}); err != nil {
		f.t.Fatalf("seed check artifact: %v", err)
	}
}

// TestStrandedQueuedRunsReportsMissingWork covers the query the sweep relies
// on: a queued run is stranded when its live River job or its check is missing,
// and a healthy run is not reported.
func TestStrandedQueuedRunsReportsMissingWork(t *testing.T) {
	f := newFixture(t)
	inst := f.nextID()
	f.installation(inst)
	repo := f.repo(inst, inst, "acme/queue", nil, true)

	missingBoth := f.run(f.openPR(repo.ID, 1, store.StatusPending, false), store.RunQueued)
	missingCheck := f.run(f.openPR(repo.ID, 2, store.StatusPending, false), store.RunQueued)
	f.liveReviewJob(missingCheck.ID)
	healthyPR := f.openPR(repo.ID, 3, store.StatusPending, false)
	healthy := f.run(healthyPR, store.RunQueued)
	f.liveReviewJob(healthy.ID)
	f.checkArtifact(healthyPR, healthy.HeadSHA)
	ignoredRunning := f.run(f.openPR(repo.ID, 4, store.StatusPending, false), store.RunRunning)
	f.liveReviewJob(ignoredRunning.ID)

	got, err := f.s.StrandedQueuedRuns(f.ctx, 50)
	if err != nil {
		t.Fatalf("StrandedQueuedRuns: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("stranded runs = %+v, want two", got)
	}
	byID := make(map[uint]store.QueuedRunWork, len(got))
	for _, work := range got {
		byID[work.RunID] = work
	}
	if work, ok := byID[missingBoth.ID]; !ok || work.HasJob || work.HasCheck {
		t.Errorf("missingBoth = %+v, want no job and no check", work)
	}
	if work, ok := byID[missingCheck.ID]; !ok || !work.HasJob || work.HasCheck {
		t.Errorf("missingCheck = %+v, want a job and no check", work)
	}
	if _, ok := byID[healthy.ID]; ok {
		t.Error("a queued run with a job and a check was reported as stranded")
	}
	if _, ok := byID[ignoredRunning.ID]; ok {
		t.Error("a running run was reported as a queued run")
	}

	hasJob, hasCheck, err := f.s.QueuedRunWork(f.ctx, healthy.ID)
	if err != nil {
		t.Fatalf("QueuedRunWork: %v", err)
	}
	if !hasJob || !hasCheck {
		t.Errorf("QueuedRunWork(healthy) = %v/%v, want both true", hasJob, hasCheck)
	}
}

// TestActiveRunsForReconcileReturnsAllActiveRuns covers the query the sweep
// uses: every queued or running run is a candidate, because eligibility is
// re-checked under the pull request lock. Terminal runs are not.
func TestActiveRunsForReconcileReturnsAllActiveRuns(t *testing.T) {
	f := newFixture(t)
	inst := f.nextID()
	f.installation(inst)
	repo := f.repo(inst, inst, "acme/enabled", nil, true)

	closedPR := f.openPR(repo.ID, 1, store.StatusPending, false)
	f.exec(`UPDATE pull_requests SET state = 'closed' WHERE id = ?`, closedPR.ID)
	closed := f.run(closedPR, store.RunQueued)

	mergedPR := f.openPR(repo.ID, 2, store.StatusPending, false)
	f.exec(`UPDATE pull_requests SET state = 'merged' WHERE id = ?`, mergedPR.ID)
	merged := f.run(mergedPR, store.RunRunning)

	open := f.run(f.openPR(repo.ID, 3, store.StatusPending, false), store.RunQueued)
	terminal := f.run(f.openPR(repo.ID, 4, store.StatusPending, false), store.RunSucceeded)

	got, err := f.s.ActiveRunsForReconcile(f.ctx, 50)
	if err != nil {
		t.Fatalf("ActiveRunsForReconcile: %v", err)
	}
	ids := make(map[uint]bool, len(got))
	for _, run := range got {
		ids[run.ID] = true
	}
	for _, run := range []store.ReviewRun{closed, merged, open} {
		if !ids[run.ID] {
			t.Errorf("active run %d was not returned", run.ID)
		}
	}
	if ids[terminal.ID] {
		t.Error("a terminal run was returned")
	}
}
