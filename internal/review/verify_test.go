package review

import (
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func verdictComment(id, verdict, evidence string) nativeComment {
	content := "QA-VERIFY " + id + " " + verdict
	if evidence != "" {
		content += "\nEVIDENCE: " + evidence
	}
	return nativeComment{Path: "a.go", Content: content}
}

func testFinding(id, severity, source string) store.Finding {
	return store.Finding{ID: id, Path: "a.go", Severity: severity, TriageSource: source}
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

func TestEvidenceInFileRejectsHallucinatedRefutation(t *testing.T) {
	v := Verification{Verdict: VerdictRefute, Evidence: "if user == nil {"}
	if EvidenceInFile(v, []byte("if user != nil {\n\treturn nil\n}\n")) {
		t.Fatal("hallucinated evidence matched the file")
	}
	if !EvidenceInFile(v, []byte("func x() {\n\tif user == nil {\n\t}\n}\n")) {
		t.Fatal("literal evidence did not match the file")
	}
	empty := Verification{Verdict: VerdictRefute, Evidence: "   "}
	if EvidenceInFile(empty, []byte("anything")) {
		t.Fatal("empty evidence must not count as present")
	}
}

func TestDecideMissingVerdictIsUncertain(t *testing.T) {
	d := Decide([]store.Finding{testFinding("f_1", "minor", "")}, nil, nil, applyPolicy())
	if len(d) != 1 || d[0].Apply || d[0].Verdict != VerdictUncertain {
		t.Fatalf("decision = %+v, want an applied-nothing uncertain decision", d)
	}
}

func TestDecideRefuteWithoutEvidenceIsUncertain(t *testing.T) {
	f := testFinding("f_1", "minor", "")
	verdicts := map[string]Verification{"f_1": {Verdict: VerdictRefute, Evidence: "not in the file"}}
	d := Decide([]store.Finding{f}, verdicts, map[string][]byte{"a.go": []byte("guard()")}, applyPolicy())
	if len(d) != 1 || d[0].Apply || d[0].Verdict != VerdictUncertain {
		t.Fatalf("decision = %+v, want uncertain when the evidence is not in the head file", d)
	}
	if d[0].Verification.Evidence != "not in the file" {
		t.Errorf("evidence not recorded for the human: %+v", d[0].Verification)
	}
}

func TestDecideNeverWithdrawsMajorOrBlocker(t *testing.T) {
	for _, severity := range []string{"major", "blocker"} {
		f := testFinding("f_1", severity, "")
		verdicts := map[string]Verification{"f_1": {Verdict: VerdictRefute, Evidence: "guard()"}}
		d := Decide([]store.Finding{f}, verdicts, map[string][]byte{"a.go": []byte("guard()")}, applyPolicy())
		if d[0].Apply || d[0].Verdict != VerdictUncertain {
			t.Errorf("severity %s: decision = %+v, want uncertain and not applied", severity, d[0])
		}
	}
}

func TestDecideRefuteNeedsAutoWithdraw(t *testing.T) {
	f := testFinding("f_1", "nit", "")
	verdicts := map[string]Verification{"f_1": {Verdict: VerdictRefute, Evidence: "guard()"}}
	files := map[string][]byte{"a.go": []byte("guard()")}
	d := Decide([]store.Finding{f}, verdicts, files, VerifyPolicy{Mode: store.AutoTriageApply})
	if d[0].Apply || d[0].Verdict != VerdictUncertain {
		t.Fatalf("decision = %+v, want uncertain when auto_withdraw is off", d[0])
	}
}

func TestDecideConfirmAppliesForEverySeverity(t *testing.T) {
	for _, severity := range []string{"nit", "minor", "major", "blocker"} {
		f := testFinding("f_1", severity, "")
		verdicts := map[string]Verification{"f_1": {Verdict: VerdictConfirm, Evidence: "guard()"}}
		d := Decide([]store.Finding{f}, verdicts, nil, applyPolicy())
		if !d[0].Apply || d[0].Verdict != VerdictConfirm || d[0].Status != store.FindingOpen {
			t.Errorf("severity %s: decision = %+v, want an applied confirm", severity, d[0])
		}
	}
}

func TestDecideSkipsHumanTriaged(t *testing.T) {
	f := testFinding("f_1", "minor", "human")
	verdicts := map[string]Verification{"f_1": {Verdict: VerdictRefute, Evidence: "guard()"}}
	d := Decide([]store.Finding{f}, verdicts, map[string][]byte{"a.go": []byte("guard()")}, applyPolicy())
	if len(d) != 0 {
		t.Fatalf("decisions = %+v, want none for a human-triaged finding", d)
	}
}

func TestDecideDryRunNeverApplies(t *testing.T) {
	confirm := testFinding("f_1", "minor", "")
	refute := testFinding("f_2", "nit", "")
	verdicts := map[string]Verification{
		"f_1": {Verdict: VerdictConfirm, Evidence: "guard()"},
		"f_2": {Verdict: VerdictRefute, Evidence: "guard()"},
	}
	d := Decide([]store.Finding{confirm, refute}, verdicts,
		map[string][]byte{"a.go": []byte("guard()")}, VerifyPolicy{Mode: store.AutoTriageDryRun, AutoWithdraw: true})
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
	verdicts := map[string]Verification{"f_1": {Verdict: VerdictConfirm, Evidence: "guard()"}}
	d := Decide([]store.Finding{f}, verdicts, nil, VerifyPolicy{Mode: store.AutoTriageOff})
	if len(d) != 1 || d[0].Apply {
		t.Fatalf("decision = %+v, want a recorded but unapplied verdict", d)
	}
}
