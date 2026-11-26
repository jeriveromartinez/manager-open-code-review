package review

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestArchitectureAnalysisRequestedOnlyByExplicitSignal(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		err     error
		signals []string
		want    bool
	}{
		{"observation with signal", store.ArchitectureAnalysisObservation, nil, []string{"persistence"}, true},
		{"observation without signal", store.ArchitectureAnalysisObservation, nil, nil, false},
		{"off with signal", store.ArchitectureAnalysisOff, nil, []string{"persistence"}, false},
		{"inventory failure", store.ArchitectureAnalysisObservation, errTestInventory, []string{"persistence"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := architectureAnalysisRequested(tc.mode, tc.err, tc.signals); got != tc.want {
				t.Errorf("requested = %v, want %v", got, tc.want)
			}
		})
	}
}

var errTestInventory = errors.New("inventory failed")

func TestBuildReviewArgsRequestsAnalysisOnlyWhenEnabled(t *testing.T) {
	base := nativeInvocation{
		RepoDir: "/repo", BaseRef: "base", HeadRef: "head", BackgroundFile: "bg.md",
		Profile: store.LLMProfile{Model: "model"}, Effort: "low", MaxTokens: 1000,
	}
	args := buildReviewArgs(base)
	if containsArgValue(args, "--analysis") || containsArgValue(args, "--analysis-max-tokens-budget") {
		t.Fatalf("analysis flags present without a request: %v", args)
	}

	withAnalysis := base
	withAnalysis.Analysis = true
	withAnalysis.TokenBudget = 12000
	withAnalysis.AnalysisTokenBudget = 12000
	args = buildReviewArgs(withAnalysis)
	if !containsArgValue(args, "--analysis") {
		t.Fatalf("--analysis missing: %v", args)
	}
	// OCR budgets the discarded re-review and the analysis independently, so
	// the phase must cap both flags.
	if !containsArgPair(args, "--max-tokens-budget", "12000") {
		t.Fatalf("discarded re-review token budget missing: %v", args)
	}
	if !containsArgPair(args, "--analysis-max-tokens-budget", "12000") {
		t.Fatalf("analysis token budget missing: %v", args)
	}
}

