package store_test

import (
	"errors"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestReviewUnitsUpsertReplaceAndDelete(t *testing.T) {
	f := newFixture(t)
	instID := f.nextID()
	f.installation(instID)
	repo := f.repo(instID, instID, "acme/app", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)
	run := f.run(pr, store.RunRunning)

	units := []store.ReviewUnit{
		{RunID: run.ID, UnitKey: "main", Kind: "main", Result: []byte(`{"status":"complete"}`), Usage: []byte(`{"total":100}`)},
		{RunID: run.ID, UnitKey: "chunk-1", Kind: "chunk", Path: "big.go", Result: []byte(`{"status":"complete"}`), Usage: []byte(`{"total":50}`)},
		{RunID: run.ID, UnitKey: "chunk-2", Kind: "chunk", Path: "big.go", Result: []byte(`{"status":"complete"}`), Usage: []byte(`{"total":50}`)},
	}
	for _, unit := range units {
		if err := f.s.UpsertReviewUnit(f.ctx, unit); err != nil {
			t.Fatalf("UpsertReviewUnit(%s): %v", unit.UnitKey, err)
		}
	}

	got, err := f.s.ReviewUnitsForRun(f.ctx, run.ID)
	if err != nil {
		t.Fatalf("ReviewUnitsForRun: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("units = %d, want 3", len(got))
	}
	if got[0].Status != store.ReviewUnitCompleted {
		t.Errorf("status = %q, want completed", got[0].Status)
	}

	// The same key replaces the row instead of duplicating it.
	replacement := units[0]
	replacement.Usage = []byte(`{"total":999}`)
	if err := f.s.UpsertReviewUnit(f.ctx, replacement); err != nil {
		t.Fatalf("replace unit: %v", err)
	}
	got, err = f.s.ReviewUnitsForRun(f.ctx, run.ID)
	if err != nil {
		t.Fatalf("ReviewUnitsForRun after replace: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("units after replace = %d, want 3", len(got))
	}
	if string(got[0].Usage) != `{"total": 999}` {
		t.Errorf("replaced usage = %s, want total 999", got[0].Usage)
	}

	// A terminal run drops its resumable state.
	if err := f.s.CompleteReviewRun(f.ctx, run.ID, "", store.RunCompletion{Status: store.RunSucceeded}); err != nil {
		t.Fatalf("CompleteReviewRun: %v", err)
	}
	got, err = f.s.ReviewUnitsForRun(f.ctx, run.ID)
	if err != nil {
		t.Fatalf("ReviewUnitsForRun after complete: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("units after complete = %d, want 0", len(got))
	}
}

func TestSupersedeDeletesReviewUnits(t *testing.T) {
	f := newFixture(t)
	instID := f.nextID()
	f.installation(instID)
	repo := f.repo(instID, instID, "acme/app", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)
	run := f.run(pr, store.RunQueued)
	if err := f.s.UpsertReviewUnit(f.ctx, store.ReviewUnit{
		RunID: run.ID, UnitKey: "main", Result: []byte(`{"status":"complete"}`),
	}); err != nil {
		t.Fatalf("seed unit: %v", err)
	}

	if err := f.s.SupersedeActiveRuns(f.ctx, pr.ID); err != nil {
		t.Fatalf("SupersedeActiveRuns: %v", err)
	}
	assertNoReviewUnits(t, f, run.ID)

	other := f.run(pr, store.RunQueued)
	if err := f.s.UpsertReviewUnit(f.ctx, store.ReviewUnit{
		RunID: other.ID, UnitKey: "main", Result: []byte(`{"status":"complete"}`),
	}); err != nil {
		t.Fatalf("seed unit: %v", err)
	}
	comparison := store.Comparison{HeadSHA: "another-head", BaseRef: "main"}
	if err := f.s.SupersedeOtherActiveRuns(f.ctx, pr.ID, comparison); err != nil {
		t.Fatalf("SupersedeOtherActiveRuns: %v", err)
	}
	assertNoReviewUnits(t, f, other.ID)
}

func assertNoReviewUnits(t *testing.T, f *fixture, runID uint) {
	t.Helper()
	units, err := f.s.ReviewUnitsForRun(f.ctx, runID)
	if err != nil {
		t.Fatalf("ReviewUnitsForRun(%d): %v", runID, err)
	}
	if len(units) != 0 {
		t.Errorf("run %d kept %d review units after supersession", runID, len(units))
	}
}

func TestClaimPreservesAggregateBudgetAndOCRSession(t *testing.T) {
	f := newFixture(t)
	instID := f.nextID()
	f.installation(instID)
	repo := f.repo(instID, instID, "acme/app", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)
	run := f.run(pr, store.RunQueued)

	claimed, ok, err := f.s.ClaimReviewRun(f.ctx, run.ID)
	if err != nil || !ok {
		t.Fatalf("ClaimReviewRun = %v/%v", ok, err)
	}
	if claimed.BudgetStartedAt == nil {
		t.Fatal("budget_started_at not set on the first claim")
	}
	first := *claimed.BudgetStartedAt

	if err := f.s.SetReviewRunSession(f.ctx, run.ID, "", "session-1", "scope-1"); err != nil {
		t.Fatalf("SetReviewRunSession: %v", err)
	}
	if err := f.s.RequeueReviewRun(f.ctx, run.ID, "", "interrupted", "worker stopped"); err != nil {
		t.Fatalf("RequeueReviewRun: %v", err)
	}

	reclaimed, ok, err := f.s.ClaimReviewRun(f.ctx, run.ID)
	if err != nil || !ok {
		t.Fatalf("second ClaimReviewRun = %v/%v", ok, err)
	}
	if reclaimed.BudgetStartedAt == nil || !reclaimed.BudgetStartedAt.Equal(first) {
		t.Errorf("budget_started_at = %v, want preserved %v", reclaimed.BudgetStartedAt, first)
	}
	if reclaimed.Attempt != 2 {
		t.Errorf("attempt = %d, want 2", reclaimed.Attempt)
	}
	if reclaimed.OCRSessionID != "session-1" || reclaimed.OCRSessionScope != "scope-1" {
		t.Errorf("OCR session = %q/%q, want it preserved across the requeue",
			reclaimed.OCRSessionID, reclaimed.OCRSessionScope)
	}

	// A queued run must not accept a session write: it may already belong to
	// another worker's attempt.
	if err := f.s.RequeueReviewRun(f.ctx, run.ID, "", "interrupted", "worker stopped"); err != nil {
		t.Fatalf("RequeueReviewRun: %v", err)
	}
	if err := f.s.SetReviewRunSession(f.ctx, run.ID, "", "session-2", "scope-2"); !errors.Is(err, store.ErrRunNotActive) {
		t.Errorf("SetReviewRunSession on queued run = %v, want ErrRunNotActive", err)
	}
}
