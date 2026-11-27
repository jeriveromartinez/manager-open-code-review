package store_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// claimQueuedRun creates one queued run of a fresh pull request and claims it,
// standing in for a worker that took the job.
func claimQueuedRun(t *testing.T, f *fixture) store.ReviewRun {
	t.Helper()

	instID := f.nextID()
	f.installation(instID)
	repo := f.repo(instID, instID, fmt.Sprintf("acme/lease-%d", instID), nil, true)
	pr := f.openPR(repo.ID, int(instID), store.StatusPending, false)
	queued := f.run(pr, store.RunQueued)
	run, claimed, err := f.s.ClaimReviewRun(f.ctx, queued.ID)
	if err != nil || !claimed {
		t.Fatalf("first claim = claimed %v, err %v", claimed, err)
	}
	return run
}

func expireLease(t *testing.T, f *fixture, runID uint) {
	t.Helper()

	stale := time.Now().UTC().Add(-store.RunLeaseTTL - time.Minute)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.ReviewRun{}).
		Where("id = ?", runID).Update("heartbeat_at", stale).Error; err != nil {
		t.Fatalf("age heartbeat of run %d: %v", runID, err)
	}
}

// TestClaimReviewRunReturnsOwnedState proves the claim is one operation: the
// worker receives the running row and the lease that authorizes its writes.
func TestClaimReviewRunReturnsOwnedState(t *testing.T) {
	f := newFixture(t)
	run := claimQueuedRun(t, f)

	if run.LeaseOwner == "" {
		t.Fatal("claimed run has no lease owner")
	}
	if run.Status != store.RunRunning || run.HeartbeatAt == nil {
		t.Fatalf("claimed run = %q heartbeat %v, want running with a beat", run.Status, run.HeartbeatAt)
	}

	again, claimed, err := f.s.ClaimReviewRun(f.ctx, run.ID)
	if err != nil || claimed {
		t.Fatalf("second claim = claimed %v, err %v; want a no-op", claimed, err)
	}
	if again.LeaseOwner != run.LeaseOwner {
		t.Errorf("second claim changed the owner from %q to %q", run.LeaseOwner, again.LeaseOwner)
	}
}

// TestClaimReviewRunRecoversExpiredLease is the #228 acceptance test: an
// orphaned attempt is reclaimed by the next delivery, its compatible units
// survive, and only the new owner can finish the result.
func TestClaimReviewRunRecoversExpiredLease(t *testing.T) {
	f := newFixture(t)
	crashed := claimQueuedRun(t, f)
	unit := store.ReviewUnit{
		RunID: crashed.ID, UnitKey: "main", Kind: "main",
		Result: []byte(`{"status":"complete"}`), Usage: []byte(`{"total":100}`),
	}
	if err := f.s.UpsertReviewUnit(f.ctx, unit); err != nil {
		t.Fatalf("seed unit: %v", err)
	}
	expireLease(t, f, crashed.ID)

	recovered, claimed, err := f.s.ClaimReviewRun(f.ctx, crashed.ID)
	if err != nil || !claimed {
		t.Fatalf("recovery claim = claimed %v, err %v; want the orphan recovered", claimed, err)
	}
	if recovered.LeaseOwner == crashed.LeaseOwner {
		t.Fatal("recovery kept the previous owner")
	}
	if recovered.RecoveryCount != 1 {
		t.Errorf("recovery count = %d, want 1", recovered.RecoveryCount)
	}
	if recovered.Phase != store.RunPhaseRecovering {
		t.Errorf("phase = %q, want %q while recovering", recovered.Phase, store.RunPhaseRecovering)
	}
	if recovered.ErrorMessage == "" {
		t.Error("recovery left no reason for the panel")
	}
	if units, err := f.s.ReviewUnitsForRun(f.ctx, crashed.ID); err != nil || len(units) != 1 {
		t.Fatalf("units after recovery = %d, err %v; want the compatible unit kept", len(units), err)
	}

	// The previous worker lost ownership: its heartbeat is refused and its
	// result must not be written or published.
	if err := f.s.HeartbeatReviewRun(f.ctx, crashed.ID, crashed.LeaseOwner); !errors.Is(err, store.ErrRunLeaseLost) {
		t.Fatalf("stale heartbeat = %v, want ErrRunLeaseLost", err)
	}
	if err := f.s.CompleteReviewRun(f.ctx, crashed.ID, crashed.LeaseOwner, store.RunCompletion{
		Status: store.RunFailed, ErrorCode: "stale",
	}); !errors.Is(err, store.ErrRunLeaseLost) {
		t.Fatalf("stale complete = %v, want ErrRunLeaseLost", err)
	}
	stillRunning, err := f.s.ReviewRun(f.ctx, crashed.ID)
	if err != nil {
		t.Fatalf("ReviewRun: %v", err)
	}
	if stillRunning.Status != store.RunRunning {
		t.Fatalf("stale worker changed the run to %q", stillRunning.Status)
	}
	if units, err := f.s.ReviewUnitsForRun(f.ctx, crashed.ID); err != nil || len(units) != 1 {
		t.Fatalf("stale complete dropped the units: %d, err %v", len(units), err)
	}

	// Exactly one finalization: the new owner writes it once and a second
	// complete finds the run terminal.
	if err := f.s.CompleteReviewRun(f.ctx, crashed.ID, recovered.LeaseOwner, store.RunCompletion{
		Status: store.RunSucceeded,
	}); err != nil {
		t.Fatalf("recovered complete: %v", err)
	}
	if err := f.s.CompleteReviewRun(f.ctx, crashed.ID, recovered.LeaseOwner, store.RunCompletion{
		Status: store.RunSucceeded,
	}); !errors.Is(err, store.ErrRunNotActive) {
		t.Fatalf("second complete = %v, want ErrRunNotActive", err)
	}
}