func TestDetectArchitectureSignals(t *testing.T) {
	cases := []struct {
		name  string
		files []changedFile
		stats architectureChangeStats
		want  []string
		not   []string
	}{
		{
			name:  "low risk local change does not activate analysis",
			files: []changedFile{{Path: "src/util.go", Kind: changedModified}},
			stats: architectureChangeStats{Files: 1, AddedLines: 5, RemovedLines: 2},
		},
		{
			name: "nested paths in one source tree stay one module",
			files: []changedFile{
				{Path: "src/a.go", Kind: changedModified},
				{Path: "src/sub/b.go", Kind: changedModified},
			},
			stats: architectureChangeStats{Files: 2},
		},
		{
			name: "non-code root does not create a boundary with a code module",
			files: []changedFile{
				{Path: "docs/readme.md", Kind: changedModified},
				{Path: "internal/review/a.go", Kind: changedModified},
			},
			stats: architectureChangeStats{Files: 2},
		},
		{
			name:  "migration activates persistence",
			files: []changedFile{{Path: "internal/store/migrations/00001_init.sql", Kind: changedAdded}},
			stats: architectureChangeStats{Files: 1, AddedLines: 20},
			want:  []string{architectureSignalPersistence},
		},
		{
			name:  "public contract activates interface",
			files: []changedFile{{Path: "api/openapi.yaml", Kind: changedModified}},
			stats: architectureChangeStats{Files: 1},
			want:  []string{architectureSignalInterface},
		},
		{
			name:  "auth path activates permissions",
			files: []changedFile{{Path: "internal/auth/token.go", Kind: changedModified}},
			stats: architectureChangeStats{Files: 1},
			want:  []string{architectureSignalPermissions},
		},
		{
			name:  "docker file activates topology",
			files: []changedFile{{Path: "Dockerfile", Kind: changedModified}},
			stats: architectureChangeStats{Files: 1},
			want:  []string{architectureSignalTopology},
		},
		{
			name:  "settings activate configuration",
			files: []changedFile{{Path: "config/app.yaml", Kind: changedModified}},
			stats: architectureChangeStats{Files: 1},
			want:  []string{architectureSignalConfig},
		},
		{
			name:  "manifest activates dependencies and module boundary",
			files: []changedFile{{Path: "go.mod", Kind: changedModified}},
			stats: architectureChangeStats{Files: 1},
			want:  []string{architectureSignalDeps, architectureSignalModule},
		},
		{
			name:  "large change activates size",
			files: []changedFile{{Path: "src/util.go", Kind: changedModified}},
			stats: architectureChangeStats{Files: 25, AddedLines: 10},
			want:  []string{architectureSignalChangeSize},
		},
		{
			name: "two modules activate boundary",
			files: []changedFile{
				{Path: "internal/review/run.go", Kind: changedModified},
				{Path: "internal/store/review.go", Kind: changedModified},
			},
			stats: architectureChangeStats{Files: 2},
			want:  []string{architectureSignalModule},
		},
		{
			name:  "rename activates change type",
			files: []changedFile{{Path: "src/new.go", Kind: changedModified, Renamed: true}},
			stats: architectureChangeStats{Files: 1, Renamed: 1},
			want:  []string{architectureSignalType},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectArchitectureSignals(tc.files, tc.stats)
			for _, signal := range tc.want {
				if !containsString(got, signal) {
					t.Errorf("signals %v do not contain %q", got, signal)
				}
			}
			for _, signal := range tc.not {
				if containsString(got, signal) {
					t.Errorf("signals %v unexpectedly contain %q", got, signal)
				}
			}
			if len(tc.want) == 0 && len(got) != 0 {
				t.Errorf("signals = %v, want none", got)
			}
		})
	}
}

func TestArchitectureModeFailsClosed(t *testing.T) {
	cases := map[string]string{
		"":              store.ArchitectureAnalysisObservation,
		"observation":   store.ArchitectureAnalysisObservation,
		" OBSERVATION ": store.ArchitectureAnalysisObservation,
		"off":           store.ArchitectureAnalysisOff,
		"unexpected":    store.ArchitectureAnalysisOff,
	}
	for value, want := range cases {
		if got := architectureMode(store.QAPolicy{ArchitectureAnalysisMode: value}); got != want {
			t.Errorf("architectureMode(%q) = %q, want %q", value, got, want)
		}
	}
}

