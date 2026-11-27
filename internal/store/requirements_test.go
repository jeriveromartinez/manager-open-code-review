package store_test

import (
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func criterionTestRun(t *testing.T, f *fixture, prID uint) store.ReviewRun {
	t.Helper()
	run := store.ReviewRun{PullRequestID: prID, HeadSHA: "head-1", Status: store.RunSucceeded}
	if err := f.s.DB.WithContext(f.ctx).Create(&run).Error; err != nil {
		t.Fatalf("create review run: %v", err)
	}
	return run
}

func criterionRow(prID, runID uint, id, status string, evidence []store.CriterionEvidence) store.CriterionAssessment {
	return store.CriterionAssessment{
		PullRequestID: prID,
		ReviewRunID:   runID,
		HeadSHA:       "head-1",
		BaseRef:       "main",
		CriterionID:   id,
		Fingerprint:   "fp-" + id,
		SourceKind:    "issue",
		SourceRepo:    "acme/repo",
		SourceNumber:  7,
		SourceURL:     "https://github.com/acme/repo/issues/7",
		SourceVersion: "v1",
		Text:          "criterion " + id,
		Status:        status,
		Evidence:      store.EncodeCriterionEvidence(evidence),
		Observed:      "observed",
		Required:      "required",
	}
}

func TestSaveCriterionAssessmentsReplacesTheCurrentSet(t *testing.T) {
	f := newFixture(t)
	instID := f.nextID()
	f.installation(instID)
	repo := f.repo(instID, instID, "acme/criteria", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusReadyForHuman, false)
	run := criterionTestRun(t, f, pr.ID)

	first := []store.CriterionAssessment{
		criterionRow(pr.ID, run.ID, "a", store.CriterionSatisfied,
			[]store.CriterionEvidence{{Path: "internal/auth.go", StartLine: 1, EndLine: 2, Digest: "d"}}),
		criterionRow(pr.ID, run.ID, "b", store.CriterionUnverifiable, nil),
	}
	if err := f.s.SaveCriterionAssessments(f.ctx, first); err != nil {
		t.Fatalf("SaveCriterionAssessments: %v", err)
	}
	current, err := f.s.CurrentCriterionAssessments(f.ctx, pr.ID, "head-1")
	if err != nil {
		t.Fatalf("CurrentCriterionAssessments: %v", err)
	}
	if len(current) != 2 {
		t.Fatalf("current = %+v, want 2", current)
	}

	second := []store.CriterionAssessment{
		criterionRow(pr.ID, run.ID, "a", store.CriterionUnsatisfied,
			[]store.CriterionEvidence{{Path: "internal/auth.go", StartLine: 1, EndLine: 2, Digest: "d2"}}),
	}
	if err := f.s.SaveCriterionAssessments(f.ctx, second); err != nil {
		t.Fatalf("SaveCriterionAssessments(replace): %v", err)
	}
	current, err = f.s.CurrentCriterionAssessments(f.ctx, pr.ID, "head-1")
	if err != nil {
		t.Fatalf("CurrentCriterionAssessments: %v", err)
	}
	if len(current) != 1 || current[0].CriterionID != "a" || current[0].Status != store.CriterionUnsatisfied {
		t.Fatalf("current after replacement = %+v", current)
	}
	if evidence := store.DecodeCriterionEvidence(current[0].Evidence); len(evidence) != 1 || evidence[0].Digest != "d2" {
		t.Errorf("evidence = %+v", evidence)
	}

	if other, err := f.s.CurrentCriterionAssessments(f.ctx, pr.ID, "head-2"); err != nil || len(other) != 0 {
		t.Errorf("other head returned %+v (err %v), want none", other, err)
	}

	var invalidated int64
	if err := f.s.DB.WithContext(f.ctx).Model(&store.CriterionAssessment{}).
		Where("pull_request_id = ? AND invalidated_at IS NOT NULL", pr.ID).
		Count(&invalidated).Error; err != nil {
		t.Fatalf("count invalidated: %v", err)
	}
	if invalidated != 2 {
		t.Errorf("invalidated = %d, want the 2 replaced rows", invalidated)
	}
}

func TestInvalidateCriterionAssessmentsHidesTheCurrentSet(t *testing.T) {
	f := newFixture(t)
	instID := f.nextID()
	f.installation(instID)
	repo := f.repo(instID, instID, "acme/criteria-invalidate", nil, true)
	pr := f.openPR(repo.ID, 2, store.StatusReadyForHuman, false)
	run := criterionTestRun(t, f, pr.ID)
	if err := f.s.SaveCriterionAssessments(f.ctx, []store.CriterionAssessment{
		criterionRow(pr.ID, run.ID, "a", store.CriterionSatisfied, nil),
	}); err != nil {
		t.Fatalf("SaveCriterionAssessments: %v", err)
	}
	if err := f.s.InvalidateCriterionAssessments(f.ctx, pr.ID, "comparison_moved"); err != nil {
		t.Fatalf("InvalidateCriterionAssessments: %v", err)
	}
	current, err := f.s.CurrentCriterionAssessments(f.ctx, pr.ID, "")
	if err != nil {
		t.Fatalf("CurrentCriterionAssessments: %v", err)
	}
	if len(current) != 0 {
		t.Errorf("current = %+v, want none", current)
	}
}

func TestSaveCriterionAssessmentsRejectsInvalidStatus(t *testing.T) {
	f := newFixture(t)
	instID := f.nextID()
	f.installation(instID)
	repo := f.repo(instID, instID, "acme/criteria-invalid", nil, true)
	pr := f.openPR(repo.ID, 3, store.StatusReadyForHuman, false)
	run := criterionTestRun(t, f, pr.ID)
	row := criterionRow(pr.ID, run.ID, "a", "aprobado", nil)
	if err := f.s.SaveCriterionAssessments(f.ctx, []store.CriterionAssessment{row}); err == nil {
		t.Fatal("SaveCriterionAssessments accepted an unknown status")
	}
}
