package review

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestVerifyPolicyFillsLegacyDefaults(t *testing.T) {
	policy := verifyPolicy(store.QAPolicy{})
	if policy.Mode != store.AutoTriageOff ||
		policy.MaxFindings != store.DefaultVerifyMaxFindings ||
		policy.MaxFiles != store.DefaultVerifyMaxFiles ||
		policy.TokenShare != store.DefaultVerifyTokenShare ||
		policy.TimeShare != store.DefaultVerifyTimeShare ||
		policy.TokenMax != store.DefaultVerifyTokenMax {
		t.Fatalf("policy = %+v, want the documented defaults", policy)
	}
}

func TestSelectVerifyFindingsPrioritizesMinorAndCaps(t *testing.T) {
	findings := []store.Finding{
		{ID: "major", Path: "a.go", Severity: "major", StartLine: 1},
		{ID: "nit", Path: "b.go", Severity: "nit", StartLine: 9},
		{ID: "minor", Path: "c.go", Severity: "minor", StartLine: 5},
		{ID: "minor-too", Path: "d.go", Severity: "minor", StartLine: 7},
	}
	selected, paths := selectVerifyFindings(findings, VerifyPolicy{MaxFindings: 2, MaxFiles: 2})
	if len(selected) != 2 || selected[0].ID != "minor" || selected[1].ID != "minor-too" {
		t.Fatalf("selected = %+v, want the two minors first", selected)
	}
	if len(paths) != 2 || paths[0] != "c.go" || paths[1] != "d.go" {
		t.Fatalf("paths = %v, want the selected files", paths)
	}

	selected, paths = selectVerifyFindings(findings, VerifyPolicy{MaxFindings: 4, MaxFiles: 1})
	if len(selected) != 1 || len(paths) != 1 {
		t.Fatalf("selected/paths = %+v/%v, want one file only", selected, paths)
	}
}