func TestApplyNativeArchitectureAnalysisPersistsEvidence(t *testing.T) {
	markdown := strings.Join([]string{
		"## Problem and Behavior Changes",
		"- Before: callers used the old path",
		"- After: callers go through the new boundary",
		"## Compatibility Risks",
		"- Existing clients may still send the legacy field",
		"## Alternatives",
		"- Keep both paths behind a flag",
		"## Pre-merge maintainer decisions",
		"- Run the migration test before merging",
		"## Recommendation",
		"- Prefer the boundary approach",
		"## Verification and Limits",
		"- The external consumer was not exercised",
	}, "\n")
	meta := nativeArchitectureMeta{
		SchemaVersion:      "ocr.review-analysis/v2",
		ExecutionStatus:    "completed",
		Comparison:         nativeArchitectureCompare{Old: "base-sha", Head: "head-sha"},
		Scope:              []nativeArchitectureChange{{NewPath: "internal/review/run.go"}},
		DeclaredCompletion: "complete",
		Usage: nativeArchitectureUsage{
			TotalTokens: 1234, InputTokens: 1000, OutputTokens: 234,
		},
		Model:               "model-x",
		SessionID:           "session-1",
		CitedObservationIDs: []string{"obs-000001"},
	}
	rawMeta, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	res := nativeReview{ReviewStatus: "succeeded", AnalysisRaw: &markdown, AnalysisMeta: rawMeta}
	verdict := newArchitectureVerdict(
		store.ArchitectureAnalysisObservation, true, []string{architectureSignalPersistence},
		store.ArchitectureScope{Included: []store.ArchitectureScopeFile{{Path: "internal/review/run.go"}}},
		store.ArchitectureComparison{BaseRef: "main", HeadRef: "feature", BaseSHA: "base-sha", HeadSHA: "head-sha"},
		store.ArchitectureBudget{TokenBudget: 10000, TimeBudgetSeconds: 60},
	)

	got := applyNativeArchitectureAnalysis(verdict, res)
	if got.Status != store.ArchitectureComplete {
		t.Fatalf("status = %q, want complete (errors: %v)", got.Status, got.Errors)
	}
	if !got.Requested || got.ExecutionStatus != "completed" {
		t.Errorf("requested/execution = %v/%q", got.Requested, got.ExecutionStatus)
	}
	if got.Comparison.BaseSHA != "base-sha" || got.Comparison.HeadSHA != "head-sha" {
		t.Errorf("comparison = %+v, want OCR base/head", got.Comparison)
	}
	if got.Evidence.Markdown != markdown || !containsString(got.Evidence.CitedObservationIDs, "obs-000001") {
		t.Errorf("evidence = %+v, want markdown and cited ids", got.Evidence)
	}
	if got.Usage.Total != 1234 || !got.Usage.Known {
		t.Errorf("usage = %+v, want 1234 known", got.Usage)
	}
	if len(got.Dimensions) != 6 {
		t.Errorf("dimensions = %d, want 6: %+v", len(got.Dimensions), got.Dimensions)
	}
	if !containsString(got.BehaviorChanges, "Before: callers used the old path") {
		t.Errorf("behavior changes = %v", got.BehaviorChanges)
	}
	if !containsString(got.Risks, "Existing clients may still send the legacy field") {
		t.Errorf("risks = %v", got.Risks)
	}
	if !containsString(got.Alternatives, "Keep both paths behind a flag") {
		t.Errorf("alternatives = %v", got.Alternatives)
	}
	if !containsString(got.AcceptanceConditions, "Run the migration test before merging") {
		t.Errorf("acceptance = %v", got.AcceptanceConditions)
	}
	if !containsString(got.Uncertainties, "The external consumer was not exercised") {
		t.Errorf("uncertainties = %v", got.Uncertainties)
	}
	if !strings.Contains(got.Recommendation, "boundary approach") {
		t.Errorf("recommendation = %q", got.Recommendation)
	}
	if strings.Contains(got.Recommendation, "migration test") {
		t.Errorf("recommendation duplicated acceptance conditions: %q", got.Recommendation)
	}
	if got.Budget.TokenBudget != 10000 || got.Budget.TimeBudgetSeconds != 60 {
		t.Errorf("budget = %+v", got.Budget)
	}
}

func TestApplyNativeArchitectureAnalysisRejectsAnotherComparison(t *testing.T) {
	markdown := "## Problem\n- evidence"
	meta := json.RawMessage(`{"execution_status":"completed","comparison":{"old":"other-base","head":"other-head"}}`)
	verdict := newArchitectureVerdict(store.ArchitectureAnalysisObservation, true, []string{"persistence"},
		store.ArchitectureScope{}, store.ArchitectureComparison{BaseSHA: "base-sha", HeadSHA: "head-sha"},
		store.ArchitectureBudget{})
	got := applyNativeArchitectureAnalysis(verdict, nativeReview{AnalysisRaw: &markdown, AnalysisMeta: meta})
	if got.Status != store.ArchitectureInconclusive {
		t.Fatalf("status = %q, want inconclusive", got.Status)
	}
	if got.Evidence.Markdown != "" || len(got.Dimensions) != 0 {
		t.Errorf("mismatched report was persisted as evidence: %+v", got.Evidence)
	}
	if !strings.Contains(strings.Join(got.Errors, "; "), "does not match reviewed base") {
		t.Errorf("errors = %v, want a mismatch explanation", got.Errors)
	}
}

