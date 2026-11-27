package publish

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func differentialVerification(t *testing.T, head string, differential map[string]any) store.Finding {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"verdict": "confirm", "evidence": "guard()", "head_sha": head,
		"differential": differential,
	})
	if err != nil {
		t.Fatalf("marshal verification: %v", err)
	}
	return store.Finding{
		ID: "f1", Severity: "major", Path: "calc/calc.go", StartLine: 3, EndLine: 3,
		Title: "Add cambió de resultado", Verification: raw,
	}
}

func TestFindingBulletRendersDifferentialEvidence(t *testing.T) {
	p := &publishContext{redactor: NewRedactor(), run: store.ReviewRun{HeadSHA: "abcdef1234567890"}}
	f := differentialVerification(t, "abcdef1234567890", map[string]any{
		"status": "regression", "case": "calc-sum",
		"base_sha": "1111111aaaaaaaa", "head_sha": "2222222bbbbbbbb",
		"base_passed": true, "head_passed": false,
	})
	got := p.findingBullet(f)
	for _, want := range []string{
		"Prueba diferencial: regresión confirmada",
		"`calc-sum`",
		"base 1111111: pasa",
		"head 2222222: falla",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("bullet does not contain %q:\n%s", want, got)
		}
	}
}

func TestFindingBulletRendersInconclusiveReason(t *testing.T) {
	p := &publishContext{redactor: NewRedactor(), run: store.ReviewRun{HeadSHA: "abcdef1234567890"}}
	f := differentialVerification(t, "abcdef1234567890", map[string]any{
		"status": "inconclusive", "case": "calc-sum", "reason": "head: dependency",
	})
	got := p.findingBullet(f)
	if !strings.Contains(got, "Prueba diferencial: inconclusa") ||
		!strings.Contains(got, "head: dependency") {
		t.Errorf("bullet = %q, want the inconclusive result and its reason", got)
	}
	if strings.Contains(got, ": pasa") || strings.Contains(got, ": falla") {
		t.Errorf("bullet = %q, want no side result for an inconclusive run", got)
	}
}

func TestFindingBulletIgnoresDifferentialFromAnotherHead(t *testing.T) {
	p := &publishContext{redactor: NewRedactor(), run: store.ReviewRun{HeadSHA: "abcdef1234567890"}}
	f := differentialVerification(t, "otherownersha", map[string]any{
		"status": "regression", "case": "calc-sum",
		"base_passed": true, "head_passed": false,
	})
	if got := p.findingBullet(f); strings.Contains(got, "Prueba diferencial") {
		t.Errorf("bullet = %q, want evidence from another head ignored", got)
	}
}
