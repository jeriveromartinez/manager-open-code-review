package review

import (
	"context"
	"fmt"
	"sync"
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

func (f *triggerFixture) setRepoPolicy(pr store.PullRequest, policy store.RepositoryPolicy) store.RepositoryPolicy {
	f.t.Helper()

	policy.RepositoryID = pr.RepositoryID
	saved, err := f.st.SaveRepositoryPolicy(f.ctx, policy)
	if err != nil {
		f.t.Fatalf("SaveRepositoryPolicy: %v", err)
	}
	return saved
}

func TestTriggerEnforcesRepositoryCIGateOverride(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("repo-gated", eligibleOpts())
	f.setCI(pr, store.PRValidations{Passed: 0, Failed: 1, Total: 1})
	requiredGreen := true
	f.setRepoPolicy(pr, store.RepositoryPolicy{RequireGreenCI: &requiredGreen})

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}
	if runs := f.runsOf(pr); len(runs) != 0 {
		t.Fatalf("runs = %+v, want none while the repository requires green CI", runs)
	}

	f.setCI(pr, store.PRValidations{Passed: 1, Total: 1})
	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() after green error = %v", err)
	}
	runs := f.runsOf(pr)
	if len(runs) != 1 || runs[0].Status != store.RunQueued {
		t.Fatalf("runs = %+v, want one queued run once the override is green", runs)
	}
	if runs[0].PolicyObservation || runs[0].PolicyRevision == "" {
		t.Errorf("run observation/revision = %v/%q, want enforced with a recorded revision",
			runs[0].PolicyObservation, runs[0].PolicyRevision)
	}
}

func TestTriggerObservationModeChangesNothing(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("repo-observe", eligibleOpts())
	f.setCI(pr, store.PRValidations{Passed: 0, Failed: 1, Total: 1})
	requiredGreen := true
	f.setRepoPolicy(pr, store.RepositoryPolicy{
		ObservationMode: true, RequireGreenCI: &requiredGreen,
	})

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}
	runs := f.runsOf(pr)
	if len(runs) != 1 {
		t.Fatalf("runs = %+v, want the review to proceed under the team policy", runs)
	}
	if !runs[0].PolicyObservation {
		t.Error("PolicyObservation = false, want true for an observed override")
	}
	if runs[0].PolicyRevision == "" {
		t.Error("PolicyRevision is empty, want the observed revision recorded with the run")
	}
}

func TestEnqueuePendingHonoursRepositoryCIGateOverride(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("polling-override", eligibleOpts())
	f.setCI(pr, store.PRValidations{Passed: 0, Failed: 1, Total: 1})
	requiredGreen := true
	f.setRepoPolicy(pr, store.RepositoryPolicy{RequireGreenCI: &requiredGreen})

	created, err := f.trigger.EnqueuePending(f.ctx, 50)
	if err != nil {
		t.Fatalf("EnqueuePending() error = %v", err)
	}
	if created != 0 || len(f.runsOf(pr)) != 0 {
		t.Fatalf("created = %d, runs = %+v; want none while the repository override is red",
			created, f.runsOf(pr))
	}

	f.setCI(pr, store.PRValidations{Passed: 1, Total: 1})
	created, err = f.trigger.EnqueuePending(f.ctx, 50)
	if err != nil {
		t.Fatalf("EnqueuePending() after green error = %v", err)
	}
	if created != 1 {
		t.Fatalf("created = %d, want one run once the override is green", created)
	}
}

func TestEnqueuePendingObservationModeIgnoresOverride(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("polling-observe", eligibleOpts())
	f.setCI(pr, store.PRValidations{Passed: 0, Failed: 1, Total: 1})
	requiredGreen := true
	f.setRepoPolicy(pr, store.RepositoryPolicy{
		ObservationMode: true, RequireGreenCI: &requiredGreen,
	})

	created, err := f.trigger.EnqueuePending(f.ctx, 50)
	if err != nil {
		t.Fatalf("EnqueuePending() error = %v", err)
	}
	if created != 1 {
		t.Fatalf("created = %d, want one run because the override is only observed", created)
	}
	if runs := f.runsOf(pr); len(runs) != 1 || !runs[0].PolicyObservation {
		t.Fatalf("runs = %+v, want one observation run", runs)
	}
}

func TestEnqueuePendingHonoursRepositoryRequiredChecks(t *testing.T) {
	f := newTriggerFixture(t)
	checks, err := store.NormalizeRequiredChecks([]store.RequiredCheck{{Name: "build", Origin: "app:7"}})
	if err != nil {
		t.Fatalf("NormalizeRequiredChecks: %v", err)
	}

	// The team gate is on; the repository narrows the set to the named build,
	// so another red check must not close it (#143 + #153).
	listed := f.seed("repo-checks", gatedOpts())
	f.setRepoPolicy(listed, store.RepositoryPolicy{RequiredChecks: checks})
	f.setObservation(listed,
		store.CheckObservation{Name: "build", Origin: "app:7", State: store.CheckPassed},
		store.CheckObservation{Name: "other", Origin: "app:8", State: store.CheckFailed},
	)

	blocked := f.seed("repo-checks-pending", gatedOpts())
	f.setRepoPolicy(blocked, store.RepositoryPolicy{RequiredChecks: checks})
	f.setObservation(blocked,
		store.CheckObservation{Name: "build", Origin: "app:7", State: store.CheckPending},
		store.CheckObservation{Name: "other", Origin: "app:8", State: store.CheckPassed},
	)

	created, err := f.trigger.EnqueuePending(f.ctx, 50)
	if err != nil {
		t.Fatalf("EnqueuePending() error = %v", err)
	}
	if created != 1 {
		t.Fatalf("created = %d, want only the PR whose required check is green", created)
	}
	if runs := f.runsOf(listed); len(runs) != 1 {
		t.Errorf("listed runs = %+v, want one", runs)
	}
	if runs := f.runsOf(blocked); len(runs) != 0 {
		t.Errorf("pending runs = %+v, want none while the required check is pending", runs)
	}
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

// triggerTestRepublish records the verdicts the trigger asks to recompute.
type triggerTestRepublish struct {
	mu  sync.Mutex
	ids []uint
	err error
}

func (r *triggerTestRepublish) Republish(_ context.Context, prID uint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, prID)
	return r.err
}