func TestArchitectureStatsCountRenamesOnce(t *testing.T) {
	stats := architectureStatsFromFiles([]changedFile{{Path: "src/new.go", Renamed: true}}, architectureChangeStats{Renamed: 1})
	if stats.Renamed != 1 || stats.Files != 1 {
		t.Errorf("stats = %+v, want one file and one rename", stats)
	}
}

func TestApplyNativeArchitectureAnalysisDegradesWithoutEvidence(t *testing.T) {
	cases := []struct {
		name string
		res  nativeReview
	}{
		{name: "missing metadata", res: nativeReview{Status: "complete"}},
		{
			name: "failed execution",
			res: nativeReview{
				Status:       "complete",
				AnalysisMeta: json.RawMessage(`{"execution_status":"failed","errors":["analysis failed"]}`),
			},
		},
		{
			name: "empty report",
			res: nativeReview{
				Status:       "complete",
				AnalysisMeta: json.RawMessage(`{"execution_status":"completed"}`),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict := newArchitectureVerdict(store.ArchitectureAnalysisObservation, true, []string{"persistence"},
				store.ArchitectureScope{}, store.ArchitectureComparison{}, store.ArchitectureBudget{})
			got := applyNativeArchitectureAnalysis(verdict, tc.res)
			if got.Status != store.ArchitectureInconclusive {
				t.Fatalf("status = %q, want inconclusive", got.Status)
			}
			if len(got.Errors) == 0 {
				t.Fatal("inconclusive verdict has no traceable error")
			}
			// The review outcome is computed from the review status alone.
			status, code := nativeOutcome(tc.res)
			if status != store.RunSucceeded || code != "" {
				t.Errorf("review outcome = %s/%q, want succeeded", status, code)
			}
		})
	}
}

func TestArchitecturePartialVerdict(t *testing.T) {
	markdown := "## Problem\n- partial"
	meta := json.RawMessage(`{"execution_status":"completed","declared_completion":"partial"}`)
	verdict := newArchitectureVerdict(store.ArchitectureAnalysisObservation, true, []string{"change_size"},
		store.ArchitectureScope{}, store.ArchitectureComparison{}, store.ArchitectureBudget{})
	got := applyNativeArchitectureAnalysis(verdict, nativeReview{AnalysisRaw: &markdown, AnalysisMeta: meta})
	if got.Status != store.ArchitecturePartial {
		t.Fatalf("status = %q, want partial", got.Status)
	}
}

// TestMarkArchitectureTimedOutKeepsReviewOutcome pins the boundary of the
// analysis deadline: the observation becomes inconclusive, but the review
// status OCR reported is what decides the run.
func TestMarkArchitectureTimedOutKeepsReviewOutcome(t *testing.T) {
	verdict := newArchitectureVerdict(store.ArchitectureAnalysisObservation, true, []string{"persistence"},
		store.ArchitectureScope{}, store.ArchitectureComparison{}, store.ArchitectureBudget{})
	verdict.Status = store.ArchitectureComplete
	verdict.Evidence.Markdown = "## Resumen\n- ok"

	got := markArchitectureTimedOut(verdict, "analysis deadline exceeded")
	if got.Status != store.ArchitectureInconclusive {
		t.Errorf("status = %q, want inconclusive", got.Status)
	}
	if len(got.Errors) == 0 || !strings.Contains(got.Errors[len(got.Errors)-1], "deadline") {
		t.Errorf("errors = %v, want the deadline reason", got.Errors)
	}

	res := nativeReview{Status: "complete", Manifest: nativeManifest{TerminalState: "complete"}}
	if status, code := nativeOutcome(res); status != store.RunSucceeded || code != "" {
		t.Errorf("review outcome = %s/%q, want succeeded", status, code)
	}
}

