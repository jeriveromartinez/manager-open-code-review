package review

import (
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const verifyHead = "head-1"

func verdictComment(id, verdict, evidence string) nativeComment {
	content := "QA-VERIFY " + id + " " + verdict
	if evidence != "" {
		content += "\nEVIDENCE: " + evidence
	}
	return nativeComment{Path: "a.go", Content: content}
}

func testFinding(id, severity, source string) store.Finding {
	return store.Finding{
		ID: id, Path: "a.go", Severity: severity, TriageSource: source,
		StartLine: 3, EndLine: 3,
	}
}

func testFiles(content string) map[string][]byte {
	return map[string][]byte{"a.go": []byte(content)}
}

func verified(verdict Verdict, evidence string) Verification {
	return Verification{Verdict: verdict, Evidence: evidence, HeadSHA: verifyHead}
}

func applyPolicy() VerifyPolicy {
	return VerifyPolicy{Mode: store.AutoTriageApply, AutoWithdraw: true}
}

func TestParseVerdictsReadsConfirmRefuteUncertain(t *testing.T) {
	got := ParseVerdicts([]nativeComment{
		verdictComment("f_1", "CONFIRM", "guard()"),
		verdictComment("f_2", "REFUTE", "return nil"),
		verdictComment("f_3", "UNCERTAIN", ""),
	})
	if len(got) != 3 {
		t.Fatalf("verdicts = %d, want 3", len(got))
	}
	if got["f_1"].Verdict != VerdictConfirm || got["f_1"].Evidence != "guard()" {
		t.Errorf("f_1 = %+v", got["f_1"])
	}
	if got["f_2"].Verdict != VerdictRefute || got["f_2"].Evidence != "return nil" {
		t.Errorf("f_2 = %+v", got["f_2"])
	}
	if got["f_3"].Verdict != VerdictUncertain {
		t.Errorf("f_3 = %+v", got["f_3"])
	}
}

func TestParseVerdictsIgnoresMissingPrefix(t *testing.T) {
	got := ParseVerdicts([]nativeComment{
		{Content: "This looks wrong to me."},
		{Content: "QA-VERIFY\nCONFIRM"},
		{Content: "QA-VERIFY f_1 MAYBE"},
		{Content: "QA-VERIFY f_2"},
	})
	if len(got) != 0 {
		t.Fatalf("verdicts = %+v, want none for comments without the contract", got)
	}
}

func TestParseVerdictsKeepsFirstOfDuplicateIDs(t *testing.T) {
	got := ParseVerdicts([]nativeComment{
		verdictComment("f_1", "CONFIRM", "first"),
		verdictComment("f_1", "REFUTE", "second"),
	})
	if got["f_1"].Verdict != VerdictConfirm || got["f_1"].Evidence != "first" {
		t.Fatalf("duplicate id = %+v, want the first verdict", got["f_1"])
	}
}

func TestValidateEvidence(t *testing.T) {
	content := "package a\n\n\n\n\n\n\n\n\nfunc guard() {\nfunc other() {\n}\n"
	f := testFinding("f_1", "minor", "")
	f.StartLine, f.EndLine = 10, 10
	cases := []struct {
		name     string
		evidence string
		head     string
		files    map[string][]byte
		want     string
	}{
		{"valid quote at the cited line", "func guard() {", verifyHead, testFiles(content), ""},
		{"quote at a neighbouring line is not evidence", "func other() {", verifyHead, testFiles(content), "la evidencia no coincide con las líneas citadas del hallazgo"},
		{"quote at the wrong line", "package a", verifyHead, testFiles(content), "la evidencia no coincide con las líneas citadas del hallazgo"},
		{"missing quote", "frobnicate()", verifyHead, testFiles(content), "la evidencia no coincide con las líneas citadas del hallazgo"},
		{"empty quote", "   ", verifyHead, testFiles(content), "el verificador no citó evidencia"},
		{"stale head", "func guard() {", "head-2", testFiles(content), "la verificación no corresponde al head revisado"},
		{"evidence from another file", "func guard() {", verifyHead, map[string][]byte{"b.go": []byte(content)}, "el archivo citado no existe en el head revisado"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := Verification{Verdict: VerdictConfirm, Evidence: tc.evidence, HeadSHA: tc.head}
			if got := validateEvidence(f, v, tc.files, verifyHead); got != tc.want {
				t.Errorf("validateEvidence() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDecideConfirmRequiresValidEvidence(t *testing.T) {
	f := testFinding("f_1", "minor", "")
	valid := Decide([]store.Finding{f}, map[string]Verification{"f_1": verified(VerdictConfirm, "func guard() {")},
		testFiles("package a\n\nfunc guard() {\n}\n"), verifyHead, applyPolicy())
	if len(valid) != 1 || !valid[0].Apply || valid[0].Status != store.FindingOpen ||
		valid[0].Verdict != VerdictConfirm || valid[0].Verification.Reason != "" {
		t.Fatalf("valid confirm = %+v, want an applied confirm", valid)
	}

	invalid := Decide([]store.Finding{f}, map[string]Verification{"f_1": verified(VerdictConfirm, "func other() {")},
		testFiles("package a\n\nfunc guard() {\n}\n"), verifyHead, applyPolicy())
	if invalid[0].Apply || invalid[0].Verdict != VerdictUncertain || invalid[0].Status != f.Status {
		t.Fatalf("invalid confirm = %+v, want an unapplied uncertainty", invalid[0])
	}
	if invalid[0].Reason != "la evidencia no coincide con las líneas citadas del hallazgo" ||
		invalid[0].Verification.Reason != invalid[0].Reason ||
		invalid[0].Verification.Verdict != VerdictUncertain {
		t.Errorf("invalid confirm does not record the reason: %+v", invalid[0])
	}
}

func TestDecideRefuteRejectsHallucinatedEvidence(t *testing.T) {
	f := testFinding("f_1", "nit", "")
	verdicts := map[string]Verification{"f_1": verified(VerdictRefute, "if user == nil {")}
	d := Decide([]store.Finding{f}, verdicts, testFiles("if user != nil {\n\treturn nil\n}\n"), verifyHead, applyPolicy())
	if d[0].Apply || d[0].Verdict != VerdictUncertain || d[0].Status != f.Status {
		t.Fatalf("decision = %+v, want an unapplied uncertainty", d[0])
	}
	if d[0].Verification.Evidence != "if user == nil {" {
		t.Errorf("evidence not recorded for the human: %+v", d[0].Verification)
	}
	if d[0].Verification.Reason == "" {
		t.Error("a rejected refutation must record why it was rejected")
	}
}

func TestDecideRefuteWithEvidenceAtTheWrongLineIsUncertain(t *testing.T) {
	f := testFinding("f_1", "nit", "")
	f.StartLine, f.EndLine = 10, 10
	verdicts := map[string]Verification{"f_1": verified(VerdictRefute, "package a")}
	d := Decide([]store.Finding{f}, verdicts, testFiles("package a\n\n\n\n\n\n\n\n\nfunc guard() {\n\treturn nil\n}\n"), verifyHead, applyPolicy())
	if d[0].Apply || d[0].Verdict != VerdictUncertain {
		t.Fatalf("decision = %+v, want uncertain when the quote is outside the cited range", d[0])
	}
	if d[0].Reason != "la evidencia no coincide con las líneas citadas del hallazgo" {
		t.Errorf("reason = %q, want the range mismatch", d[0].Reason)
	}
}

func TestDecideRefuteUsesEvidenceOnlyFromTheCitedFile(t *testing.T) {
	f := testFinding("f_1", "nit", "")
	verdicts := map[string]Verification{"f_1": verified(VerdictRefute, "func guard() {")}

	other := Decide([]store.Finding{f}, verdicts,
		map[string][]byte{"b.go": []byte("func guard() {\n}\n")}, verifyHead, applyPolicy())
	if other[0].Apply || other[0].Verdict != VerdictUncertain {
		t.Fatalf("decision = %+v, want uncertain when the quote is from another file", other[0])
	}
	if other[0].Reason != "el archivo citado no existe en el head revisado" {
		t.Errorf("reason = %q, want the missing file", other[0].Reason)
	}

	cited := Decide([]store.Finding{f}, verdicts, testFiles("package a\n\nfunc guard() {\n}\n"), verifyHead, applyPolicy())
	if !cited[0].Apply || cited[0].Status != store.FindingWithdrawn {
		t.Errorf("decision = %+v, want the refutation applied with evidence in the cited file", cited[0])
	}
}

func TestDecideStaleVerdictIsUncertain(t *testing.T) {
	f := testFinding("f_1", "minor", "")
	verdicts := map[string]Verification{"f_1": {Verdict: VerdictConfirm, Evidence: "func guard() {", HeadSHA: "head-2"}}
	d := Decide([]store.Finding{f}, verdicts, testFiles("func guard() {\n}\n"), verifyHead, applyPolicy())
	if d[0].Apply || d[0].Verdict != VerdictUncertain {
		t.Fatalf("decision = %+v, want uncertain for a verdict from another head", d[0])
	}
	if d[0].Reason != "la verificación no corresponde al head revisado" {
		t.Errorf("reason = %q, want the stale head", d[0].Reason)
	}
}

func TestDecideMissingVerdictIsUncertain(t *testing.T) {
	d := Decide([]store.Finding{testFinding("f_1", "minor", "")}, nil, nil, verifyHead, applyPolicy())
	if len(d) != 1 || d[0].Apply || d[0].Verdict != VerdictUncertain {
		t.Fatalf("decision = %+v, want an applied-nothing uncertain decision", d)
	}
	if d[0].Verification.Reason == "" || d[0].Verification.HeadSHA != verifyHead {
		t.Errorf("missing answer does not record its reason: %+v", d[0].Verification)
	}
}

func TestDecideNeverWithdrawsMajorOrBlocker(t *testing.T) {
	for _, severity := range []string{"major", "blocker"} {
		f := testFinding("f_1", severity, "")
		verdicts := map[string]Verification{"f_1": verified(VerdictRefute, "func guard() {")}
		d := Decide([]store.Finding{f}, verdicts, testFiles("func guard() {\n}\n"), verifyHead, applyPolicy())
		if d[0].Apply || d[0].Verdict != VerdictUncertain {
			t.Errorf("severity %s: decision = %+v, want uncertain and not applied", severity, d[0])
		}
	}
}

func TestDecidePolicyDegradationKeepsTheVerifierVerdict(t *testing.T) {
	f := testFinding("f_1", "major", "")
	verdicts := map[string]Verification{"f_1": verified(VerdictRefute, "func guard() {")}
	d := Decide([]store.Finding{f}, verdicts, testFiles("func guard() {\n}\n"), verifyHead, applyPolicy())
	if d[0].Verdict != VerdictUncertain || d[0].Apply || d[0].Status != f.Status {
		t.Fatalf("decision = %+v, want the policy to refuse the dismissal", d[0])
	}
	if d[0].Verification.Verdict != VerdictRefute || d[0].Verification.Evidence != "func guard() {" {
		t.Errorf("verification = %+v, want OCR's raw refutation kept for a dry run", d[0].Verification)
	}
	if d[0].Verification.Reason == "" {
		t.Error("policy degradation must record its reason")
	}
}

func TestDecideRefuteNeedsAutoWithdraw(t *testing.T) {
	f := testFinding("f_1", "nit", "")
	verdicts := map[string]Verification{"f_1": verified(VerdictRefute, "func guard() {")}
	files := testFiles("func guard() {\n}\n")
	d := Decide([]store.Finding{f}, verdicts, files, verifyHead, VerifyPolicy{Mode: store.AutoTriageApply})
	if d[0].Apply || d[0].Verdict != VerdictUncertain {
		t.Fatalf("decision = %+v, want uncertain when auto_withdraw is off", d[0])
	}
}

func TestDecideConfirmAppliesForEverySeverity(t *testing.T) {
	for _, severity := range []string{"nit", "minor", "major", "blocker"} {
		f := testFinding("f_1", severity, "")
		verdicts := map[string]Verification{"f_1": verified(VerdictConfirm, "func guard() {")}
		d := Decide([]store.Finding{f}, verdicts, testFiles("package a\n\nfunc guard() {\n}\n"), verifyHead, applyPolicy())
		if !d[0].Apply || d[0].Verdict != VerdictConfirm || d[0].Status != store.FindingOpen {
			t.Errorf("severity %s: decision = %+v, want an applied confirm", severity, d[0])
		}
	}
}

func TestDecideSkipsHumanTriaged(t *testing.T) {
	f := testFinding("f_1", "minor", "human")
	verdicts := map[string]Verification{"f_1": verified(VerdictRefute, "func guard() {")}
	d := Decide([]store.Finding{f}, verdicts, testFiles("func guard() {\n}\n"), verifyHead, applyPolicy())
	if len(d) != 0 {
		t.Fatalf("decisions = %+v, want none for a human-triaged finding", d)
	}
}

func TestDecideDryRunNeverApplies(t *testing.T) {
	confirm := testFinding("f_1", "minor", "")
	refute := testFinding("f_2", "nit", "")
	verdicts := map[string]Verification{
		"f_1": verified(VerdictConfirm, "func guard() {"),
		"f_2": verified(VerdictRefute, "func guard() {"),
	}
	d := Decide([]store.Finding{confirm, refute}, verdicts,
		testFiles("package a\n\nfunc guard() {\n}\n"), verifyHead, VerifyPolicy{Mode: store.AutoTriageDryRun, AutoWithdraw: true})
	if len(d) != 2 {
		t.Fatalf("decisions = %d, want the verdicts recorded", len(d))
	}
	for _, decision := range d {
		if decision.Apply {
			t.Errorf("dry run applied %+v", decision)
		}
	}
	if d[0].Verdict != VerdictConfirm || d[1].Status != store.FindingWithdrawn {
		t.Errorf("dry run lost what would happen: %+v", d)
	}
}

func TestDecideOffNeverApplies(t *testing.T) {
	f := testFinding("f_1", "minor", "")
	verdicts := map[string]Verification{"f_1": verified(VerdictConfirm, "func guard() {")}
	d := Decide([]store.Finding{f}, verdicts, testFiles("package a\n\nfunc guard() {\n}\n"), verifyHead, VerifyPolicy{Mode: store.AutoTriageOff})
	if len(d) != 1 || d[0].Apply {
		t.Fatalf("decision = %+v, want a recorded but unapplied verdict", d)
	}
}
