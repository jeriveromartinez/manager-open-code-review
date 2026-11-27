package review

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestParseCriterionAssessmentsReadsTheContract(t *testing.T) {
	comments := []nativeComment{
		{Content: "QA-CRITERION abc123 CUMPLIDO\nEVIDENCE: internal/auth.go:10-20; internal/auth_test.go:1-5 [test]\nOBSERVED: rejects insecure sessions\nREQUIRED: must reject insecure sessions"},
		{Content: "QA-CRITERION def456 NO_APLICABLE\nEVIDENCE: docs/readme.md\nREASON: not part of this change\nREQUIRED: documentation"},
	}
	answers := ParseCriterionAssessments(comments)
	if len(answers) != 2 {
		t.Fatalf("answers = %+v, want 2", answers)
	}
	first := answers["abc123"]
	if first.Status != store.CriterionSatisfied || first.Observed != "rejects insecure sessions" {
		t.Errorf("first answer = %+v", first)
	}
	if len(first.Evidence) != 2 || !first.Evidence[1].Test || first.Evidence[0].StartLine != 10 || first.Evidence[0].EndLine != 20 {
		t.Errorf("evidence = %+v", first.Evidence)
	}
	if second := answers["def456"]; second.Status != store.CriterionNotApplicable || second.Reason == "" {
		t.Errorf("second answer = %+v", second)
	}
}

func TestParseCriterionAssessmentsIgnoresMalformedAnswers(t *testing.T) {
	comments := []nativeComment{
		{Content: "QA-VERIFY abc CONFIRM\nEVIDENCE: x"},
		{Content: "QA-CRITERION abc DESCONOCIDO\nEVIDENCE: x"},
		{Content: "QA-CRITERION abc INCUMPLIDO\nEVIDENCE: internal/a.go:1"},
		{Content: "QA-CRITERION abc CUMPLIDO\nEVIDENCE: internal/b.go:1"},
	}
	answers := ParseCriterionAssessments(comments)
	if len(answers) != 1 {
		t.Fatalf("answers = %+v, want only the first usable answer", answers)
	}
	if got := answers["abc"].Status; got != store.CriterionUnsatisfied {
		t.Errorf("first answer did not win: %q", got)
	}
}

func TestParseCriterionEvidenceRejectsUnsafeLocations(t *testing.T) {
	evidence := parseCriterionEvidence("/etc/passwd:1; ../outside.go:1; internal/ok.go:3-4")
	if len(evidence) != 1 || evidence[0].Path != "internal/ok.go" {
		t.Errorf("evidence = %+v, want only the checkout-relative path", evidence)
	}
}

func TestCriteriaRuleJSONIsAProjectRule(t *testing.T) {
	var rule struct {
		Rules []struct {
			Path string `json:"path"`
			Rule string `json:"rule"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(criteriaRuleJSON(), &rule); err != nil {
		t.Fatalf("decode rule: %v", err)
	}
	if len(rule.Rules) != 1 || rule.Rules[0].Path == "" {
		t.Fatalf("rule = %+v", rule)
	}
	for _, want := range []string{"QA-CRITERION", "CUMPLIDO", "NO_VERIFICABLE", "EVIDENCE:"} {
		if !strings.Contains(rule.Rules[0].Rule, want) {
			t.Errorf("rule misses %q", want)
		}
	}
}