// TestApplyAnalysisDeadlineDowngradesOnlyTheObservation connects the flag the
// reviewer sets to the verdict run.go persists, without depending on a shell.
func TestApplyAnalysisDeadlineDowngradesOnlyTheObservation(t *testing.T) {
	budget := store.ArchitectureBudget{TimeBudgetSeconds: 120}
	verdict := newArchitectureVerdict(store.ArchitectureAnalysisObservation, true, []string{"persistence"},
		store.ArchitectureScope{}, store.ArchitectureComparison{}, budget)
	verdict.Status = store.ArchitectureComplete

	res := nativeReview{Status: "complete", Manifest: nativeManifest{TerminalState: "complete"}, analysisTimedOut: true}
	got, applied := applyAnalysisDeadline(verdict, res, budget)
	if !applied {
		t.Fatal("applied = false, want the analysis deadline branch")
	}
	if got.Status != store.ArchitectureInconclusive {
		t.Errorf("status = %q, want inconclusive", got.Status)
	}
	if !strings.Contains(strings.Join(got.Errors, " "), "timeout_analysis") {
		t.Errorf("errors = %v, want the analysis phase code recorded", got.Errors)
	}
	if status, code := nativeOutcome(res); status != store.RunSucceeded || code != "" {
		t.Errorf("review outcome = %s/%q, want succeeded", status, code)
	}

	untouched, applied := applyAnalysisDeadline(verdict, nativeReview{Status: "complete"}, budget)
	if applied || untouched.Status != store.ArchitectureComplete {
		t.Errorf("verdict without a close = %q (applied=%v), want untouched", untouched.Status, applied)
	}
}

func TestMarkArchitectureTimedOutLeavesUnrequestedVerdict(t *testing.T) {
	verdict := newArchitectureVerdict(store.ArchitectureAnalysisOff, false, nil,
		store.ArchitectureScope{}, store.ArchitectureComparison{}, store.ArchitectureBudget{})
	got := markArchitectureTimedOut(verdict, "analysis deadline exceeded")
	if got.Status != store.ArchitectureNotRequested {
		t.Errorf("status = %q, want not_requested", got.Status)
	}
	if len(got.Errors) != 0 {
		t.Errorf("errors = %v, want none for an unrequested analysis", got.Errors)
	}
}

func TestArchitectureBudgetIsSeparateAndBounded(t *testing.T) {
	policy := store.QAPolicy{ArchitectureTokenShare: 25, ArchitectureTimeShare: 20, ArchitectureTokenMax: 100000}

	large := architectureBudget(policy, 45*time.Minute, 1_000_000)
	if large.TokenBudget != 100000 {
		t.Errorf("token budget = %d, want the 100000 cap", large.TokenBudget)
	}
	if large.TimeBudgetSeconds != 540 {
		t.Errorf("time budget = %ds, want 20%% of 45m", large.TimeBudgetSeconds)
	}
	if large.TimeBudgetSeconds > int64(architectureTimeMax/time.Second) {
		t.Errorf("time budget = %ds exceeds the analysis maximum", large.TimeBudgetSeconds)
	}

	small := architectureBudget(policy, 5*time.Minute, 0)
	if small.TokenBudget != 100000 {
		t.Errorf("unbounded review token budget = %d, want the analysis maximum", small.TokenBudget)
	}
	if small.TimeBudgetSeconds != 60 {
		t.Errorf("time budget = %ds, want 20%% of 5m", small.TimeBudgetSeconds)
	}
	if small.TimeBudgetSeconds > int64((5*time.Minute/2)/time.Second) {
		t.Errorf("time budget = %ds exceeds half the remaining time", small.TimeBudgetSeconds)
	}

	shared := architectureBudget(policy, time.Minute, 40000)
	if shared.TokenBudget != 10000 {
		t.Errorf("shared token budget = %d, want 25%% of 40000", shared.TokenBudget)
	}
}

