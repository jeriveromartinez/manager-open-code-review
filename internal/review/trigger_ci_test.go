package review

import (
	"fmt"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func (f *triggerFixture) setCI(pr store.PullRequest, v store.PRValidations) {
	f.t.Helper()
	if _, err := f.st.SetPRValidations(f.ctx, pr.ID, pr.HeadSHA, v); err != nil {
		f.t.Fatalf("SetPRValidations: %v", err)
	}
}

func (f *triggerFixture) runsOf(pr store.PullRequest) []store.ReviewRun {
	f.t.Helper()
	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		f.t.Fatalf("ListReviewRuns: %v", err)
	}
	return runs
}

func gatedOpts() seedOpts {
	opts := eligibleOpts()
	opts.requireGreenCI = true
	return opts
}

func TestTriggerCIGateBlocksFailingChecks(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("gated-red", gatedOpts())
	f.setCI(pr, store.PRValidations{Passed: 2, Failed: 1, Total: 3})

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}
	if runs := f.runsOf(pr); len(runs) != 0 {
		t.Fatalf("runs = %+v, want none while CI is red", runs)
	}
	if len(f.jobs.reviews) != 0 || len(f.checks.started) != 0 {
		t.Errorf("unexpected enqueues/checks: %v / %v", f.jobs.reviews, f.checks.started)
	}
}

func TestTriggerCIGateAllowsGreenChecks(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("gated-green", gatedOpts())
	f.setCI(pr, store.PRValidations{Passed: 9, Total: 9})

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}
	runs := f.runsOf(pr)
	if len(runs) != 1 || runs[0].Status != store.RunQueued || runs[0].HeadSHA != pr.HeadSHA {
		t.Fatalf("runs = %+v, want one queued run for the head", runs)
	}

	// A redelivery of the same green head must not create a second run.
	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("second OnPullRequest() error = %v", err)
	}
	if runs := f.runsOf(pr); len(runs) != 1 {
		t.Fatalf("runs = %+v, want still one run", runs)
	}
}

// A head with no checks, or with checks still running, cannot be green: the
// gate waits for the later delivery that completes them.
func TestTriggerCIGateWaitsThenReleases(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("gated-pending", gatedOpts())

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() without checks error = %v", err)
	}
	if runs := f.runsOf(pr); len(runs) != 0 {
		t.Fatalf("runs = %+v, want none before any check exists", runs)
	}

	f.setCI(pr, store.PRValidations{Passed: 1, Failed: 0, Total: 3})
	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() with running checks error = %v", err)
	}
	if runs := f.runsOf(pr); len(runs) != 0 {
		t.Fatalf("runs = %+v, want none while checks are running", runs)
	}

	f.setCI(pr, store.PRValidations{Passed: 3, Total: 3})
	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() with green checks error = %v", err)
	}
	if runs := f.runsOf(pr); len(runs) != 1 {
		t.Fatalf("runs = %+v, want one run once CI turned green", runs)
	}
}

func TestTriggerCIGateOffKeepsBehaviour(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("gate-off", eligibleOpts())
	f.setCI(pr, store.PRValidations{Passed: 0, Failed: 2, Total: 2})

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}
	if runs := f.runsOf(pr); len(runs) != 1 {
		t.Fatalf("runs = %+v, want one run with the gate disabled", runs)
	}
}

// Counters of a previous head never open the gate of the current one.
func TestTriggerCIGateIgnoresPreviousHeadCounters(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("gated-stale", gatedOpts())
	f.setCI(pr, store.PRValidations{Passed: 5, Total: 5})

	if _, _, err := f.st.SyncPullRequest(f.ctx, store.PullRequest{
		RepositoryID:    pr.RepositoryID,
		Number:          pr.Number,
		Title:           pr.Title,
		HeadSHA:         "brand-new-sha",
		State:           "open",
		GitHubUpdatedAt: pr.GitHubUpdatedAt.Add(1),
	}); err != nil {
		t.Fatalf("SyncPullRequest(new head): %v", err)
	}

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}
	if runs := f.runsOf(pr); len(runs) != 0 {
		t.Fatalf("runs = %+v, want none for a head without its own checks", runs)
	}
}

// Reconciliation goes through the same gate, so polling mode matches webhooks.
func TestEnqueuePendingHonoursCIGate(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("polling", gatedOpts())
	f.setCI(pr, store.PRValidations{Passed: 1, Failed: 1, Total: 2})

	created, err := f.trigger.EnqueuePending(f.ctx, 50)
	if err != nil {
		t.Fatalf("EnqueuePending() error = %v", err)
	}
	if created != 0 || len(f.runsOf(pr)) != 0 {
		t.Fatalf("created = %d, runs = %+v; want none while CI is red", created, f.runsOf(pr))
	}

	f.setCI(pr, store.PRValidations{Passed: 2, Total: 2})
	created, err = f.trigger.EnqueuePending(f.ctx, 50)
	if err != nil {
		t.Fatalf("EnqueuePending() after green error = %v", err)
	}
	if created != 1 {
		t.Errorf("created = %d, want 1", created)
	}
	if runs := f.runsOf(pr); len(runs) != 1 {
		t.Errorf("runs = %+v, want one", runs)
	}
}

// A full batch of recent PRs still waiting for CI must not consume the limit:
// an older eligible candidate must still be scheduled.
func TestEnqueuePendingDoesNotLetCIWaitersConsumeBatch(t *testing.T) {
	f := newTriggerFixture(t)
	eligible := f.seed("eligible", eligibleOpts())

	// Age the eligible PR so the CI waiters would win the newest-first order.
	old := time.Now().UTC().Add(-time.Hour)
	if err := f.st.DB.WithContext(f.ctx).Exec(
		`UPDATE pull_requests SET updated_at = ? WHERE id = ?`, old, eligible.ID,
	).Error; err != nil {
		t.Fatalf("age eligible pull request: %v", err)
	}
	for i := range 3 {
		pr := f.seed(fmt.Sprintf("ci-pending-%d", i), gatedOpts())
		f.setCI(pr, store.PRValidations{Passed: 1, Failed: 0, Total: 3})
	}

	created, err := f.trigger.EnqueuePending(f.ctx, 1)
	if err != nil {
		t.Fatalf("EnqueuePending() error = %v", err)
	}
	if created != 1 {
		t.Fatalf("created = %d, want the eligible PR scheduled despite the full CI-waiting batch", created)
	}
	if runs := f.runsOf(eligible); len(runs) != 1 {
		t.Errorf("eligible runs = %+v, want one", runs)
	}
}
