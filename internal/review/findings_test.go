package review

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

func TestFingerprintStableAcrossLineNumbers(t *testing.T) {
	base := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	shifted := append([]string{"X"}, base[:7]...)
	f1 := AgentFinding{Path: "src/a.go", Category: "bug", Title: "Bloqueo", StartLine: 4, EndLine: 4}
	f2 := AgentFinding{Path: "src/a.go", Category: "bug", Title: "Bloqueo", StartLine: 5, EndLine: 5}

	got := Fingerprint(7, f1, base, 0)
	if want := Fingerprint(7, f2, shifted, 0); got != want {
		t.Fatalf("fingerprint changed with line number: %s != %s", got, want)
	}
	if len(got) != 64 || strings.ToLower(got) != got {
		t.Fatalf("expected lowercase hex sha256, got %q", got)
	}
}

func TestFingerprintChangesWithContext(t *testing.T) {
	f := AgentFinding{Path: "src/a.go", Category: "bug", Title: "x", StartLine: 4, EndLine: 4}
	a := Fingerprint(1, f, []string{"a", "b", "c", "d", "e", "f", "g"}, 0)
	b := Fingerprint(1, f, []string{"a", "b", "c", "Z", "e", "f", "g"}, 0)
	if a == b {
		t.Fatal("context not included in fingerprint")
	}
}

func TestFingerprintPrefersExistingCodeAsAnchor(t *testing.T) {
	f := AgentFinding{Path: "src/a.go", Category: "bug", Title: "x", StartLine: 4, EndLine: 4, ExistingCode: "  guard()  "}
	lines := []string{"a", "b", "c", "d", "e", "f", "g"}
	if got, want := Fingerprint(1, f, lines, 0), Fingerprint(1, AgentFinding{
		Path: "src/a.go", Category: "bug", Title: "x", StartLine: 4, EndLine: 4, ExistingCode: "guard()",
	}, nil, 0); got != want {
		t.Fatalf("ExistingCode anchor not normalized: %s != %s", got, want)
	}
}

func TestFingerprintNormalizesTitle(t *testing.T) {
	lines := []string{"a", "b", "c", "d", "e", "f", "g"}
	a := AgentFinding{Path: "p", Category: "bug", Title: "Fix  BUG Now", StartLine: 4, EndLine: 4}
	b := AgentFinding{Path: "p", Category: "bug", Title: "  fix bug   now ", StartLine: 4, EndLine: 4}
	if Fingerprint(1, a, lines, 0) != Fingerprint(1, b, lines, 0) {
		t.Fatal("title normalization failed")
	}
}

func TestFingerprintClampsContext(t *testing.T) {
	lines := []string{"a", "b", "c"}
	low := AgentFinding{Path: "p", Category: "bug", Title: "t", StartLine: -5, EndLine: 1}
	high := AgentFinding{Path: "p", Category: "bug", Title: "t", StartLine: 1, EndLine: 50}
	if got, want := Fingerprint(1, low, lines, 0), Fingerprint(1, high, lines, 0); got != want {
		t.Fatalf("out-of-bounds window not clamped: %s != %s", got, want)
	}
}

func TestFingerprintIncludesPullRequest(t *testing.T) {
	f := AgentFinding{Path: "src/a.go", Category: "bug", Title: "Same", StartLine: 3, EndLine: 3}
	fp1 := Fingerprint(10, f, nil, 0)
	fp2 := Fingerprint(20, f, nil, 0)
	if fp1 == fp2 {
		t.Fatal("fingerprint does not include the pull request")
	}
	if FindingID(10, fp1) == FindingID(20, fp2) {
		t.Fatal("finding IDs collide across pull requests")
	}
	if id := FindingID(10, fp1); len(id) != 18 || !strings.HasPrefix(id, "f_") {
		t.Fatalf("FindingID = %q, want f_ plus 16 hex characters", id)
	}
}

func TestReconcileResolvesOnlyWhenComplete(t *testing.T) {
	af := AgentFinding{Path: "src/a.go", Severity: "major", Category: "bug", Title: "Keep me", StartLine: 3, EndLine: 3}
	fp := Fingerprint(9, af, nil, 0)
	open := []store.Finding{{
		ID: "f_existing", PullRequestID: 9, Fingerprint: fp, Path: "src/a.go",
		Category: "bug", Title: "Keep me", StartLine: 3, EndLine: 3,
		IdentityVersion: store.IdentityVersionCurrent, Status: store.FindingOpen,
	}}

	upserts, resolved := Reconcile(open, AgentOutput{Findings: []AgentFinding{af}}, 7, false, 9, nil)
	if len(resolved) != 0 {
		t.Fatalf("resolved with complete=false: %v", resolved)
	}
	if len(upserts) != 1 || upserts[0].ID != "f_existing" {
		t.Fatalf("expected matched upsert keeping ID: %+v", upserts)
	}

	upserts, resolved = Reconcile(open, AgentOutput{}, 7, true, 9, nil)
	if len(upserts) != 0 {
		t.Fatalf("unexpected upserts: %+v", upserts)
	}
	if len(resolved) != 1 || resolved[0] != "f_existing" {
		t.Fatalf("expected resolved ID when complete: %v", resolved)
	}
}

