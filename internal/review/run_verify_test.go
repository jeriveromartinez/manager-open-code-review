package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestVerifyPolicyFillsLegacyDefaults(t *testing.T) {
	policy := verifyPolicy(store.QAPolicy{})
	if policy.Mode != store.AutoTriageOff ||
		policy.MaxFindings != defaultVerifyMaxFindings ||
		policy.MaxFiles != defaultVerifyMaxFiles ||
		policy.TokenShare != defaultVerifyTokenShare ||
		policy.TimeShare != defaultVerifyTimeShare ||
		policy.TokenMax != defaultVerifyTokenMax {
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
}

func TestVerifyBudgetSharesAndFallsBackToTheFixedCap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	policy := VerifyPolicy{TokenShare: 20, TimeShare: 15, TokenMax: 50000}

	timeout, budget := verifyBudget(ctx, store.QAPolicy{ReviewTokenBudget: 200000}, policy)
	if budget != 40000 {
		t.Errorf("budget = %d, want 20%% of 200000", budget)
	}
	if timeout < 80*time.Second || timeout > 100*time.Second {
		t.Errorf("timeout = %s, want 15%% of the remaining time", timeout)
	}

	if _, budget = verifyBudget(ctx, store.QAPolicy{}, policy); budget != 50000 {
		t.Errorf("budget = %d, want the fixed VerifyTokenMax when the run budget is disabled", budget)
	}
}
