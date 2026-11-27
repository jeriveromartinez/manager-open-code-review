package api

import (
	"encoding/json"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestCriterionDTOOfExposesSourceEvidenceAndUncertainty(t *testing.T) {
	assessment := store.CriterionAssessment{
		CriterionID:   "abc",
		Text:          "auth rejects insecure sessions",
		Status:        store.CriterionUnverifiable,
		SourceKind:    "issue",
		SourceRepo:    "acme/repo",
		SourceNumber:  7,
		SourceURL:     "https://github.com/acme/repo/issues/7",
		SourceTitle:   "Add auth",
		SourceVersion: "v1",
		Evidence: store.EncodeCriterionEvidence([]store.CriterionEvidence{
			{Path: "internal/auth.go", StartLine: 3, EndLine: 4, Test: false},
			{Path: "internal/auth_test.go", StartLine: 1, EndLine: 2, Test: true},
		}),
		Observed:    "accepts insecure sessions",
		Required:    "must reject insecure sessions",
		Reason:      "evaluator ran out of context",
		Uncertainty: "requisitos contradictorios",
		HeadSHA:     "head-1",
	}
	dto := criterionDTOOf(assessment)
	encoded, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal dto: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode dto: %v", err)
	}
	for _, field := range []string{"id", "text", "status", "source", "link", "evidence", "observed", "required", "uncertainty", "head_sha"} {
		if _, ok := decoded[field]; !ok {
			t.Errorf("dto misses %q: %s", field, encoded)
		}
	}
	evidence, ok := decoded["evidence"].([]any)
	if !ok || len(evidence) != 2 {
		t.Fatalf("evidence = %v", decoded["evidence"])
	}
	second := evidence[1].(map[string]any)
	if second["test"] != true {
		t.Errorf("test evidence flag was lost: %v", second)
	}
}