func TestReconcileDoesNotResolveClosedFindings(t *testing.T) {
	known := []store.Finding{
		{ID: "f_open", PullRequestID: 9, Fingerprint: "fp-open", IdentityVersion: store.IdentityVersionCurrent, Status: store.FindingOpen},
		{ID: "f_withdrawn", PullRequestID: 9, Fingerprint: "fp-withdrawn", IdentityVersion: store.IdentityVersionCurrent, Status: store.FindingWithdrawn},
		{ID: "f_resolved", PullRequestID: 9, Fingerprint: "fp-resolved", IdentityVersion: store.IdentityVersionCurrent, Status: store.FindingResolved},
	}
	_, resolved := Reconcile(known, AgentOutput{}, 9, true, 9, nil)
	if len(resolved) != 1 || resolved[0] != "f_open" {
		t.Fatalf("resolved = %v, want only the open finding", resolved)
	}
}

func TestReconcileDeterministicIDs(t *testing.T) {
	af := AgentFinding{Path: "src/b.go", Severity: "minor", Category: "style", Title: "Nuevo", StartLine: 2, EndLine: 2}
	fp := Fingerprint(9, af, nil, 0)
	wantID := FindingID(9, fp)
	out := AgentOutput{Findings: []AgentFinding{af}}

	u1, _ := Reconcile(nil, out, 1, false, 9, nil)
	u2, _ := Reconcile(nil, out, 2, true, 9, nil)
	if u1[0].ID != wantID || u2[0].ID != wantID {
		t.Fatalf("IDs not stable: %s, %s (want %s)", u1[0].ID, u2[0].ID, wantID)
	}
	if u1[0].FirstSeenRunID != 1 || u2[0].LastSeenRunID != 2 {
		t.Fatalf("run IDs not recorded: %+v, %+v", u1[0], u2[0])
	}
	if u1[0].IdentityVersion != store.IdentityVersionCurrent {
		t.Fatalf("identity version = %d, want %d", u1[0].IdentityVersion, store.IdentityVersionCurrent)
	}
}

func TestReconcileDistinctPullRequestsGetDistinctIDs(t *testing.T) {
	af := AgentFinding{Path: "src/a.go", Severity: "major", Category: "bug", Title: "Mismo", StartLine: 3, EndLine: 3}
	out := AgentOutput{Findings: []AgentFinding{af}}

	u1, _ := Reconcile(nil, out, 1, true, 10, nil)
	u2, _ := Reconcile(nil, out, 1, true, 20, nil)
	if len(u1) != 1 || len(u2) != 1 {
		t.Fatalf("upserts = %d/%d, want 1/1", len(u1), len(u2))
	}
	if u1[0].ID == u2[0].ID {
		t.Fatalf("two pull requests share finding ID %q", u1[0].ID)
	}
	if u1[0].PullRequestID != 10 || u2[0].PullRequestID != 20 {
		t.Fatalf("pull request IDs = %d/%d, want 10/20", u1[0].PullRequestID, u2[0].PullRequestID)
	}
}

func TestReconcileKeepsDistantOccurrences(t *testing.T) {
	first := AgentFinding{
		Path: "src/a.go", Severity: "major", Category: "bug", Title: "Mismo",
		ExistingCode: "same code", StartLine: 10, EndLine: 10, Body: "first",
	}
	second := first
	second.StartLine, second.EndLine, second.Body = 40, 40, "second"

	upserts, _ := Reconcile(nil, AgentOutput{Findings: []AgentFinding{first, second}}, 1, true, 9, nil)
	if len(upserts) != 2 {
		t.Fatalf("upserts = %d, want both occurrences: %+v", len(upserts), upserts)
	}
	if upserts[0].ID == upserts[1].ID {
		t.Fatalf("distant occurrences share ID %q", upserts[0].ID)
	}
}

func TestReconcileOrdinalOrderIsIndependentOfInputOrder(t *testing.T) {
	first := AgentFinding{
		Path: "src/a.go", Severity: "major", Category: "bug", Title: "Mismo",
		ExistingCode: "same code", StartLine: 10, EndLine: 10, Body: "first",
	}
	second := first
	second.StartLine, second.EndLine, second.Body = 40, 40, "second"

	forward, _ := Reconcile(nil, AgentOutput{Findings: []AgentFinding{first, second}}, 1, true, 9, nil)
	reverse, _ := Reconcile(nil, AgentOutput{Findings: []AgentFinding{second, first}}, 1, true, 9, nil)

	if ids := idSet(forward); !equalSets(ids, idSet(reverse)) {
		t.Fatalf("IDs depend on insertion order: %v != %v", ids, idSet(reverse))
	}
	byBody := make(map[string]string, len(forward))
	for _, f := range forward {
		byBody[f.Body] = f.ID
	}
	for _, f := range reverse {
		if byBody[f.Body] != f.ID {
			t.Fatalf("occurrence %q changed ID with input order: %q != %q", f.Body, byBody[f.Body], f.ID)
		}
	}
}