func TestReusedVerdictRequiresSameHeadAndCitedCode(t *testing.T) {
	raw, err := json.Marshal(Verification{Verdict: VerdictConfirm, HeadSHA: "head-1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	prev := store.Finding{Verification: raw, ExistingCode: "old code"}
	current := store.Finding{ExistingCode: "old code"}

	if v, ok := reusedVerdict(prev, current, "head-1"); !ok || v.Verdict != VerdictConfirm {
		t.Fatalf("reused = %+v/%v, want the stored confirm reused", v, ok)
	}
	if _, ok := reusedVerdict(prev, current, "head-2"); ok {
		t.Error("a verdict from another head must not be reused")
	}
	current.ExistingCode = "new code"
	if _, ok := reusedVerdict(prev, current, "head-1"); ok {
		t.Error("a verdict whose cited code changed must not be reused")
	}
	if _, ok := reusedVerdict(store.Finding{}, current, "head-1"); ok {
		t.Error("a finding without a stored verdict cannot be reused")
	}
}

func TestReadCitedBlobsReadsBothSides(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "qa@example.com")
	runGit(t, dir, "config", "user.name", "QA")
	runGit(t, dir, "config", "core.autocrlf", "false")
	if err := os.WriteFile(filepath.Join(dir, "gone.go"), []byte("package a\nfunc removed() {}\n"), 0o644); err != nil {
		t.Fatalf("write gone.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kept.go"), []byte("package a\nfunc kept() {}\n"), 0o644); err != nil {
		t.Fatalf("write kept.go: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o755); err != nil {
		t.Fatalf("create pkg dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "util.go"), []byte("package pkg\n"), 0o644); err != nil {
		t.Fatalf("write pkg/util.go: %v", err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base")
	baseSHA := runGit(t, dir, "rev-parse", "HEAD")
	if err := os.Remove(filepath.Join(dir, "gone.go")); err != nil {
		t.Fatalf("remove gone.go: %v", err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "delete gone.go")

	findings := []store.Finding{
		{
			ID: "base", Path: "gone.go", StartLine: 2, EndLine: 2,
			Citations: store.EncodeFindingCitations([]store.FindingCitation{{
				Path: "gone.go", StartLine: 2, EndLine: 2, Side: store.FindingCitationBase,
			}}),
		},
		{ID: "head", Path: "kept.go", StartLine: 2, EndLine: 2},
		{
			ID: "dir", Path: "pkg", StartLine: 1, EndLine: 1,
			Citations: store.EncodeFindingCitations([]store.FindingCitation{{
				Path: "pkg", StartLine: 1, EndLine: 1, Side: store.FindingCitationBase,
			}}),
		},
	}
	blobs := readCitedBlobs(context.Background(), dir, baseSHA, findings)
	if got := string(blobs.base["gone.go"]); got != "package a\nfunc removed() {}\n" {
		t.Errorf("base blob = %q, want the deleted file content", got)
	}
	if _, ok := blobs.head["gone.go"]; ok {
		t.Error("a file deleted at head was read from the head checkout")
	}
	if got := string(blobs.head["kept.go"]); got != "package a\nfunc kept() {}\n" {
		t.Errorf("head blob = %q, want the checked-out content", got)
	}
	if _, ok := blobs.base["pkg"]; ok {
		t.Error("a directory citation was read as a blob")
	}
	if _, err := readBaseBlob(context.Background(), dir, baseSHA, "pkg"); err == nil {
		t.Error("readBaseBlob accepted a directory, want cat-file to reject the tree")
	}
}

func TestCleanRepoPathRejectsEscapingPaths(t *testing.T) {
	for _, bad := range []string{"", "../a.go", `..\a.go`, "/a.go", `\a.go`, "../dir/a.go"} {
		if _, err := cleanRepoPath(bad); err == nil {
			t.Errorf("path %q was accepted, want rejected", bad)
		}
	}
	if abs, err := filepath.Abs("a.go"); err != nil {
		t.Fatalf("absolute path: %v", err)
	} else if _, err := cleanRepoPath(abs); err == nil {
		t.Errorf("absolute path %q was accepted, want rejected", abs)
	}
	if got, err := cleanRepoPath(`dir\a.go`); err != nil || got != "dir/a.go" {
		t.Errorf("cleanRepoPath() = %q (err=%v), want dir/a.go", got, err)
	}
	if got, err := cleanRepoPath("./a.go"); err != nil || got != "a.go" {
		t.Errorf("cleanRepoPath() = %q (err=%v), want a.go", got, err)
	}
}

func TestReadRepoFileRejectsEscapingPaths(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "a.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := readRepoFile(workDir, "a.go"); err != nil {
		t.Fatalf("read cited file: %v", err)
	}
	for _, path := range []string{"", "../a.go", filepath.Join(workDir, "a.go"), "."} {
		if _, err := readRepoFile(workDir, path); err == nil {
			t.Errorf("path %q was accepted, want rejected", path)
		}
	}

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.go")
	if err := os.WriteFile(secret, []byte("token\n"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(secret, filepath.Join(workDir, "escape.go")); err != nil {
		t.Skipf("symlinks unavailable on this host: %v", err)
	}
	if _, err := readRepoFile(workDir, "escape.go"); err == nil {
		t.Error("a symlink out of the checkout was followed, want rejected")
	}
	// A symlink that stays inside the checkout is ordinary repository content.
	if err := os.Symlink(filepath.Join(workDir, "a.go"), filepath.Join(workDir, "inside.go")); err != nil {
		t.Fatalf("symlink inside the checkout: %v", err)
	}
	if content, err := readRepoFile(workDir, "inside.go"); err != nil || string(content) != "package main\n" {
		t.Errorf("inside symlink = %q (err=%v), want the target content", content, err)
	}
}

func TestReserveVerificationHoldsBackTheVerifierShare(t *testing.T) {
	policy := VerifyPolicy{Mode: store.AutoTriageDryRun, TokenShare: 20, TimeShare: 15, TokenMax: 50000}
	p := store.QAPolicy{ReviewTokenBudget: 100000, AutoTriageMode: store.AutoTriageDryRun}

	reserved := reserveVerification(p, policy, 10*time.Minute)
	if !reserved.enabled {
		t.Fatal("reservation = disabled, want it enabled when auto-triage is on")
	}
	if reserved.mainTokens != 80000 || reserved.tokens != 20000 {
		t.Errorf("main/reserved = %d/%d, want 80000/20000", reserved.mainTokens, reserved.tokens)
	}
	if reserved.mainTokens+reserved.tokens != p.ReviewTokenBudget {
		t.Errorf("main+verify = %d, want the configured %d",
			reserved.mainTokens+reserved.tokens, p.ReviewTokenBudget)
	}
	if reserved.timeout != 90*time.Second || reserved.mainTime != 8*time.Minute+30*time.Second {
		t.Errorf("main/verify time = %s/%s, want 8m30s/90s", reserved.mainTime, reserved.timeout)
	}

	off := reserveVerification(p, VerifyPolicy{Mode: store.AutoTriageOff}, 10*time.Minute)
	if off.enabled || off.mainTokens != 0 || off.tokens != 0 {
		t.Errorf("reservation with auto-triage off = %+v, want the full budget for the main pass", off)
	}
}

func TestReserveVerificationReturnsUnusableSharesToTheMainPass(t *testing.T) {
	policy := VerifyPolicy{Mode: store.AutoTriageApply, TokenShare: 20, TimeShare: 15, TokenMax: 50000}
	p := store.QAPolicy{ReviewTokenBudget: 40000, AutoTriageMode: store.AutoTriageApply}

	// 20% of 40000 is below the minimum: reserving it would only weaken the
	// main pass because the verifier is skipped anyway. Both quotas return.
	reserved := reserveVerification(p, policy, 10*time.Minute)
	if reserved.mainTokens != 40000 || reserved.tokens != 8000 {
		t.Errorf("main/reserved = %d/%d, want the unusable 8000 back with the main pass",
			reserved.mainTokens, reserved.tokens)
	}
	if reserved.mainTime != 10*time.Minute {
		t.Errorf("main time = %s, want the full run when the verifier cannot start", reserved.mainTime)
	}

	// 15% of 4m is below the minimum timeout: nothing is held back either.
	short := reserveVerification(p, policy, 4*time.Minute)
	if short.mainTime != 4*time.Minute || short.timeout != 36*time.Second {
		t.Errorf("main/verify time = %s/%s, want the unusable 36s back with the main pass",
			short.mainTime, short.timeout)
	}
	if short.mainTokens != 40000 {
		t.Errorf("main tokens = %d, want the full budget when the verifier cannot start", short.mainTokens)
	}
}

func TestVerifyPolicyClampsTheSharesBelowOneHundred(t *testing.T) {
	policy := verifyPolicy(store.QAPolicy{
		AutoTriageMode:   store.AutoTriageApply,
		VerifyTokenShare: 100,
		VerifyTimeShare:  100,
	})
	if policy.TokenShare != 99 || policy.TimeShare != 99 {
		t.Errorf("shares = %d/%d, want both clamped to 99", policy.TokenShare, policy.TimeShare)
	}
	if mainTokens, _ := verifyAllowance(store.QAPolicy{ReviewTokenBudget: 1000}, policy); mainTokens == 0 {
		t.Error("main allowance = 0, want a positive cap so OCR does not run uncapped")
	}
}

func TestVerifyTokensWithoutRunBudgetKeepsTheFixedCap(t *testing.T) {
	policy := VerifyPolicy{Mode: store.AutoTriageApply, TokenShare: 20, TokenMax: 50000}

	mainTokens, tokens := verifyAllowance(store.QAPolicy{}, policy)
	if mainTokens != 0 || tokens != 50000 {
		t.Errorf("allowance = %d/%d, want the uncapped main pass and the fixed 50000",
			mainTokens, tokens)
	}
	if got := verifyBudgetFrom(0, tokens, 900000, true); got != 50000 {
		t.Errorf("verify tokens = %d, want the fixed cap 50000", got)
	}
	if got := verifyBudgetFrom(0, tokens, 0, false); got != 50000 {
		t.Errorf("verify tokens with unknown usage = %d, want the fixed cap 50000", got)
	}
}

func TestVerifyTokensChargesUnknownUsageToTheMainAllowance(t *testing.T) {
	cases := []struct {
		name       string
		used       int64
		usageKnown bool
		want       int64
	}{
		{"known zero usage", 0, true, 20000},
		// The run fixture's main pass: 150 tokens of 100000 must still leave
		// the whole reserved share for the verifier.
		{"known small usage keeps the reservation", 150, true, 20000},
		{"known usage over the reservation", 90000, true, 10000},
		{"known usage over the whole budget", 110000, true, 0},
		// Unknown usage is charged as the whole main allowance, never as zero.
		{"unknown usage cannot unlock the main allowance", 0, false, 20000},
		{"unknown usage is bounded by the reservation", 999999, false, 20000},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := verifyBudgetFrom(100000, 20000, tt.used, tt.usageKnown); got != tt.want {
				t.Errorf("verify tokens = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestChunkAllowanceDoesNotSpendUnknownUsage(t *testing.T) {
	if got := chunkAllowance(120, 100, true); got != 20 {
		t.Errorf("remaining = %d, want 20", got)
	}
	if got := chunkAllowance(120, 120, true); got != 0 {
		t.Errorf("remaining = %d, want 0 when the main pass spent it all", got)
	}
	// A run without usage data may have spent everything: no budget is assumed
	// left for the chunks.
	if got := chunkAllowance(120, 0, false); got != 0 {
		t.Errorf("remaining with unknown usage = %d, want 0", got)
	}
	if got := chunkAllowance(0, 0, true); got != 0 {
		t.Errorf("remaining without a run budget = %d, want 0", got)
	}
}

func TestVerificationSkipNamesTheMissingResource(t *testing.T) {
	if got := verificationSkip(30*time.Second, 50000); got != "insufficient_time" {
		t.Errorf("skip = %q, want insufficient_time", got)
	}
	if got := verificationSkip(time.Minute, 50000); got != "" {
		t.Errorf("skip = %q, want the minimum timeout accepted", got)
	}
	if got := verificationSkip(0, verifyMinBudget-1); got != "insufficient_budget" {
		t.Errorf("skip = %q, want insufficient_budget", got)
	}
	if got := verificationSkip(0, verifyMinBudget); got != "" {
		t.Errorf("skip = %q, want the minimum budget accepted", got)
	}
}

func TestVerifyBudgetKeepsTheFixtureReservation(t *testing.T) {
	p := store.QAPolicy{ReviewTokenBudget: 100000, AutoTriageMode: store.AutoTriageDryRun}
	policy := verifyPolicy(p)
	reserved := reserveVerification(p, policy, 10*time.Minute)
	if !reserved.enabled || reserved.tokens != 20000 || reserved.mainTokens != 80000 {
		t.Fatalf("reservation = %+v, want 80000 for main and 20000 reserved", reserved)
	}

	budget := verifyBudgetFrom(p.ReviewTokenBudget, reserved.tokens, 150, true)
	args := buildScanArgs(verifyInvocation{
		RepoDir: "/repo", Paths: []string{"auth.go"}, RulePath: "/repo/rule.json",
		MaxTokens: 1000, TokenBudget: budget,
	})
	if got, ok := argValue(args, "--max-tokens-budget"); !ok || got != "20000" {
		t.Errorf("scan --max-tokens-budget = %q (present=%v), want the reserved 20000 in %q",
			got, ok, args)
	}
}

func TestVerifyContextExpiresTheStalledVerifier(t *testing.T) {
	parent := context.Background()
	verifyCtx, cancel := verifyContext(parent, 20*time.Millisecond)
	defer cancel()
	select {
	case <-verifyCtx.Done():
		if !errors.Is(verifyCtx.Err(), context.DeadlineExceeded) {
			t.Errorf("error = %v, want the verifier deadline to expire", verifyCtx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("the verifier context outlived its reserved time")
	}

	// Without a configured timeout the parent is the only deadline there is.
	plain, cancelPlain := verifyContext(parent, 0)
	defer cancelPlain()
	if plain != parent {
		t.Error("a zero timeout replaced the parent context")
	}
}

func TestMarkUnverifiedKeepsStatusAndRecordsTheReason(t *testing.T) {
	findings := []store.Finding{
		{ID: "stored", Status: store.FindingOpen, Severity: "major", Path: "a.go", StartLine: 1},
		{ID: "pending", Status: store.FindingOpen, Severity: "minor", Path: "a.go", StartLine: 2},
	}
	verdicts := map[string]Verification{"stored": {Verdict: VerdictConfirm, HeadSHA: "head-1"}}
	markUnverified(verdicts, findings, "head-1", unverifiedReason("insufficient_budget"))

	if verdicts["stored"].Verdict != VerdictConfirm {
		t.Errorf("stored verdict = %+v, want the reused confirm untouched", verdicts["stored"])
	}
	decisions := Decide(findings, verdicts, evidenceBlobs{}, "head-1",
		VerifyPolicy{Mode: store.AutoTriageApply, AutoWithdraw: true})
	var pending Decision
	for _, d := range decisions {
		if d.FindingID == "pending" {
			pending = d
		}
	}
	if pending.Status != store.FindingOpen || pending.Apply {
		t.Errorf("decision = %+v, want the original status kept and no change applied", pending)
	}
	if pending.Verification.Reason != "verificación omitida: presupuesto insuficiente" {
		t.Errorf("reason = %q, want the skip recorded on the finding", pending.Verification.Reason)
	}
}