// TestConcurrentRecoveryClaimsOnce proves two workers that see the same
// expired lease cannot both recover the attempt.
func TestConcurrentRecoveryClaimsOnce(t *testing.T) {
	f := newFixture(t)
	crashed := claimQueuedRun(t, f)
	expireLease(t, f, crashed.ID)

	const workers = 4
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]bool, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			_, claimed, err := f.s.ClaimReviewRun(f.ctx, crashed.ID)
			results[index], errs[index] = claimed, err
		}(i)
	}
	close(start)
	wg.Wait()

	claimedBy := -1
	for i, claimed := range results {
		if errs[i] != nil {
			t.Fatalf("worker %d claim error = %v", i, errs[i])
		}
		if claimed {
			if claimedBy >= 0 {
				t.Fatalf("workers %d and %d both recovered the run", claimedBy, i)
			}
			claimedBy = i
		}
	}
	if claimedBy < 0 {
		t.Fatal("no worker recovered the expired run")
	}

	stored, err := f.s.ReviewRun(f.ctx, crashed.ID)
	if err != nil {
		t.Fatalf("ReviewRun: %v", err)
	}
	if stored.RecoveryCount != 1 {
		t.Errorf("recovery count = %d, want exactly one recovery", stored.RecoveryCount)
	}
}

// TestClaimReviewRunFailsAfterRecoveryExhaustion covers the retry budget: a run
// that keeps losing its workers ends in a terminal state, keeps its publication
// owed, and drops the resumable units.
func TestClaimReviewRunFailsAfterRecoveryExhaustion(t *testing.T) {
	f := newFixture(t)
	crashed := claimQueuedRun(t, f)
	if err := f.s.UpsertReviewUnit(f.ctx, store.ReviewUnit{
		RunID: crashed.ID, UnitKey: "main", Result: []byte(`{"status":"complete"}`),
	}); err != nil {
		t.Fatalf("seed unit: %v", err)
	}
	if err := f.s.DB.WithContext(f.ctx).Model(&store.ReviewRun{}).
		Where("id = ?", crashed.ID).Update("recovery_count", store.MaxRunRecoveries).Error; err != nil {
		t.Fatalf("set recovery count: %v", err)
	}
	expireLease(t, f, crashed.ID)

	exhausted, claimed, err := f.s.ClaimReviewRun(f.ctx, crashed.ID)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed {
		t.Fatal("exhausted run was recovered again")
	}
	if exhausted.Status != store.RunFailed || exhausted.ErrorCode != store.RunErrorInterrupted {
		t.Fatalf("run = %q/%q, want failed/%s", exhausted.Status, exhausted.ErrorCode, store.RunErrorInterrupted)
	}
	if !exhausted.PublishPending {
		t.Error("terminal run has no publication owed: its check would stay open")
	}
	if exhausted.FinishedAt == nil {
		t.Error("terminal run has no finished_at")
	}
	if units, err := f.s.ReviewUnitsForRun(f.ctx, crashed.ID); err != nil || len(units) != 0 {
		t.Fatalf("terminal run kept units: %d, err %v", len(units), err)
	}
}

// TestLeaseExpiredRunsSelectsRecoverableOnly separates a live worker from an
// orphan and from a run the global watchdog already owns.
func TestLeaseExpiredRunsSelectsRecoverableOnly(t *testing.T) {
	f := newFixture(t)

	instID := f.nextID()
	f.installation(instID)
	repo := f.repo(instID, instID, "acme/sweep", nil, true)
	oldStart := time.Now().UTC().Add(-2 * time.Hour)

	expired := claimQueuedRun(t, f)
	expireLease(t, f, expired.ID)

	live := claimQueuedRun(t, f)

	stale := claimQueuedRun(t, f)
	expireLease(t, f, stale.ID)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.ReviewRun{}).
		Where("id = ?", stale.ID).Update("started_at", oldStart).Error; err != nil {
		t.Fatalf("age started_at: %v", err)
	}

	pr := f.openPR(repo.ID, 99, store.StatusPending, false)
	queued := f.run(pr, store.RunQueued)

	now := time.Now().UTC()
	runs, err := f.s.LeaseExpiredRuns(f.ctx, now.Add(-store.RunLeaseTTL), now.Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("LeaseExpiredRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != expired.ID {
		ids := make([]uint, 0, len(runs))
		for _, run := range runs {
			ids = append(ids, run.ID)
		}
		t.Fatalf("lease-expired runs = %v, want only %d (live %d, stale %d, queued %d)",
			ids, expired.ID, live.ID, stale.ID, queued.ID)
	}
}