func TestArchitectureScopeRecordsExclusions(t *testing.T) {
	files := []changedFile{
		{Path: "src/a.go", Kind: changedModified},
		{Path: "docs/old.md", Kind: changedDeleted},
		{Path: "assets/logo.png", Kind: changedBinary},
		{Path: "vendor/lib", Kind: changedSubmodule},
		{Path: "generated/api.go", Kind: changedModified},
	}
	scope := architectureScope(files, []string{"generated/**"})
	if len(scope.Included) != 1 || scope.Included[0].Path != "src/a.go" {
		t.Errorf("included = %+v", scope.Included)
	}
	if len(scope.Excluded) != 4 {
		t.Errorf("excluded = %+v, want 4 entries", scope.Excluded)
	}
	for _, reason := range []string{"deleted", "binary", "submodule", "policy"} {
		if !containsString(scope.OutOfScope, reason) {
			t.Errorf("out of scope %v missing %q", scope.OutOfScope, reason)
		}
	}
}

func containsArgValue(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func containsArgPair(args []string, name, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name && args[i+1] == value {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestArchitectureFindingsUseEvidenceCitations(t *testing.T) {
	verdict := store.ArchitectureVerdict{
		Requested: true, Status: store.ArchitectureComplete, Mode: store.ArchitectureAnalysisObservation,
		Signals: []string{"module_boundary"},
		Scope: store.ArchitectureScope{Included: []store.ArchitectureScopeFile{
			{Path: "internal/review/run.go"}, {Path: "internal/store/review.go"},
		}},
		Risks: []string{
			"El contrato cambia en `internal/review/run.go:10` y `internal/store/review.go:20-22`",
		},
		Recommendation: "Separar el límite entre los módulos",
	}

	findings := ArchitectureFindings(verdict)
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want one design finding", len(findings))
	}
	f := findings[0]
	if !f.Advisory {
		t.Error("architecture finding is not advisory; the verdict must stay observational")
	}
	if f.Scope != store.FindingScopeFiles {
		t.Errorf("scope = %q, want files", f.Scope)
	}
	if len(f.Citations) != 2 {
		t.Fatalf("citations = %+v, want both cited files", f.Citations)
	}
	if f.Path != "internal/review/run.go" || f.StartLine != 10 || f.EndLine != 10 {
		t.Errorf("primary anchor = %s:%d-%d, want the first citation", f.Path, f.StartLine, f.EndLine)
	}
	if !strings.Contains(f.Title, "Arquitectura:") || !strings.Contains(f.Body, "internal/review/run.go:10") {
		t.Errorf("finding = %+v, want the evidence in title/body", f)
	}
}

func TestArchitectureFindingsWithoutCitationsArePRScoped(t *testing.T) {
	verdict := store.ArchitectureVerdict{
		Requested: true, Status: store.ArchitecturePartial, Mode: store.ArchitectureAnalysisObservation,
		Risks: []string{"El contrato nuevo no tiene consumidores ejercitados"},
	}
	findings := ArchitectureFindings(verdict)
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want one design finding", len(findings))
	}
	f := findings[0]
	if f.Scope != store.FindingScopePR || f.Path != "" || f.StartLine != 0 {
		t.Errorf("finding = %+v, want a PR-scope finding with no anchor", f)
	}
	if len(f.Citations) != 0 {
		t.Errorf("citations = %+v, want none", f.Citations)
	}
}

func TestArchitectureFindingsSkipUnrequestedOrInconclusive(t *testing.T) {
	cases := []store.ArchitectureVerdict{
		{Requested: false, Status: store.ArchitectureNotRequested, Risks: []string{"r"}},
		{Requested: true, Status: store.ArchitectureInconclusive, Risks: []string{"r"}},
		{Requested: true, Status: store.ArchitectureComplete},
	}
	for _, verdict := range cases {
		if findings := ArchitectureFindings(verdict); len(findings) != 0 {
			t.Errorf("findings = %+v, want none for %+v", findings, verdict)
		}
	}
}

func TestReconcileResolvesAdvisoryFindingOutsideReviewScope(t *testing.T) {
	advisory := AgentFinding{
		Title: "Arquitectura: límite nuevo", Severity: "minor", Category: "design",
		Scope: store.FindingScopePR, Advisory: true,
	}
	first, _ := Reconcile(nil, AgentOutput{Findings: []AgentFinding{advisory}}, 1, true, 9, nil)
	if len(first) != 1 || !first[0].Advisory {
		t.Fatalf("first upserts = %+v, want one advisory finding", first)
	}
	_, resolved := ReconcileScoped(first, AgentOutput{}, 2, true, 9, nil, map[string]bool{})
	if len(resolved) != 1 || resolved[0] != first[0].ID {
		t.Fatalf("resolved = %v, want the advisory finding", resolved)
	}
}

func TestArchitectureFindingsOnlyCiteTheComparedScope(t *testing.T) {
	verdict := store.ArchitectureVerdict{
		Requested: true, Status: store.ArchitectureComplete, Mode: store.ArchitectureAnalysisObservation,
		Scope: store.ArchitectureScope{Included: []store.ArchitectureScopeFile{{Path: "internal/review/run.go"}}},
		Risks: []string{
			"El contrato cambia en `internal/review/run.go:10` y `internal/store/other.go:20`",
		},
	}
	findings := ArchitectureFindings(verdict)
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(findings))
	}
	citations := findings[0].Citations
	if len(citations) != 1 || citations[0].Path != "internal/review/run.go" {
		t.Errorf("citations = %+v, want only the compared path", citations)
	}
}

