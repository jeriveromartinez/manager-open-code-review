package publish

import (
	"strings"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestRenderCriteriaBlockShowsStatusSourceEvidenceAndUncertainty(t *testing.T) {
	assessments := []store.CriterionAssessment{
		{
			CriterionID:   "abc",
			Text:          "auth rejects insecure sessions",
			Status:        store.CriterionSatisfied,
			SourceKind:    "issue",
			SourceRepo:    "acme/repo",
			SourceNumber:  7,
			SourceURL:     "https://github.com/acme/repo/issues/7",
			SourceVersion: "v1",
			Evidence: store.EncodeCriterionEvidence([]store.CriterionEvidence{
				{Path: "internal/auth.go", StartLine: 10, EndLine: 20},
				{Path: "internal/auth_test.go", StartLine: 1, EndLine: 5, Test: true},
			}),
			Observed: "rejects insecure sessions",
			Required: "must reject insecure sessions",
		},
		{
			CriterionID:  "def",
			Text:         "logout clears the token",
			Status:       store.CriterionUnverifiable,
			SourceKind:   "issue",
			SourceRepo:   "acme/repo",
			SourceNumber: 7,
			Uncertainty:  "requisitos contradictorios: abc",
			Reason:       "the issue contradicts itself",
		},
	}
	block := renderCriteriaBlock(assessments, func(value string) string { return value })

	for _, want := range []string{
		"Cumplimiento de criterios",
		"cumplido",
		"auth rejects insecure sessions",
		"[acme/repo#7](https://github.com/acme/repo/issues/7)",
		"versión v1",
		"internal/auth.go:10-20",
		"internal/auth_test.go:1-5",
		"(prueba)",
		"Observado: rejects insecure sessions",
		"Exigido: must reject insecure sessions",
		"no verificable",
		"Incertidumbre: requisitos contradictorios: abc",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block misses %q:\n%s", want, block)
		}
	}
}

func TestRenderCriteriaBlockBoundsModelProse(t *testing.T) {
	block := renderCriteriaBlock([]store.CriterionAssessment{{
		Status:   store.CriterionSatisfied,
		Text:     strings.Repeat("x", criteriaFieldBytes+50),
		Observed: strings.Repeat("y", criteriaFieldBytes+50),
	}}, func(value string) string { return value })
	if strings.Count(block, strings.Repeat("x", criteriaFieldBytes+1)) > 0 {
		t.Error("criterion text was not bounded")
	}
	if !strings.Contains(block, "…") {
		t.Error("bounded fields do not show the truncation marker")
	}
}

func TestRenderCriteriaBlockSanitizesEveryValue(t *testing.T) {
	block := renderCriteriaBlock([]store.CriterionAssessment{{
		Status:       store.CriterionUnverifiable,
		Text:         "criterion",
		SourceKind:   "issue",
		SourceRepo:   "acme/repo",
		SourceNumber: 7,
		Evidence:     store.EncodeCriterionEvidence([]store.CriterionEvidence{{Path: "internal/a.go"}}),
		Uncertainty:  "source",
	}}, func(value string) string { return "<" + value + ">" })
	if strings.Contains(block, "internal/a.go") && !strings.Contains(block, "<internal/a.go>") {
		t.Errorf("evidence path was not sanitized:\n%s", block)
	}
	if strings.Contains(block, "criterion\n") && !strings.Contains(block, "<criterion>") {
		t.Errorf("criterion text was not sanitized:\n%s", block)
	}
}