func TestReconcileShiftedOccurrenceKeepsID(t *testing.T) {
	base := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	shifted := append([]string{"X"}, base...)
	before := AgentFinding{Path: "src/a.go", Category: "bug", Title: "Bloqueo", StartLine: 4, EndLine: 4}
	after := before
	after.StartLine, after.EndLine = 5, 5

	first, _ := Reconcile(nil, AgentOutput{Findings: []AgentFinding{before}}, 1, true, 7, linesProvider(base))
	second, resolved := Reconcile(first, AgentOutput{Findings: []AgentFinding{after}}, 2, true, 7, linesProvider(shifted))
	if len(second) != 1 || len(resolved) != 0 {
		t.Fatalf("upserts/resolved = %d/%v, want 1/none", len(second), resolved)
	}
	if second[0].ID != first[0].ID {
		t.Fatalf("shifted occurrence changed ID: %q != %q", second[0].ID, first[0].ID)
	}
	if second[0].StartLine != 5 || second[0].Fingerprint != first[0].Fingerprint {
		t.Fatalf("shifted occurrence = %+v, want the same identity with the updated range", second[0])
	}
}

func TestReconcileMigratesLegacyRowInPlace(t *testing.T) {
	af := AgentFinding{
		Path: "src/a.go", Category: "bug", Title: "Legacy", StartLine: 4, EndLine: 4, Body: "body",
	}
	legacyFP := LegacyFingerprint(af, nil)
	legacy := store.Finding{
		ID: "f_legacy", PullRequestID: 9, Fingerprint: legacyFP,
		IdentityVersion: store.IdentityVersionLegacy,
		Path:            "src/a.go", Category: "bug", Title: "Legacy",
		StartLine: 4, EndLine: 4, Body: "body", Status: store.FindingOpen,
		FirstSeenRunID: 2, GithubCommentID: 77, GithubCommentNodeID: "node-1", GithubThreadID: "thread-1",
	}

	upserts, resolved := Reconcile([]store.Finding{legacy}, AgentOutput{Findings: []AgentFinding{af}}, 5, true, 9, nil)
	if len(upserts) != 1 || len(resolved) != 0 {
		t.Fatalf("upserts/resolved = %d/%v, want 1/none: %+v", len(upserts), resolved, upserts)
	}
	got := upserts[0]
	if got.ID != legacy.ID {
		t.Fatalf("legacy ID = %q, want %q", got.ID, legacy.ID)
	}
	if got.IdentityVersion != store.IdentityVersionCurrent || got.Fingerprint == legacyFP {
		t.Fatalf("legacy row not migrated: version %d, fingerprint %q", got.IdentityVersion, got.Fingerprint)
	}
	if got.FirstSeenRunID != 2 || got.GithubCommentID != 77 ||
		got.GithubCommentNodeID != "node-1" || got.GithubThreadID != "thread-1" {
		t.Fatalf("legacy references not preserved: %+v", got)
	}

	// The migrated row is matched by the new fingerprint on the next run.
	again, resolved := Reconcile(upserts, AgentOutput{Findings: []AgentFinding{af}}, 6, true, 9, nil)
	if len(again) != 1 || again[0].ID != legacy.ID || again[0].Fingerprint != got.Fingerprint ||
		len(resolved) != 0 {
		t.Fatalf("rerun not idempotent: %+v resolved=%v", again, resolved)
	}
	if again[0].LastSeenRunID != 6 {
		t.Fatalf("last_seen_run_id = %d, want 6", again[0].LastSeenRunID)
	}
}

func TestReconcileResolvesAbsentLegacyRowOnlyWhenComplete(t *testing.T) {
	legacy := store.Finding{
		ID: "f_absent", PullRequestID: 9, Fingerprint: "old-fp",
		IdentityVersion: store.IdentityVersionLegacy,
		Path:            "src/x.go", Category: "style", Title: "Ausente", Status: store.FindingOpen,
	}

	_, resolved := Reconcile([]store.Finding{legacy}, AgentOutput{}, 1, false, 9, nil)
	if len(resolved) != 0 {
		t.Fatalf("legacy row resolved on an incomplete run: %v", resolved)
	}
	_, resolved = Reconcile([]store.Finding{legacy}, AgentOutput{}, 1, true, 9, nil)
	if len(resolved) != 1 || resolved[0] != legacy.ID {
		t.Fatalf("resolved = %v, want the absent legacy row", resolved)
	}
}

