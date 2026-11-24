package review

import (
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestFingerprintStableAcrossLineNumbers(t *testing.T) {
	base := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	shifted := append([]string{"X"}, base[:7]...)
	f1 := AgentFinding{Path: "src/a.go", Category: "bug", Title: "Bloqueo", StartLine: 4, EndLine: 4}
	f2 := AgentFinding{Path: "src/a.go", Category: "bug", Title: "Bloqueo", StartLine: 5, EndLine: 5}

	got := Fingerprint(f1, base)
	if want := Fingerprint(f2, shifted); got != want {
		t.Fatalf("fingerprint changed with line number: %s != %s", got, want)
	}
	if len(got) != 64 || strings.ToLower(got) != got {
		t.Fatalf("expected lowercase hex sha256, got %q", got)
	}
}

func TestFingerprintChangesWithContext(t *testing.T) {
	f := AgentFinding{Path: "src/a.go", Category: "bug", Title: "x", StartLine: 4, EndLine: 4}
	a := Fingerprint(f, []string{"a", "b", "c", "d", "e", "f", "g"})
	b := Fingerprint(f, []string{"a", "b", "c", "Z", "e", "f", "g"})
	if a == b {
		t.Fatal("context not included in fingerprint")
	}
}

func TestFingerprintNormalizesTitle(t *testing.T) {
	lines := []string{"a", "b", "c", "d", "e", "f", "g"}
	a := AgentFinding{Path: "p", Category: "bug", Title: "Fix  BUG Now", StartLine: 4, EndLine: 4}
	b := AgentFinding{Path: "p", Category: "bug", Title: "  fix bug   now ", StartLine: 4, EndLine: 4}
	if Fingerprint(a, lines) != Fingerprint(b, lines) {
		t.Fatal("title normalization failed")
	}
}

func TestFingerprintClampsContext(t *testing.T) {
	lines := []string{"a", "b", "c"}
	low := AgentFinding{Path: "p", Category: "bug", Title: "t", StartLine: -5, EndLine: 1}
	high := AgentFinding{Path: "p", Category: "bug", Title: "t", StartLine: 1, EndLine: 50}
	if got, want := Fingerprint(low, lines), Fingerprint(high, lines); got != want {
		t.Fatalf("out-of-bounds window not clamped: %s != %s", got, want)
	}
}

func TestReconcileResolvesOnlyWhenComplete(t *testing.T) {
	af := AgentFinding{Path: "src/a.go", Severity: "major", Category: "bug", Title: "Keep me", StartLine: 3, EndLine: 3}
	fp := Fingerprint(af, nil)
	open := []store.Finding{{ID: "f_existing", Fingerprint: fp, Status: store.FindingOpen, Title: "old"}}

	upserts, resolved := Reconcile(open, AgentOutput{Findings: []AgentFinding{af}}, 7, false)
	if len(resolved) != 0 {
		t.Fatalf("resolved with complete=false: %v", resolved)
	}
	if len(upserts) != 1 || upserts[0].ID != "f_existing" {
		t.Fatalf("expected matched upsert keeping ID: %+v", upserts)
	}

	upserts, resolved = Reconcile(open, AgentOutput{}, 7, true)
	if len(upserts) != 0 {
		t.Fatalf("unexpected upserts: %+v", upserts)
	}
	if len(resolved) != 1 || resolved[0] != "f_existing" {
		t.Fatalf("expected resolved ID when complete: %v", resolved)
	}
}

func TestReconcileDoesNotResolveClosedFindings(t *testing.T) {
	known := []store.Finding{
		{ID: "f_open", Fingerprint: "fp-open", Status: store.FindingOpen},
		{ID: "f_withdrawn", Fingerprint: "fp-withdrawn", Status: store.FindingWithdrawn},
		{ID: "f_resolved", Fingerprint: "fp-resolved", Status: store.FindingResolved},
	}
	_, resolved := Reconcile(known, AgentOutput{}, 9, true)
	if len(resolved) != 1 || resolved[0] != "f_open" {
		t.Fatalf("resolved = %v, want only the open finding", resolved)
	}
}

func TestReconcileDeterministicIDs(t *testing.T) {
	af := AgentFinding{Path: "src/b.go", Severity: "minor", Category: "style", Title: "Nuevo", StartLine: 2, EndLine: 2}
	fp := Fingerprint(af, nil)
	wantID := "f_" + fp[:16]
	out := AgentOutput{Findings: []AgentFinding{af}}

	u1, _ := Reconcile(nil, out, 1, false)
	u2, _ := Reconcile(nil, out, 2, true)
	if u1[0].ID != wantID || u2[0].ID != wantID {
		t.Fatalf("IDs not stable: %s, %s (want %s)", u1[0].ID, u2[0].ID, wantID)
	}
	if u1[0].FirstSeenRunID != 1 || u2[0].LastSeenRunID != 2 {
		t.Fatalf("run IDs not recorded: %+v, %+v", u1[0], u2[0])
	}
}

func TestReconcileKeepsExistingFields(t *testing.T) {
	af := AgentFinding{Path: "src/c.go", Severity: "blocker", Category: "security", Title: "Updated", StartLine: 6, EndLine: 8, Body: "new body"}
	fp := Fingerprint(af, nil)
	open := []store.Finding{{
		ID:              "f_old",
		Fingerprint:     fp,
		PullRequestID:   9,
		Severity:        "minor",
		Title:           "old",
		Status:          store.FindingOpen,
		FirstSeenRunID:  1,
		LastSeenRunID:   2,
		GithubCommentID: 42,
	}}

	u, _ := Reconcile(open, AgentOutput{Findings: []AgentFinding{af}}, 3, true)
	if len(u) != 1 {
		t.Fatalf("expected one upsert: %+v", u)
	}
	got := u[0]
	if got.ID != "f_old" || got.FirstSeenRunID != 1 || got.LastSeenRunID != 3 ||
		got.GithubCommentID != 42 || got.PullRequestID != 9 || got.Status != store.FindingOpen {
		t.Fatalf("existing fields not preserved: %+v", got)
	}
	if got.Title != "Updated" || got.Severity != "blocker" || got.Body != "new body" ||
		got.StartLine != 6 || got.EndLine != 8 || got.Fingerprint != fp {
		t.Fatalf("content not updated: %+v", got)
	}
}

func TestReconcileCarriesExistingCode(t *testing.T) {
	af := AgentFinding{
		Path: "src/c.go", Severity: "minor", Category: "correctness", Title: "Guard",
		StartLine: 4, EndLine: 4, ExistingCode: "guard()",
	}
	upserts, _ := Reconcile(nil, AgentOutput{Findings: []AgentFinding{af}}, 1, true)
	if len(upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(upserts))
	}
	if upserts[0].ExistingCode != "guard()" {
		t.Errorf("ExistingCode = %q, want guard()", upserts[0].ExistingCode)
	}
}

func TestReconcileCarriesTriageState(t *testing.T) {
	af := AgentFinding{
		Path: "src/c.go", Severity: "minor", Category: "correctness", Title: "Guard",
		StartLine: 4, EndLine: 4, ExistingCode: "guard()",
	}
	fp := Fingerprint(af, nil)
	triagedAt := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	verification := datatypes.JSON(`{"verdict":"refute","evidence":"guard()"}`)
	open := []store.Finding{{
		ID: "f_triaged", Fingerprint: fp, PullRequestID: 9, Status: store.FindingOpen,
		TriageVerdict: store.VerdictFalsePositive, TriageSource: "human",
		TriagedBy: "qa-user", TriagedAt: &triagedAt, Verification: verification,
	}}

	upserts, _ := Reconcile(open, AgentOutput{Findings: []AgentFinding{af}}, 2, true)
	if len(upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(upserts))
	}
	got := upserts[0]
	if got.TriageVerdict != store.VerdictFalsePositive || got.TriageSource != "human" ||
		got.TriagedBy != "qa-user" || got.TriagedAt == nil || !got.TriagedAt.Equal(triagedAt) ||
		string(got.Verification) != string(verification) {
		t.Errorf("triage state not carried: %+v", got)
	}
}