func TestArchitectureFindingsMapDeletedEvidenceToBase(t *testing.T) {
	verdict := store.ArchitectureVerdict{
		Requested: true, Status: store.ArchitectureComplete, Mode: store.ArchitectureAnalysisObservation,
		Scope: store.ArchitectureScope{
			Included: []store.ArchitectureScopeFile{{Path: "internal/review/run.go"}},
			Excluded: []store.ArchitectureScopeFile{
				{Path: "internal/review/old.go", Kind: "deleted"},
				{Path: "internal/review/asset.bin", Kind: "binary"},
			},
		},
		Risks: []string{
			"Antes `internal/review/old.go:5`, ahora `internal/review/run.go:10`; " +
				"el binario `internal/review/asset.bin:2` no aplica",
		},
	}
	findings := ArchitectureFindings(verdict)
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(findings))
	}
	citations := findings[0].Citations
	if len(citations) != 2 {
		t.Fatalf("citations = %+v, want the head and base citations", citations)
	}
	byPath := make(map[string]store.FindingCitation, len(citations))
	for _, citation := range citations {
		byPath[citation.Path] = citation
	}
	if got := byPath["internal/review/run.go"]; got.Side != store.FindingCitationHead {
		t.Errorf("run.go side = %q, want RIGHT", got.Side)
	}
	if got := byPath["internal/review/old.go"]; got.Side != store.FindingCitationBase {
		t.Errorf("old.go side = %q, want LEFT", got.Side)
	}
	if _, ok := byPath["internal/review/asset.bin"]; ok {
		t.Error("a binary exclusion was cited as code")
	}
}

func TestArchitectureFindingsRequireKnownScope(t *testing.T) {
	verdict := store.ArchitectureVerdict{
		Requested: true, Status: store.ArchitectureComplete, Mode: store.ArchitectureAnalysisObservation,
		Risks: []string{"El contrato cambia en `internal/review/run.go:10`"},
	}
	findings := ArchitectureFindings(verdict)
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(findings))
	}
	if len(findings[0].Citations) != 0 || findings[0].Scope != store.FindingScopePR {
		t.Errorf("finding = %+v, want a PR-scope finding without citations", findings[0])
	}
}