func TestReconcileKeepsExistingFields(t *testing.T) {
	af := AgentFinding{Path: "src/c.go", Severity: "blocker", Category: "security", Title: "Updated", StartLine: 6, EndLine: 8, Body: "new body"}
	fp := Fingerprint(9, af, nil, 0)
	open := []store.Finding{{
		ID:              "f_old",
		PullRequestID:   9,
		Fingerprint:     fp,
		IdentityVersion: store.IdentityVersionCurrent,
		Path:            "src/c.go",
		Category:        "security",
		Title:           "old",
		StartLine:       6,
		EndLine:         8,
		Severity:        "minor",
		Status:          store.FindingOpen,
		FirstSeenRunID:  1,
		LastSeenRunID:   2,
		GithubCommentID: 42,
	}}

	u, _ := Reconcile(open, AgentOutput{Findings: []AgentFinding{af}}, 3, true, 9, nil)
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
	upserts, _ := Reconcile(nil, AgentOutput{Findings: []AgentFinding{af}}, 1, true, 9, nil)
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
	fp := Fingerprint(9, af, nil, 0)
	triagedAt := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	verification := datatypes.JSON(`{"verdict":"refute","evidence":"guard()"}`)
	open := []store.Finding{{
		ID: "f_triaged", Fingerprint: fp, PullRequestID: 9, IdentityVersion: store.IdentityVersionCurrent,
		Status:        store.FindingOpen,
		TriageVerdict: store.VerdictFalsePositive, TriageSource: "human",
		TriagedBy: "qa-user", TriagedAt: &triagedAt, Verification: verification,
	}}

	upserts, _ := Reconcile(open, AgentOutput{Findings: []AgentFinding{af}}, 2, true, 9, nil)
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

func TestReconcileKeepsSurvivorMovedFar(t *testing.T) {
	first := AgentFinding{
		Path: "src/a.go", Category: "bug", Title: "Mismo", ExistingCode: "dup()",
		StartLine: 10, EndLine: 10, Body: "first",
	}
	second := first
	second.StartLine, second.EndLine, second.Body = 40, 40, "second"

	known, _ := Reconcile(nil, AgentOutput{Findings: []AgentFinding{first, second}}, 1, true, 9, nil)
	if len(known) != 2 {
		t.Fatalf("first run upserts = %d, want 2", len(known))
	}
	var removed, survivor string
	for _, f := range known {
		if f.StartLine == 10 {
			removed = f.ID
		} else {
			survivor = f.ID
		}
	}

	// The survivor moves well past the context radius; it must keep its row
	// instead of reusing the removed occurrence's ordinal and comment.
	moved := second
	moved.StartLine, moved.EndLine = 60, 60
	upserts, resolved := Reconcile(known, AgentOutput{Findings: []AgentFinding{moved}}, 2, true, 9, nil)
	if len(upserts) != 1 {
		t.Fatalf("second run upserts = %d, want 1", len(upserts))
	}
	if upserts[0].ID != survivor {
		t.Fatalf("moved survivor ID = %q, want %q", upserts[0].ID, survivor)
	}
	if upserts[0].StartLine != 60 || upserts[0].EndLine != 60 {
		t.Fatalf("moved survivor range = %d-%d, want 60-60", upserts[0].StartLine, upserts[0].EndLine)
	}
	if len(resolved) != 1 || resolved[0] != removed {
		t.Fatalf("resolved = %v, want the removed occurrence %q", resolved, removed)
	}
}

func TestReconcileGivesNewOccurrenceFreshOrdinal(t *testing.T) {
	first := AgentFinding{
		Path: "src/a.go", Category: "bug", Title: "Mismo", ExistingCode: "dup()",
		StartLine: 10, EndLine: 10, Body: "first",
	}
	second := first
	second.StartLine, second.EndLine, second.Body = 40, 40, "second"

	known, _ := Reconcile(nil, AgentOutput{Findings: []AgentFinding{first, second}}, 1, true, 9, nil)
	if len(known) != 2 {
		t.Fatalf("first run upserts = %d, want 2", len(known))
	}
	knownIDs := make(map[string]string, len(known))
	for _, f := range known {
		knownIDs[f.Body] = f.ID
	}

	third := first
	third.StartLine, third.EndLine, third.Body = 25, 25, "third"
	upserts, resolved := Reconcile(known, AgentOutput{Findings: []AgentFinding{first, third, second}}, 2, true, 9, nil)
	if len(upserts) != 3 || len(resolved) != 0 {
		t.Fatalf("upserts/resolved = %d/%v, want 3/none", len(upserts), resolved)
	}
	byBody := make(map[string]string, len(upserts))
	for _, f := range upserts {
		byBody[f.Body] = f.ID
	}
	if byBody["first"] != knownIDs["first"] || byBody["second"] != knownIDs["second"] {
		t.Fatalf("existing occurrences changed ID: %v, want %v", byBody, knownIDs)
	}
	if byBody["third"] == "" || byBody["third"] == knownIDs["first"] || byBody["third"] == knownIDs["second"] {
		t.Fatalf("new occurrence ID %q stole an existing ordinal (known %v)", byBody["third"], knownIDs)
	}
}

func TestFingerprintNormalizesPath(t *testing.T) {
	f := AgentFinding{
		Path: "./src/a.go", Category: "bug", Title: "t",
		ExistingCode: "guard()", StartLine: 3, EndLine: 3,
	}
	want := Fingerprint(1, f, nil, 0)
	windows := f
	windows.Path = `src\a.go`
	doubled := f
	doubled.Path = "src//a.go"
	for _, other := range []AgentFinding{windows, doubled} {
		if got := Fingerprint(1, other, nil, 0); got != want {
			t.Errorf("path %q fingerprint = %q, want %q", other.Path, got, want)
		}
	}

	for path, want := range map[string]string{
		"./x": "x", " ./x ": "x", `dir\x`: "dir/x", "a//b": "a/b", ".": "",
	} {
		if got := normalizeCoveragePath(path); got != want {
			t.Errorf("normalizeCoveragePath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestFindingsInPathsNormalizesPath(t *testing.T) {
	open := []store.Finding{
		{ID: "f_kept", Path: `./src\a.go`},
		{ID: "f_outside", Path: "other.go"},
	}
	kept := findingsInPaths(open, map[string]bool{"src/a.go": true})
	if len(kept) != 1 || kept[0].ID != "f_kept" {
		t.Fatalf("findingsInPaths = %+v, want only the normalized match", kept)
	}
}

func TestCheckoutAnchors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "crlf.go"), []byte("a\r\nb\r\nc\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatalf("create directory: %v", err)
	}
	anchors := checkoutAnchors(dir)

	tests := []struct {
		name string
		path string
		want []string
	}{
		{name: "checkout file with CRLF", path: "crlf.go", want: []string{"a", "b", "c", ""}},
		{name: "missing file", path: "missing.go", want: nil},
		{name: "directory", path: "nested", want: nil},
		{name: "absolute path", path: filepath.Join(dir, "crlf.go"), want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := anchors(tt.path); !slices.Equal(got, tt.want) {
				t.Fatalf("anchors(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}

	// ExistingCode is the anchor even when the checkout cannot provide lines.
	if got := Anchor(AgentFinding{Path: "missing.go", ExistingCode: "  guard()  "}, nil); got != "guard()" {
		t.Errorf("Anchor with ExistingCode = %q, want guard()", got)
	}
}

func TestReconcileKeepsSurvivorOrdinal(t *testing.T) {
	first := AgentFinding{
		Path: "src/a.go", Category: "bug", Title: "Mismo", ExistingCode: "dup()",
		StartLine: 10, EndLine: 10, Body: "first",
	}
	second := first
	second.StartLine, second.EndLine, second.Body = 40, 40, "second"

	run := func(open []store.Finding, findings []AgentFinding, runID uint) ([]store.Finding, []string) {
		t.Helper()
		return Reconcile(open, AgentOutput{Findings: findings}, runID, true, 9, nil)
	}

	known, _ := run(nil, []AgentFinding{first, second}, 1)
	if len(known) != 2 {
		t.Fatalf("first run upserts = %d, want 2", len(known))
	}
	var removed, survivor string
	for _, f := range known {
		if f.StartLine == 10 {
			removed = f.ID
		} else {
			survivor = f.ID
		}
	}

	// One of the two identical occurrences disappears: the survivor must keep
	// its ID and the removed occurrence must be resolved, not repointed.
	upserts, resolved := run(known, []AgentFinding{second}, 2)
	if len(upserts) != 1 {
		t.Fatalf("second run upserts = %d, want 1", len(upserts))
	}
	if upserts[0].ID != survivor {
		t.Fatalf("survivor ID = %q, want %q", upserts[0].ID, survivor)
	}
	if len(resolved) != 1 || resolved[0] != removed {
		t.Fatalf("resolved = %v, want the removed occurrence %q", resolved, removed)
	}
}

func TestReconcileAndStoreKeepFindingsPerPullRequest(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	installation := store.Installation{ID: 901, AccountLogin: "acme", AccountType: "Organization"}
	if err := s.DB.WithContext(ctx).Create(&installation).Error; err != nil {
		t.Fatalf("create installation: %v", err)
	}
	repo := store.Repository{
		ID: 902, InstallationID: installation.ID, FullName: "acme/finding-identity", Enabled: true,
	}
	if err := s.DB.WithContext(ctx).Create(&repo).Error; err != nil {
		t.Fatalf("create repository: %v", err)
	}
	prs := []store.PullRequest{
		{RepositoryID: repo.ID, Number: 1, State: "open", GitHubUpdatedAt: now},
		{RepositoryID: repo.ID, Number: 2, State: "open", GitHubUpdatedAt: now},
	}
	for i := range prs {
		if err := s.DB.WithContext(ctx).Create(&prs[i]).Error; err != nil {
			t.Fatalf("create pull request %d: %v", i+1, err)
		}
	}

	finding := AgentFinding{
		Path: "./src/a.go", Category: "bug", Title: "Misma firma",
		ExistingCode: "guard()", StartLine: 3, EndLine: 3,
	}
	ids := make([]string, len(prs))
	for i, pr := range prs {
		upserts, _ := Reconcile(nil, AgentOutput{Findings: []AgentFinding{finding}}, 1, true, pr.ID, nil)
		if len(upserts) != 1 {
			t.Fatalf("PR %d upserts = %d, want 1", pr.ID, len(upserts))
		}
		saved, err := s.UpsertFinding(ctx, upserts[0])
		if err != nil {
			t.Fatalf("persist PR %d finding: %v", pr.ID, err)
		}
		ids[i] = saved.ID
	}
	if ids[0] == ids[1] {
		t.Fatalf("both pull requests persisted the same finding ID %q", ids[0])
	}

	for i, pr := range prs {
		findings, err := s.FindingsForPR(ctx, pr.ID)
		if err != nil {
			t.Fatalf("FindingsForPR(%d): %v", pr.ID, err)
		}
		if len(findings) != 1 || findings[0].ID != ids[i] || findings[0].PullRequestID != pr.ID {
			t.Fatalf("PR %d findings = %+v, want only its own row %q", pr.ID, findings, ids[i])
		}
	}
}

func linesProvider(lines []string) AnchorProvider {
	return func(string) []string { return lines }
}

func idSet(findings []store.Finding) map[string]bool {
	ids := make(map[string]bool, len(findings))
	for _, f := range findings {
		ids[f.ID] = true
	}
	return ids
}

func equalSets(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for id := range a {
		if !b[id] {
			return false
		}
	}
	return true
}

func TestFingerprintUsesCitationsWithoutCitationLines(t *testing.T) {
	base := AgentFinding{
		Path: "src/a.go", Category: "design", Title: "Boundary", StartLine: 10, EndLine: 10,
		Scope: store.FindingScopeFiles,
		Citations: []store.FindingCitation{
			{Path: "src/a.go", StartLine: 10, EndLine: 10},
			{Path: "src/b.go", StartLine: 20, EndLine: 20},
		},
	}
	single := base
	single.Scope = store.FindingScopeFile
	single.Citations = base.Citations[:1]

	if Fingerprint(9, base, nil, 0) == Fingerprint(9, single, nil, 0) {
		t.Fatal("a finding that spans files shares the identity of a single-file finding")
	}

	reordered := base
	reordered.Citations = []store.FindingCitation{base.Citations[1], base.Citations[0]}
	if Fingerprint(9, base, nil, 0) != Fingerprint(9, reordered, nil, 0) {
		t.Fatal("fingerprint depends on citation order")
	}

	// A push shifts every cited line; the finding keeps its identity so its
	// comment, thread and audit follow it.
	shifted := base
	shifted.StartLine, shifted.EndLine = 14, 14
	shifted.Citations = []store.FindingCitation{
		{Path: "src/a.go", StartLine: 14, EndLine: 14},
		{Path: "src/b.go", StartLine: 26, EndLine: 26},
	}
	if Fingerprint(9, base, nil, 0) != Fingerprint(9, shifted, nil, 0) {
		t.Fatal("fingerprint changed when only citation lines moved")
	}
}

func TestReconcileKeepsMultiFileFindingIdentityAndAudit(t *testing.T) {
	firstFinding := AgentFinding{
		Path: "internal/a.go", Severity: "major", Category: "design", Title: "Divide el límite",
		StartLine: 10, EndLine: 10, Scope: store.FindingScopeFiles,
		Citations: []store.FindingCitation{
			{Path: "internal/a.go", StartLine: 10, EndLine: 10},
			{Path: "internal/b.go", StartLine: 20, EndLine: 20, Side: store.FindingCitationHead},
		},
	}
	first, _ := Reconcile(nil, AgentOutput{Findings: []AgentFinding{firstFinding}}, 1, true, 9, nil)
	if len(first) != 1 {
		t.Fatalf("first upserts = %d, want 1", len(first))
	}
	first[0].TriageVerdict = store.VerdictConfirmed
	first[0].TriagedBy = "alice"
	first[0].TriageSource = store.TriageSourceHuman

	shifted := firstFinding
	shifted.StartLine, shifted.EndLine = 14, 14
	shifted.Citations = []store.FindingCitation{
		{Path: "internal/a.go", StartLine: 14, EndLine: 14},
		{Path: "internal/b.go", StartLine: 26, EndLine: 26},
	}
	second, _ := Reconcile(first, AgentOutput{Findings: []AgentFinding{shifted}}, 2, true, 9, nil)
	if len(second) != 1 {
		t.Fatalf("second upserts = %d, want 1", len(second))
	}
	if second[0].ID != first[0].ID {
		t.Fatalf("identity changed across runs: %s != %s", second[0].ID, first[0].ID)
	}
	if second[0].FirstSeenRunID != 1 || second[0].LastSeenRunID != 2 {
		t.Errorf("run traceability = %d/%d, want 1/2", second[0].FirstSeenRunID, second[0].LastSeenRunID)
	}
	if second[0].TriageVerdict != store.VerdictConfirmed || second[0].TriagedBy != "alice" ||
		second[0].TriageSource != store.TriageSourceHuman {
		t.Errorf("human audit was lost: %+v", second[0])
	}
	if second[0].Scope != store.FindingScopeFiles {
		t.Errorf("scope = %q, want files", second[0].Scope)
	}
	citations := store.DecodeFindingCitations(second[0].Citations)
	if len(citations) != 2 || citations[0].StartLine != 14 || citations[1].StartLine != 26 {
		t.Errorf("citations = %+v, want the shifted evidence", citations)
	}
}

func TestReconcileKeepsMultiFileIdentityThroughDisputeAndResolution(t *testing.T) {
	reported := AgentFinding{
		Path: "internal/a.go", Severity: "major", Category: "design", Title: "Acopla dos módulos",
		StartLine: 10, EndLine: 10, Scope: store.FindingScopeFiles,
		Citations: []store.FindingCitation{
			{Path: "internal/a.go", StartLine: 10, EndLine: 10},
			{Path: "internal/b.go", StartLine: 20, EndLine: 20},
		},
	}
	first, _ := Reconcile(nil, AgentOutput{Findings: []AgentFinding{reported}}, 1, true, 9, nil)
	if len(first) != 1 {
		t.Fatalf("first upserts = %d, want 1", len(first))
	}
	disputedAt := time.Unix(1700000000, 0).UTC()
	first[0].TriageVerdict = store.VerdictFalsePositive
	first[0].TriagedBy = "bob"
	first[0].TriagedAt = &disputedAt
	first[0].TriageSource = store.TriageSourceHuman

	shifted := reported
	shifted.StartLine, shifted.EndLine = 12, 12
	shifted.Citations = []store.FindingCitation{
		{Path: "internal/a.go", StartLine: 12, EndLine: 12},
		{Path: "internal/b.go", StartLine: 22, EndLine: 22},
	}
	second, resolved := Reconcile(first, AgentOutput{Findings: []AgentFinding{shifted}}, 2, true, 9, nil)
	if len(second) != 1 || len(resolved) != 0 {
		t.Fatalf("upserts/resolved = %d/%v, want 1/none", len(second), resolved)
	}
	if second[0].ID != first[0].ID {
		t.Fatalf("identity changed across the dispute: %s != %s", second[0].ID, first[0].ID)
	}
	if second[0].TriageVerdict != store.VerdictFalsePositive || second[0].TriagedBy != "bob" ||
		second[0].TriageSource != store.TriageSourceHuman ||
		second[0].TriagedAt == nil || !second[0].TriagedAt.Equal(disputedAt) {
		t.Errorf("dispute audit was lost: %+v", second[0])
	}

	// A complete run that no longer reports it resolves the same row, not a
	// duplicate keyed by the shifted lines.
	third, resolvedIDs := Reconcile(second, AgentOutput{}, 3, true, 9, nil)
	if len(third) != 0 || len(resolvedIDs) != 1 || resolvedIDs[0] != second[0].ID {
		t.Fatalf("upserts/resolved = %d/%v, want none/[%s]", len(third), resolvedIDs, second[0].ID)
	}
}

func TestNativeFindingsCarryHeadCitation(t *testing.T) {
	findings := nativeFindings([]nativeComment{{
		Path: "src/a.go", Content: "finding", StartLine: 5, EndLine: 7,
	}})
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(findings))
	}
	f := findings[0]
	if f.Scope != store.FindingScopeFile {
		t.Errorf("scope = %q, want file", f.Scope)
	}
	if len(f.Citations) != 1 {
		t.Fatalf("citations = %+v, want one head-side citation", f.Citations)
	}
	c := f.Citations[0]
	if c.Path != "src/a.go" || c.StartLine != 5 || c.EndLine != 7 || c.Side != store.FindingCitationHead {
		t.Errorf("citation = %+v, want src/a.go:5-7 on the head", c)
	}
}

func TestReconcileKeepsIdentityWhenCitationRestatesAnchor(t *testing.T) {
	legacy := AgentFinding{
		Path: "src/a.go", Severity: "major", Category: "bug", Title: "Mismo",
		StartLine: 3, EndLine: 3,
	}
	withCitation := legacy
	withCitation.Scope = store.FindingScopeFile
	withCitation.Citations = []store.FindingCitation{{
		Path: "src/a.go", StartLine: 3, EndLine: 3, Side: store.FindingCitationHead,
	}}

	first, _ := Reconcile(nil, AgentOutput{Findings: []AgentFinding{legacy}}, 1, true, 9, nil)
	second, _ := Reconcile(first, AgentOutput{Findings: []AgentFinding{withCitation}}, 2, true, 9, nil)
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("upserts = %d/%d, want 1/1", len(first), len(second))
	}
	if first[0].ID != second[0].ID {
		t.Fatalf("adding the canonical citation re-keyed the finding: %s != %s", second[0].ID, first[0].ID)
	}
	if citations := store.DecodeFindingCitations(second[0].Citations); len(citations) != 1 {
		t.Errorf("citations = %+v, want the canonical citation persisted", citations)
	}
}

func TestDedupeNearbyFindingsResyncsCanonicalCitation(t *testing.T) {
	first := AgentFinding{
		Path: "big.go", Category: "bug", Title: "Mismo problema", Body: "primer cuerpo",
		StartLine: 10, EndLine: 10, Scope: store.FindingScopeFile,
		Citations: []store.FindingCitation{{Path: "big.go", StartLine: 10, EndLine: 10, Side: store.FindingCitationHead}},
	}
	second := AgentFinding{
		Path: "big.go", Category: "bug", Title: "Mismo problema", Body: "primer cuerpo",
		StartLine: 9, EndLine: 12, Scope: store.FindingScopeFile,
		Citations: []store.FindingCitation{{Path: "big.go", StartLine: 9, EndLine: 12, Side: store.FindingCitationHead}},
	}
	got := dedupeNearbyFindings([]AgentFinding{first, second})
	if len(got) != 1 {
		t.Fatalf("findings = %d, want the merged occurrence", len(got))
	}
	citations := got[0].Citations
	if len(citations) != 1 || citations[0].StartLine != 9 || citations[0].EndLine != 12 {
		t.Errorf("citations = %+v, want the canonical citation on the merged range", citations)
	}
	if evidence := identityCitations(got[0]); len(evidence) != 0 {
		t.Errorf("identity evidence = %+v, a merged canonical citation must not extend identity", evidence)
	}
}

func TestDedupeNearbyFindingsKeepsRicherCitations(t *testing.T) {
	canonical := AgentFinding{
		Path: "big.go", Category: "design", Title: "Límite compartido", Body: "cuerpo",
		StartLine: 10, EndLine: 10, Scope: store.FindingScopeFile,
		Citations: []store.FindingCitation{{Path: "big.go", StartLine: 10, EndLine: 10, Side: store.FindingCitationHead}},
	}
	richer := AgentFinding{
		Path: "big.go", Category: "design", Title: "Límite compartido", Body: "cuerpo",
		StartLine: 10, EndLine: 10, Scope: store.FindingScopeFiles,
		Citations: []store.FindingCitation{
			{Path: "big.go", StartLine: 10, EndLine: 10, Side: store.FindingCitationHead},
			{Path: "other.go", StartLine: 20, EndLine: 20, Side: store.FindingCitationHead},
		},
	}
	got := dedupeNearbyFindings([]AgentFinding{canonical, richer})
	if len(got) != 1 {
		t.Fatalf("findings = %d, want the merged occurrence", len(got))
	}
	if len(got[0].Citations) != 2 || got[0].Scope != store.FindingScopeFiles {
		t.Errorf("merged finding = %+v, want both citations and files scope", got[0])
	}
}

func TestDedupeNearbyFindingsKeepsEarliestRange(t *testing.T) {
	makeFinding := func(start int) AgentFinding {
		return AgentFinding{
			Path: "big.go", Category: "bug", Title: "Mismo problema", Body: "cuerpo",
			StartLine: start, EndLine: start, Scope: store.FindingScopeFile,
			Citations: []store.FindingCitation{{
				Path: "big.go", StartLine: start, EndLine: start, Side: store.FindingCitationHead,
			}},
		}
	}
	got := dedupeNearbyFindings([]AgentFinding{makeFinding(20), makeFinding(21)})
	if len(got) != 1 {
		t.Fatalf("findings = %d, want the merged occurrence", len(got))
	}
	if got[0].StartLine != 20 || got[0].EndLine != 20 {
		t.Errorf("range = %d-%d, want the earliest range 20-20", got[0].StartLine, got[0].EndLine)
	}
	if len(got[0].Citations) != 1 || got[0].Citations[0].StartLine != 20 || got[0].Citations[0].EndLine != 20 {
		t.Errorf("citations = %+v, want the canonical citation on 20-20", got[0].Citations)
	}
}
