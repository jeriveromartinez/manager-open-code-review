package publish

import (
	"strings"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestBuildRuleBlockShowsInfluencesAndKeepsSevereVisible(t *testing.T) {
	context := &publishContext{
		redactor: NewRedactor(),
		findingSnapshot: []store.Finding{
			{
				ID: "f-1", Path: "internal/review/a.go", StartLine: 10, EndLine: 12,
				Severity: "minor", Title: "nil pointer on optional field", Status: store.FindingWithdrawn,
				RuleInfluences: store.EncodeRuleInfluences([]store.RuleInfluence{{
					RuleID: 7, RuleKey: "key", Version: 2, Status: store.RuleStatusActive,
					Decision: store.RuleDecisionWithdraw, Applied: true,
					Reason: "regla v2: el valor está validado antes de esta rama",
				}}),
			},
			{
				ID: "f-2", Path: "internal/review/b.go", StartLine: 20, EndLine: 22,
				Severity: "major", Title: "nil pointer on optional field", Status: store.FindingOpen,
				RuleInfluences: store.EncodeRuleInfluences([]store.RuleInfluence{{
					RuleID: 8, RuleKey: "key-2", Version: 1, Status: store.RuleStatusActive,
					Decision: store.RuleDecisionWithdraw, Applied: false,
					Reason: "hallazgo grave: se conserva hasta que se revise la evidencia de la regla",
				}}),
			},
		},
	}

	block := context.buildRuleBlock()
	for _, want := range []string{
		"Reglas de revisión", "1 aplicada(s)", "Regla #7 v2", "activa", "retirar",
		"Regla #8 v1", "hallazgo grave", "se conserva visible",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block does not contain %q:\n%s", want, block)
		}
	}
}

func TestBuildRuleBlockEmptyWithoutInfluences(t *testing.T) {
	context := &publishContext{
		redactor:        NewRedactor(),
		findingSnapshot: []store.Finding{{ID: "f-1", Severity: "minor", Title: "plain finding"}},
	}
	if block := context.buildRuleBlock(); block != "" {
		t.Errorf("block = %q, want empty", block)
	}
}