func (r *triggerTestRepublish) snapshot() []uint {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uint(nil), r.ids...)
}

func (f *triggerFixture) setRequiredChecks(pr store.PullRequest, checks ...store.RequiredCheck) {
	f.t.Helper()
	var repo store.Repository
	if err := f.st.DB.WithContext(f.ctx).Take(&repo, pr.RepositoryID).Error; err != nil {
		f.t.Fatalf("load repository: %v", err)
	}
	policy, err := f.st.QAPolicyForTeam(f.ctx, *repo.TeamID)
	if err != nil {
		f.t.Fatalf("load policy: %v", err)
	}
	encoded, err := store.NormalizeRequiredChecks(checks)
	if err != nil {
		f.t.Fatalf("normalize required checks: %v", err)
	}
	policy.RequiredChecks = encoded
	if _, err := f.st.SaveQAPolicy(f.ctx, policy); err != nil {
		f.t.Fatalf("save policy: %v", err)
	}
}

func (f *triggerFixture) setObservation(pr store.PullRequest, checks ...store.CheckObservation) {
	f.t.Helper()
	observation := store.ValidationObservation{Result: store.ObservationOK, Checks: checks}
	counters := store.PRValidations{}
	for _, check := range checks {
		counters.Total++
		switch check.State {
		case store.CheckPassed:
			counters.Passed++
		case store.CheckFailed:
			counters.Failed++
		}
	}
	if _, err := f.st.SetPRValidationObservation(f.ctx, pr.ID, pr.HeadSHA, counters, observation); err != nil {
		f.t.Fatalf("SetPRValidationObservation: %v", err)
	}
}

func TestTriggerCIGateSelectsRequiredCheckByNameAndOrigin(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("gated-required", gatedOpts())
	f.setRequiredChecks(pr, store.RequiredCheck{Name: "build", Origin: "app:7"})

	f.setObservation(pr,
		store.CheckObservation{Name: "build", Origin: "app:8", State: store.CheckPassed},
		store.CheckObservation{Name: "other", Origin: "app:7", State: store.CheckFailed},
	)
	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}
	if runs := f.runsOf(pr); len(runs) != 0 {
		t.Fatalf("runs = %+v, want none for a required check from another origin", runs)
	}

	f.setObservation(pr,
		store.CheckObservation{Name: "build", Origin: "app:7", State: store.CheckPending},
	)
	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}
	if runs := f.runsOf(pr); len(runs) != 0 {
		t.Fatalf("runs = %+v, want none while the required check is pending", runs)
	}

	f.setObservation(pr,
		store.CheckObservation{Name: "build", Origin: "app:7", State: store.CheckPassed},
		store.CheckObservation{Name: "optional", Origin: "app:8", State: store.CheckFailed},
	)
	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}
	if runs := f.runsOf(pr); len(runs) != 1 {
		t.Fatalf("runs = %+v, want one once the required check passed", runs)
	}
}

func TestTriggerCILookupFailureNeverOpensTheGate(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("gated-lookup", gatedOpts())
	if _, err := f.st.SetPRValidationLookupFailure(f.ctx, pr.ID, pr.HeadSHA); err != nil {
		t.Fatalf("SetPRValidationLookupFailure: %v", err)
	}

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}
	if runs := f.runsOf(pr); len(runs) != 0 {
		t.Fatalf("runs = %+v, want none after a failed lookup", runs)
	}
	if len(f.checks.started) != 0 || len(f.jobs.reviews) != 0 {
		t.Errorf("unexpected enqueues/checks: %v / %v", f.jobs.reviews, f.checks.started)
	}
}

// A check delivery must recompute the published verdict even when the current
// review is reused, so red -> green -> red converges without a second analysis.
func TestTriggerChecksUpdatedConvergesWithoutNewRun(t *testing.T) {
	f := newTriggerFixture(t)
	republish := &triggerTestRepublish{}
	f.trigger.Republish = republish
	pr := f.seed("gated-converge", gatedOpts())
	f.setObservation(pr, store.CheckObservation{Name: "build", Origin: "app:7", State: store.CheckPassed})

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}
	runs := f.runsOf(pr)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if err := f.st.DB.WithContext(f.ctx).Model(&store.ReviewRun{}).Where("id = ?", runs[0].ID).
		Update("status", store.RunSucceeded).Error; err != nil {
		t.Fatalf("finish run: %v", err)
	}

	for _, state := range []string{store.CheckFailed, store.CheckPassed, store.CheckFailed} {
		f.setObservation(pr, store.CheckObservation{Name: "build", Origin: "app:7", State: state})
		if err := f.trigger.OnChecksUpdated(f.ctx, pr.ID); err != nil {
			t.Fatalf("OnChecksUpdated(%s) error = %v", state, err)
		}
	}

	if got := len(f.runsOf(pr)); got != 1 {
		t.Errorf("runs = %d, want the finished analysis reused", got)
	}
	if got := republish.snapshot(); len(got) != 3 {
		t.Errorf("Republish calls = %v, want one per check delivery", got)
	}
}
