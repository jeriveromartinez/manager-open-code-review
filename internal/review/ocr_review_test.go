package review

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestBuildReviewArgs(t *testing.T) {
	got := buildReviewArgs(nativeInvocation{
		RepoDir:        "/work/run-1",
		BaseRef:        "base-sha",
		HeadRef:        "head-sha",
		BackgroundFile: "/work/run-1/home-ocr/background.md",
		Excludes:       "vendor/**,**/*.pb.go",
		Profile:        store.LLMProfile{Model: "model-x"},
		Effort:         "high",
		MaxTokens:      reviewContextCeiling,
		TokenBudget:    500000,
		TaskTimeout:    2 * time.Minute,
	})
	want := []string{
		"review",
		"--repo", "/work/run-1",
		"--from", "base-sha",
		"--to", "head-sha",
		"--format", "json",
		"--audience", "human",
		"--provider", nativeProviderName,
		"--model", "model-x",
		"--effort", "high",
		"--max-tokens", "270000",
		"--background-file", "/work/run-1/home-ocr/background.md",
		"--max-tokens-budget", "500000",
		"--timeout", "2",
		"--exclude", "vendor/**,**/*.pb.go",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args =\n%q\nwant\n%q", got, want)
	}

	withoutOptional := buildReviewArgs(nativeInvocation{
		RepoDir: "/r", BaseRef: "a", HeadRef: "b", BackgroundFile: "/bg",
		Profile: store.LLMProfile{Model: "m"}, Effort: "low", MaxTokens: 1,
	})
	for _, arg := range withoutOptional {
		if arg == "--timeout" {
			t.Fatalf("unexpected --timeout in %q", withoutOptional)
		}
		if arg == "--max-tokens-budget" {
			t.Fatalf("unexpected --max-tokens-budget when the budget is disabled: %q", withoutOptional)
		}
	}
	// The effective ceiling is always sent, even when the optional budget is off.
	if !containsArg(withoutOptional, "--max-tokens") {
		t.Fatalf("--max-tokens missing from %q", withoutOptional)
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func TestReviewEnvIsolation(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"KEEP=ok",
		"QA_LLM_API_KEY=other-run",
		"ANTHROPIC_API_KEY=a",
		"ANTHROPIC_BASE_URL=http://a",
		"OPENAI_API_KEY=o",
		"OPENROUTER_API_KEY=r",
		"GEMINI_API_KEY=g",
		"DASHSCOPE_API_KEY=d",
		"GROQ_API_KEY=q",
		"AZURE_OPENAI_API_KEY=z",
		"OCR_LLM_URL=http://ocr",
		"OCR_LLM_TOKEN=t",
		"OCR_USE_ANTHROPIC=1",
		"HOME=/old-home",
		"XDG_CONFIG_HOME=/old-config",
	}
	env := reviewEnv(base, "selected-key", "/tmp/isolated")
	joined := "\n" + strings.Join(env, "\n") + "\n"

	for _, want := range []string{
		"\nPATH=/usr/bin\n", "\nKEEP=ok\n",
		"\n" + nativeAPIKeyEnv + "=selected-key\n",
		"\nHOME=/tmp/isolated\n",
		"\nOCR_NO_UPDATE=1\n",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("env missing %q:\n%s", want, joined)
		}
	}
	for _, unwanted := range []string{
		"QA_LLM_API_KEY", "ANTHROPIC_", "OPENAI_", "OPENROUTER_API_KEY",
		"GEMINI_", "DASHSCOPE_", "OCR_LLM_", "OCR_USE_ANTHROPIC",
		"GROQ_API_KEY", "AZURE_OPENAI_API_KEY",
		"HOME=/old-home", "XDG_CONFIG_HOME=/old-config",
	} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("env leaked %q:\n%s", unwanted, joined)
		}
	}
}

func TestParseReviewJSON(t *testing.T) {
	raw := []byte(`{
	  "status": "partial",
	  "message": "Review partial",
	  "llm": {"provider": "qa-review", "model": "m"},
	  "summary": {"files_reviewed": 3, "comments": 1, "total_tokens": 900,
	    "input_tokens": 700, "output_tokens": 200, "cache_read_tokens": 50,
	    "cache_write_tokens": 10, "elapsed": "2s", "budget_exceeded": true},
	  "comments": [{
	    "path": "src/a.go", "content": "Nil deref\n\nmore", "start_line": 4,
	    "end_line": 6, "category": "bug", "severity": "critical",
	    "suggestion_code": "return nil"
	  }],
	  "manifest": {
	    "schema_version": "ocr.run-manifest/v1",
	    "terminal_state": "partial",
	    "coverage": {
	      "selected": [{"path": "src/a.go"}, {"path": "src/b.go"}],
	      "completed": [{"path": "src/a.go"}],
	      "reused": [{"path": "src/b.go"}],
	      "failed": [{"path": "src/c.go", "classification": "budget"}],
	      "waived": [],
	      "excluded": [{"path": "README.md"}]
	    }
	  }
	}`)

	got, err := parseNativeReview(raw)
	if err != nil {
		t.Fatalf("parseNativeReview: %v", err)
	}
	if got.Status != "partial" || got.Summary.InputTokens != 700 || got.Summary.OutputTokens != 200 {
		t.Fatalf("unexpected result: %+v", got)
	}
	if len(got.Comments) != 1 || got.Comments[0].SuggestionCode != "return nil" {
		t.Fatalf("comments not mapped: %+v", got.Comments)
	}
	if reviewed, skipped, excluded := nativeCoverageCounts(got); reviewed != 2 || skipped != 1 || excluded != 1 {
		t.Errorf("coverage = %d/%d/%d, want 2/1/1", reviewed, skipped, excluded)
	}
	usage := nativeTokenUsage(got)
	if usage.Input != 700 || usage.Output != 200 || usage.Cache.Read != 50 ||
		usage.Cache.Write != 10 || usage.Total != 900 {
		t.Errorf("usage = %+v", usage)
	}

	if _, err := parseNativeReview([]byte(`{"summary":{}}`)); err == nil {
		t.Error("expected an error when neither status nor terminal_state is present")
	}
	if _, err := parseNativeReview([]byte(`not json`)); err == nil {
		t.Error("expected an error for invalid JSON")
	}
}

func TestMapNativeFindingSeverityCategory(t *testing.T) {
	severities := map[string]string{
		"critical": "blocker", "high": "major", "medium": "minor",
		"low": "nit", "": "minor", "urgent": "minor",
	}
	for in, want := range severities {
		if got := mapNativeSeverity(in); got != want {
			t.Errorf("severity %q -> %q, want %q", in, got, want)
		}
	}
	categories := map[string]string{
		"bug": "bug", "security": "security", "performance": "performance",
		"test": "test", "style": "style", "maintainability": "maintainability",
		"documentation": "maintainability", "other": "maintainability",
		"unknown": "maintainability",
	}
	for in, want := range categories {
		if got := mapNativeCategory(in); got != want {
			t.Errorf("category %q -> %q, want %q", in, got, want)
		}
	}

	findings := nativeFindings([]nativeComment{{
		Path: "a.go", Content: "  Head line \n\nbody with details", StartLine: 5,
		EndLine: 4, Category: "documentation", Severity: "high", SuggestionCode: "fix it",
	}})
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(findings))
	}
	f := findings[0]
	if f.Title != "Head line" || f.Body != "Head line \n\nbody with details" ||
		f.Severity != "major" || f.Category != "maintainability" ||
		f.Suggestion != "fix it" || f.StartLine != 5 || f.EndLine != 5 {
		t.Errorf("mapped finding = %+v", f)
	}
}

func TestStatusFromManifestCoverage(t *testing.T) {
	tests := []struct {
		name        string
		status      string
		state       string
		failed      []nativeCoverageItem
		retryReport *nativeRetryReport
		warnings    []nativeWarning
		budget      bool
		global      bool
		want        string
		code        string
	}{
		{name: "complete", status: "complete", want: store.RunSucceeded},
		{
			// A skipped run reviewed nothing; resolving findings would approve
			// a PR the engine never looked at.
			name: "skipped", status: "skipped", want: store.RunInconclusive, code: "no_coverage",
		},
		{
			name: "partial budget", status: "partial", budget: true,
			failed: []nativeCoverageItem{{Classification: "budget"}},
			want:   store.RunInconclusive, code: "token_budget",
		},
		{
			name: "partial budget warning", status: "partial",
			failed:   []nativeCoverageItem{{Classification: "budget"}},
			warnings: []nativeWarning{{Type: "token_budget_reached"}},
			want:     store.RunInconclusive, code: "token_budget",
		},
		{
			name: "partial budget flag only", status: "partial", budget: true,
			want: store.RunInconclusive, code: "token_budget",
		},
		{
			name: "partial provider", status: "partial",
			failed: []nativeCoverageItem{{Classification: "provider"}},
			want:   store.RunInconclusive, code: "coverage_incomplete",
		},
		{
			name: "failed budget", status: "failed", budget: true,
			failed: []nativeCoverageItem{{Classification: "budget"}},
			want:   store.RunInconclusive, code: "token_budget",
		},
		{
			name: "failed budget warning", status: "failed",
			warnings: []nativeWarning{{Type: "token_budget_reached"}},
			want:     store.RunInconclusive, code: "token_budget",
		},
		{
			name: "failed budget flag only", status: "failed", budget: true,
			want: store.RunInconclusive, code: "token_budget",
		},
		{
			// No request-level reason: OCR's own per-group budget is the
			// fallback used when the worker cannot close it progressively.
			name: "failed per-group timeout", status: "failed",
			failed: []nativeCoverageItem{{Classification: "timeout"}},
			want:   store.RunInconclusive, code: "timeout_group",
		},
		{
			name: "failed request timeout", status: "failed",
			failed: []nativeCoverageItem{{
				Classification: "timeout",
				Reason:         "LLM request timed out; configure OCR_LLM_TIMEOUT or provider timeout_sec",
			}},
			want: store.RunInconclusive, code: "timeout_http",
		},
		{
			name: "failed global timeout", status: "failed", global: true,
			failed: []nativeCoverageItem{{Classification: "timeout"}},
			want:   store.RunInconclusive, code: "timeout_global",
		},
		{
			// The deadline wins over whatever failure OCR reported alongside
			// the close: findings of completed work must be preserved.
			name: "failed provider after global close", status: "failed", global: true,
			failed: []nativeCoverageItem{{Classification: "provider"}},
			want:   store.RunInconclusive, code: "timeout_global",
		},
		{
			name: "partial global timeout", status: "partial", global: true,
			want: store.RunInconclusive, code: "timeout_global",
		},
		{
			name: "failed configuration", status: "failed",
			failed: []nativeCoverageItem{{Classification: "configuration"}},
			want:   store.RunFailed, code: "config",
		},
		{
			name: "failed provider item", status: "failed",
			failed: []nativeCoverageItem{{Classification: "provider"}},
			want:   store.RunFailed, code: "llm_error",
		},
		{
			name: "failed provider retry report", status: "failed",
			retryReport: &nativeRetryReport{FailedRequests: 1, TotalRetries: 5},
			want:        store.RunFailed, code: "llm_error",
		},
		{name: "missing status", want: store.RunFailed, code: "ocr_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := nativeReview{
				Status: tt.status,
				Manifest: nativeManifest{
					TerminalState: tt.state,
					Coverage:      nativeCoverage{Failed: tt.failed},
				},
				Summary:     nativeSummary{BudgetExceeded: tt.budget},
				Warnings:    tt.warnings,
				RetryReport: tt.retryReport,
			}
			if tt.global {
				res.closeReason = "global"
			}
			got, code := nativeOutcome(res)
			if got != tt.want || code != tt.code {
				t.Errorf("nativeOutcome = %q/%q, want %q/%q", got, code, tt.want, tt.code)
			}
		})
	}
}

func TestNativeFixtures(t *testing.T) {
	tests := []struct {
		file           string
		state          string
		comments       int
		reviewed       int
		skipped        int
		excluded       int
		totalTokens    int
		status         string
		code           string
		failedClass    string
		retryFailures  int
		budgetExceeded bool
		// pending is the number of changed files the engine left without
		// coverage. A complete result with an internal exclusion is not full
		// coverage once the diff inventory is considered (#135).
		pending int
	}{
		{
			file: "ocr-review-complete.json", state: "complete", comments: 2,
			reviewed: 2, excluded: 1, totalTokens: 4960, status: store.RunSucceeded,
			pending: 1,
		},
		{
			file: "ocr-review-partial-budget.json", state: "partial", comments: 3,
			reviewed: 3, skipped: 9, totalTokens: 14880, status: store.RunInconclusive,
			code: "token_budget", failedClass: "budget", budgetExceeded: true,
		},
		{
			file: "ocr-review-failed-provider.json", state: "failed",
			reviewed: 0, skipped: 2, excluded: 1, status: store.RunFailed,
			code: "llm_error", failedClass: "provider", retryFailures: 1,
		},
		{
			file: "ocr-review-skipped.json", state: "skipped", comments: 0,
			reviewed: 0, skipped: 0, excluded: 1,
			status: store.RunInconclusive, code: "no_coverage",
		},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", tt.file))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			res, err := parseNativeReview(raw)
			if err != nil {
				t.Fatalf("parseNativeReview: %v", err)
			}
			if res.Manifest.TerminalState != tt.state {
				t.Errorf("terminal_state = %q, want %q", res.Manifest.TerminalState, tt.state)
			}
			if got := len(res.Comments); got != tt.comments {
				t.Errorf("comments = %d, want %d", got, tt.comments)
			}
			reviewed, skipped, excluded := nativeCoverageCounts(res)
			if reviewed != tt.reviewed || skipped != tt.skipped || excluded != tt.excluded {
				t.Errorf("coverage = %d/%d/%d, want %d/%d/%d",
					reviewed, skipped, excluded, tt.reviewed, tt.skipped, tt.excluded)
			}
			if res.Summary.TotalTokens != tt.totalTokens {
				t.Errorf("total_tokens = %d, want %d", res.Summary.TotalTokens, tt.totalTokens)
			}
			if res.Summary.BudgetExceeded != tt.budgetExceeded {
				t.Errorf("budget_exceeded = %v, want %v", res.Summary.BudgetExceeded, tt.budgetExceeded)
			}
			if tt.failedClass != "" {
				for _, item := range res.Manifest.Coverage.Failed {
					if item.Classification != tt.failedClass {
						t.Errorf("failed classification = %q, want %q", item.Classification, tt.failedClass)
					}
				}
			}
			if tt.retryFailures > 0 {
				if res.RetryReport == nil || res.RetryReport.FailedRequests != tt.retryFailures {
					t.Errorf("retry_report = %+v, want %d failed requests", res.RetryReport, tt.retryFailures)
				}
			}
			status, code := nativeOutcome(res)
			if status != tt.status || code != tt.code {
				t.Errorf("nativeOutcome = %q/%q, want %q/%q", status, code, tt.status, tt.code)
			}
			if tt.pending > 0 {
				// The engine alone cannot see the diff inventory: the excluded
				// README.md changed in this comparison, so the run-level decision
				// escalates even though nativeOutcome reported success.
				var changed []changedFile
				for _, item := range res.Manifest.Coverage.Selected {
					changed = append(changed, changedFile{Path: item.Path, Kind: changedModified})
				}
				for _, item := range res.Manifest.Coverage.Excluded {
					changed = append(changed, changedFile{Path: item.Path, Kind: changedModified})
				}
				_, pending := inventoryCoverage(nativeCoverageFiles(res), changed, nil, coverageSignalFrom(res))
				if len(pending) != tt.pending {
					t.Fatalf("pending = %+v, want %d file(s)", pending, tt.pending)
				}
				baseStatus, baseCode := nativeOutcome(res)
				if got, code := pendingRunStatus(baseStatus, baseCode, pending); got != store.RunInconclusive || code != "coverage_incomplete" {
					t.Errorf("run outcome with pending coverage = %q/%q, want inconclusive/coverage_incomplete", got, code)
				}
			}
		})
	}
}

func TestNativeTitleFingerprintStable(t *testing.T) {
	comment := nativeComment{
		Path: "src/a.go", Content: "Nil pointer dereference\n\nmore detail",
		StartLine: 7, EndLine: 7, Category: "bug", Severity: "high",
	}
	first := mergeNativeFindings(0, []nativeComment{comment}, nil).Findings
	second := mergeNativeFindings(0, []nativeComment{comment}, nil).Findings
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("findings = %d/%d, want 1/1", len(first), len(second))
	}
	fp1, fp2 := Fingerprint(0, first[0], nil, 0), Fingerprint(0, second[0], nil, 0)
	if fp1 != fp2 {
		t.Fatalf("fingerprint is not deterministic: %q != %q", fp1, fp2)
	}
	// OCR's title is derived from the first non-empty line; surrounding
	// whitespace must not fork the fingerprint.
	spaced := comment
	spaced.Content = "  Nil pointer dereference  \n\ndifferent details"
	if got := Fingerprint(0, mergeNativeFindings(0, []nativeComment{spaced}, nil).Findings[0], nil, 0); got != fp1 {
		t.Errorf("spaced title fingerprint = %q, want %q", got, fp1)
	}
}

func TestMergeNativeFindingsUsesAnchorIdentity(t *testing.T) {
	nearby := func(start int, code string) nativeComment {
		return nativeComment{
			Path: "src/a.go", Content: "Mismo problema\n\ncuerpo", ExistingCode: code,
			StartLine: start, EndLine: start, Category: "bug", Severity: "high",
		}
	}

	if got := mergeNativeFindings(1, []nativeComment{nearby(10, "guard()"), nearby(12, "guard()")}, nil).Findings; len(got) != 1 {
		t.Fatalf("nearby duplicates = %d, want 1", len(got))
	}
	if got := mergeNativeFindings(1, []nativeComment{nearby(10, "guard()"), nearby(40, "guard()")}, nil).Findings; len(got) != 2 {
		t.Fatalf("distant occurrences = %d, want 2", len(got))
	}
	if got := mergeNativeFindings(1, []nativeComment{nearby(10, "guard()"), nearby(11, "other()")}, nil).Findings; len(got) != 2 {
		t.Fatalf("same title over a different anchor merged into %d finding(s), want 2", len(got))
	}

	// Two chunks that split a boundary without citing code have different
	// derived anchors, but proximity is the only signal available and must merge.
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i+1)
	}
	uncited := func(start int) nativeComment {
		return nativeComment{
			Path: "src/a.go", Content: "Frontera compartida\n\ncuerpo",
			StartLine: start, EndLine: start, Category: "bug", Severity: "high",
		}
	}
	if got := mergeNativeFindings(1, []nativeComment{uncited(20), uncited(21)}, linesProvider(lines)).Findings; len(got) != 1 {
		t.Fatalf("uncited boundary duplicates = %d, want 1", len(got))
	}
}

type nativeCustomProvider struct {
	URL        string            `json:"url"`
	Protocol   string            `json:"protocol"`
	Model      string            `json:"model"`
	APIKeyCmd  string            `json:"api_key_cmd"`
	ExtraBody  map[string]string `json:"extra_body"`
	TimeoutSec int               `json:"timeout_sec"`
}

type nativeConfigFile struct {
	CustomProviders map[string]nativeCustomProvider `json:"custom_providers"`
}

func readNativeConfig(t *testing.T, home string) nativeCustomProvider {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, ".opencodereview", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg nativeConfigFile
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode OCR config: %v (%s)", err, raw)
	}
	provider, ok := cfg.CustomProviders[nativeProviderName]
	if !ok {
		t.Fatalf("config has no %q provider: %s", nativeProviderName, raw)
	}
	return provider
}

func TestNativeConfigKeepsKeyOffDisk(t *testing.T) {
	home := t.TempDir()
	base := "https://provider.example/v1"
	profile := store.LLMProfile{Provider: "custom", Model: "model-x", BaseURL: &base}
	keyPath, err := writeNativeKeyHelper(home)
	if err != nil {
		t.Fatalf("writeNativeKeyHelper: %v", err)
	}
	if err := writeNativeConfig(home, profile, keyPath, 0); err != nil {
		t.Fatalf("writeNativeConfig: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".opencodereview", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-secret") {
		t.Fatalf("config contains the API key: %s", raw)
	}
	custom := readNativeConfig(t, home)
	if custom.URL != base || custom.Model != "model-x" || custom.APIKeyCmd != keyPath {
		t.Errorf("custom provider = %+v, want url %q, model model-x and the key helper", custom, base)
	}
	// An unknown provider uses the OpenAI-compatible dialect, so it accepts the
	// explicit cache key.
	if got := custom.ExtraBody["prompt_cache_key"]; got != ocrSessionKeyPlaceholder {
		t.Errorf("prompt_cache_key = %q, want %q", got, ocrSessionKeyPlaceholder)
	}

	noEnv := exec.Command(keyPath)
	noEnv.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := noEnv.Output()
	if err != nil {
		t.Fatalf("run key helper without env: %v", err)
	}
	// Without the environment the helper prints nothing; the key only exists
	// in the child env at run time.
	if len(out) != 0 {
		t.Errorf("helper printed %q without the environment", out)
	}
	cmd := exec.Command(keyPath)
	cmd.Env = []string{nativeAPIKeyEnv + "=sk-secret"}
	out, err = cmd.Output()
	if err != nil {
		t.Fatalf("run key helper: %v", err)
	}
	if string(out) != "sk-secret" {
		t.Errorf("helper output = %q, want the injected key", out)
	}
}

// TestNativeConfigPromptCachePerProtocol pins the provider dialect the cache
// key is safe for. OCR forwards extra_body into the provider request, and
// Anthropic rejects unknown fields, so sending the key there fails every run.
func TestNativeConfigPromptCachePerProtocol(t *testing.T) {
	base := "https://provider.example/v1"
	tests := []struct {
		name         string
		provider     string
		baseURL      *string
		wantProtocol string
		wantCache    bool
	}{
		{name: "unknown provider uses openai dialect", provider: "custom", baseURL: &base, wantProtocol: "openai", wantCache: true},
		{name: "openai", provider: "openai", wantProtocol: "openai", wantCache: true},
		{name: "groq", provider: "groq", wantProtocol: "openai", wantCache: true},
		{name: "anthropic", provider: "anthropic", wantProtocol: "anthropic", wantCache: false},
		{name: "google compatibility endpoint", provider: "google", wantProtocol: "openai", wantCache: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			keyPath, err := writeNativeKeyHelper(home)
			if err != nil {
				t.Fatalf("writeNativeKeyHelper: %v", err)
			}
			profile := store.LLMProfile{Provider: tt.provider, Model: "m", BaseURL: tt.baseURL}
			if err := writeNativeConfig(home, profile, keyPath, 0); err != nil {
				t.Fatalf("writeNativeConfig: %v", err)
			}
			cfg := readNativeConfig(t, home)
			if cfg.Protocol != tt.wantProtocol {
				t.Errorf("protocol = %q, want %q", cfg.Protocol, tt.wantProtocol)
			}
			_, got := cfg.ExtraBody["prompt_cache_key"]
			if got != tt.wantCache {
				t.Errorf("prompt_cache_key present = %v, want %v (extra_body %v)",
					got, tt.wantCache, cfg.ExtraBody)
			}
		})
	}
}

// TestOCRBudget pins how the run's remaining time becomes OCR's own limits.
// OCR multiplies --timeout by the effort rounds and only stops itself (and
// writes its JSON with partial results) inside that budget, so the budget must
// end before the run context or the whole review is lost (#128).
func TestOCRBudget(t *testing.T) {
	tests := []struct {
		name        string
		remaining   time.Duration
		effort      string
		wantTask    time.Duration
		wantRequest time.Duration
	}{
		{name: "default run, low effort", remaining: 20 * time.Minute, effort: "low", wantTask: 18 * time.Minute, wantRequest: 300 * time.Second},
		{name: "medium halves the per-round budget", remaining: 20 * time.Minute, effort: "medium", wantTask: 9 * time.Minute, wantRequest: 300 * time.Second},
		{name: "high splits it in three", remaining: 20 * time.Minute, effort: "high", wantTask: 6 * time.Minute, wantRequest: 300 * time.Second},
		{name: "long run keeps a ten percent reserve", remaining: 45 * time.Minute, effort: "low", wantTask: 40 * time.Minute, wantRequest: 300 * time.Second},
		{name: "short run shortens each request", remaining: 4 * time.Minute, effort: "low", wantTask: 2 * time.Minute, wantRequest: 60 * time.Second},
		{name: "request limit follows the subtask budget", remaining: 10 * time.Minute, effort: "low", wantTask: 8 * time.Minute, wantRequest: 160 * time.Second},
		{name: "never below one minute per round", remaining: 3 * time.Minute, effort: "medium", wantTask: time.Minute, wantRequest: 60 * time.Second},
		{name: "unknown effort counts as one round", remaining: 20 * time.Minute, effort: "", wantTask: 18 * time.Minute, wantRequest: 300 * time.Second},
		{name: "no time means no limits", remaining: 0, effort: "low", wantTask: 0, wantRequest: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task, request := ocrBudget(tt.remaining, tt.effort)
			if task != tt.wantTask || request != tt.wantRequest {
				t.Fatalf("ocrBudget(%s, %q) = (%s, %s), want (%s, %s)",
					tt.remaining, tt.effort, task, request, tt.wantTask, tt.wantRequest)
			}
		})
	}
}

// TestOCRGlobalDeadlineIsSharedAcrossGroups pins that the deadline is an
// absolute instant taken from the run, not a fresh per-group minute: the second
// group gets what is left, never a full budget of its own.
func TestOCRGlobalDeadlineIsSharedAcrossGroups(t *testing.T) {
	runDeadline := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithDeadline(context.Background(), runDeadline)
	defer cancel()

	firstAt := time.Date(2029, 12, 31, 23, 40, 0, 0, time.UTC)
	secondAt := firstAt.Add(10 * time.Minute)
	first, ok := earliestDeadline(firstAt, ctx, 45*time.Minute)
	if !ok || !first.Equal(runDeadline) {
		t.Fatalf("first group deadline = %s (ok=%v), want the run deadline %s", first, ok, runDeadline)
	}
	second, ok := earliestDeadline(secondAt, ctx, 45*time.Minute)
	if !ok || !second.Equal(runDeadline) {
		t.Fatalf("second group deadline = %s (ok=%v), want the run deadline %s", second, ok, runDeadline)
	}
	if second.Sub(secondAt) >= first.Sub(firstAt) {
		t.Errorf("second group budget = %s, want less than the first %s",
			second.Sub(secondAt), first.Sub(firstAt))
	}

	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	if d, ok := earliestDeadline(now, context.Background(), 30*time.Minute); !ok || !d.Equal(now.Add(30*time.Minute)) {
		t.Errorf("deadline without context = %s (ok=%v), want %s", d, ok, now.Add(30*time.Minute))
	}
	// OCR stops before the run deadline so parsing, chunking, verification and
	// publishing keep their window.
	if stop, ok := ocrStopDeadline(now, context.Background(), 30*time.Minute); !ok || !stop.Equal(now.Add(27*time.Minute)) {
		t.Errorf("OCR stop deadline = %s (ok=%v), want %s", stop, ok, now.Add(27*time.Minute))
	}
}

// TestOCRStopDeadlineReserveIsBounded pins that the reserve never consumes
// more than half the invocation, so the OCR budget keeps growing with the time
// left instead of collapsing around the 2m floor.
func TestOCRStopDeadlineReserveIsBounded(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	previous := time.Duration(0)
	for _, remaining := range []time.Duration{
		30 * time.Second, time.Minute, 119 * time.Second, 2 * time.Minute,
		121 * time.Second, 3 * time.Minute, 4 * time.Minute, 10 * time.Minute,
	} {
		stop, ok := ocrStopDeadline(now, context.Background(), remaining)
		if !ok {
			t.Fatalf("ocrStopDeadline(%s) had no deadline", remaining)
		}
		window := stop.Sub(now)
		if window < remaining/2 {
			t.Errorf("OCR window for %s = %s, want at least half", remaining, window)
		}
		if window <= previous {
			t.Errorf("OCR window for %s = %s, want more than the previous %s", remaining, window, previous)
		}
		previous = window
	}
}

// TestAnalysisStopDeadlineUsesTheSoonerBound pins that the analysis carries its
// own bound, but never outlives the run: the analysis share wins when it is
// sooner, and a zero share keeps only the run deadline.
func TestAnalysisStopDeadlineUsesTheSoonerBound(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	runDeadline := now.Add(30 * time.Minute)

	got, ok, bounded := analysisStopDeadline(now, runDeadline, true, 5*time.Minute)
	if !ok || !bounded || !got.Equal(now.Add(5*time.Minute)) {
		t.Errorf("analysis deadline = %s (ok=%v bounded=%v), want the 5m share", got, ok, bounded)
	}

	got, ok, bounded = analysisStopDeadline(now, runDeadline, true, 45*time.Minute)
	if !ok || bounded || !got.Equal(runDeadline) {
		t.Errorf("analysis deadline = %s (ok=%v bounded=%v), want the run deadline", got, ok, bounded)
	}

	got, ok, bounded = analysisStopDeadline(now, runDeadline, true, 0)
	if !ok || bounded || !got.Equal(runDeadline) {
		t.Errorf("zero share deadline = %s (ok=%v bounded=%v), want no analysis bound", got, ok, bounded)
	}

	if got, ok, bounded := analysisStopDeadline(now, time.Time{}, false, 0); ok || bounded || !got.IsZero() {
		t.Errorf("no run deadline = %s (ok=%v bounded=%v), want no deadline at all", got, ok, bounded)
	}

	got, ok, bounded = analysisStopDeadline(now, time.Time{}, false, 5*time.Minute)
	if !ok || !bounded || !got.Equal(now.Add(5*time.Minute)) {
		t.Errorf("share without run deadline = %s (ok=%v bounded=%v), want the share", got, ok, bounded)
	}
}

// TestAnalysisTimeoutHasItsOwnCode pins the phase split: the analysis share is
// not the run's global deadline, so its expiry must not be reported as
// timeout_global and must not be treated as a review failure.
func TestAnalysisTimeoutHasItsOwnCode(t *testing.T) {
	if got := timeoutCodeFromKind("analysis"); got != "timeout_analysis" {
		t.Fatalf("analysis timeout code = %q, want timeout_analysis", got)
	}
}

// TestVerifyStopDeadlineKeepsShare pins #151: the verifier's full share is
// available to it even when it is smaller than the main run's 2m reserve.
func TestVerifyStopDeadlineKeepsShare(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	share := 2*time.Minute + 6*time.Second // 15% of ~14m
	stop, ok := verifyStopDeadline(now, context.Background(), share)
	if !ok || !stop.Equal(now.Add(share)) {
		t.Errorf("verifier stop = %s (ok=%v), want its full share %s", stop, ok, now.Add(share))
	}
}

func TestOCRLimitsGlobalMode(t *testing.T) {
	tests := []struct {
		name      string
		remaining time.Duration
		effort    string
		global    bool
		wantTask  time.Duration
		wantReq   time.Duration
	}{
		{name: "global long run", remaining: 45 * time.Minute, effort: "low", global: true, wantTask: 0, wantReq: 300 * time.Second},
		{name: "global half hour", remaining: 10*time.Minute + 30*time.Second, effort: "medium", global: true, wantTask: 0, wantReq: 160 * time.Second},
		{name: "global short run", remaining: 3 * time.Minute, effort: "medium", global: true, wantTask: 0, wantReq: 60 * time.Second},
		{name: "global without time", remaining: 0, global: true, wantTask: 0, wantReq: 0},
		{name: "fallback keeps the per-group minute", remaining: 10*time.Minute + 30*time.Second, effort: "medium", wantTask: 4 * time.Minute, wantReq: 160 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task, request := ocrLimits(tt.remaining, tt.effort, tt.global)
			if task != tt.wantTask || request != tt.wantReq {
				t.Fatalf("ocrLimits(%s, %q, %v) = (%s, %s), want (%s, %s)",
					tt.remaining, tt.effort, tt.global, task, request, tt.wantTask, tt.wantReq)
			}
		})
	}
}

// TestOCRTaskBudgetDerivedFromStopWindow pins the non-graceful path: OCR's own
// per-task deadline is computed from the window that ends at the hard stop, so
// its fallback always fires inside it.
func TestOCRTaskBudgetDerivedFromStopWindow(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	stop, ok := ocrStopDeadline(now, context.Background(), 10*time.Minute+30*time.Second)
	if !ok {
		t.Fatal("ocrStopDeadline had no deadline")
	}
	task, request := ocrLimits(stop.Sub(now), "medium", false)
	if task != 3*time.Minute || request != 120*time.Second {
		t.Errorf("fallback task/request = %s/%s, want 3m0s/2m0s derived from the stop window",
			task, request)
	}
}

func TestBuildReviewArgsGlobalDeadline(t *testing.T) {
	args := buildReviewArgs(nativeInvocation{
		RepoDir: "/clone", BaseRef: "base", HeadRef: "head",
		BackgroundFile: "/home/background.md", Profile: store.LLMProfile{Model: "m"},
		Effort: "low", MaxTokens: 1000, GlobalDeadline: true,
	})
	if got, ok := argValue(args, "--timeout"); !ok || got != "0" {
		t.Fatalf("--timeout = %q (present=%v), want 0 to disable the per-group deadline", got, ok)
	}
}

func TestOCRCloserRequestsCloseBeforeHardStop(t *testing.T) {
	var mu sync.Mutex
	var events []string
	hardStopped := make(chan struct{}, 1)
	closer := &ocrCloser{
		signalFn: func(*exec.Cmd) error {
			mu.Lock()
			events = append(events, "signal")
			mu.Unlock()
			return nil
		},
		killFn: func(*exec.Cmd) error {
			mu.Lock()
			events = append(events, "kill")
			mu.Unlock()
			hardStopped <- struct{}{}
			return nil
		},
	}
	closer.arm(400*time.Millisecond, 200*time.Millisecond, true)
	defer closer.stop()

	select {
	case <-hardStopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the hard stop never fired")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[0] != "signal" || events[1] != "kill" {
		t.Fatalf("close sequence = %v, want signal before the hard stop", events)
	}
	if closer.closeElapsed() <= 0 {
		t.Error("closeElapsed = 0, want the time between signal and hard stop")
	}
}

func TestOCRCloserHardStopsWithoutGrace(t *testing.T) {
	signalled := make(chan struct{}, 1)
	hardStopped := make(chan struct{}, 1)
	closer := &ocrCloser{
		signalFn: func(*exec.Cmd) error { signalled <- struct{}{}; return nil },
		killFn:   func(*exec.Cmd) error { hardStopped <- struct{}{}; return nil },
	}
	closer.arm(80*time.Millisecond, 50*time.Millisecond, false)
	defer closer.stop()

	select {
	case <-hardStopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the hard stop never fired without a graceful close")
	}
	select {
	case <-signalled:
		t.Fatal("no signal must be sent when graceful close is unavailable")
	default:
	}
}

func TestNativeConfigRequestTimeout(t *testing.T) {
	base := "https://provider.example/v1"
	profile := store.LLMProfile{Provider: "custom", Model: "m", BaseURL: &base}
	for _, tt := range []struct {
		timeout time.Duration
		want    int
	}{{90 * time.Second, 90}, {0, 0}} {
		home := t.TempDir()
		keyPath, err := writeNativeKeyHelper(home)
		if err != nil {
			t.Fatalf("writeNativeKeyHelper: %v", err)
		}
		if err := writeNativeConfig(home, profile, keyPath, tt.timeout); err != nil {
			t.Fatalf("writeNativeConfig: %v", err)
		}
		if got := readNativeConfig(t, home).TimeoutSec; got != tt.want {
			t.Errorf("timeout_sec for %s = %d, want %d", tt.timeout, got, tt.want)
		}
	}
}

// TestReviewBudgetsFromRemainingTime runs the reviewer against a fake OCR that
// records its arguments, so the budget it receives is the one derived from the
// context deadline at call time, not the run budget fixed before cloning.
func TestReviewBudgetsFromRemainingTime(t *testing.T) {
	skipWithoutShellFake(t)
	tests := []struct {
		name        string
		deadline    time.Duration
		effort      string
		wantTimeout string
		wantGlobal  string
		wantRequest int
	}{
		// The context deadline wins over the run budget. On a platform that
		// can close OCR gradually the per-task flag is explicitly disabled so
		// the groups share the run's deadline; the fallback keeps the rounded
		// per-group minute.
		{name: "context deadline wins over the run budget", deadline: 10*time.Minute + 30*time.Second, effort: "medium", wantTimeout: "3", wantGlobal: "0", wantRequest: 120},
		{name: "run budget without a context deadline", deadline: 0, effort: "low", wantTimeout: "16", wantGlobal: "0", wantRequest: 300},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reviewer := streamTestReviewer(t, nil,
				`{"status":"complete","summary":{"files_reviewed":1},"manifest":{"terminal_state":"complete"}}`, "0")
			argsFile := filepath.Join(t.TempDir(), "args")
			script, err := os.ReadFile(reviewer.Bin)
			if err != nil {
				t.Fatal(err)
			}
			recorded := strings.Replace(string(script), "#!/bin/sh\n",
				fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\n", argsFile), 1)
			if err := os.WriteFile(reviewer.Bin, []byte(recorded), 0o755); err != nil {
				t.Fatal(err)
			}
			reviewer.Effort = tt.effort
			reviewer.Timeout = 20 * time.Minute

			ctx := context.Background()
			if tt.deadline > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.deadline)
				defer cancel()
			}
			if _, _, err := reviewer.Review(ctx); err != nil {
				t.Fatalf("Review() error = %v", err)
			}

			raw, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatalf("read recorded args: %v", err)
			}
			args := strings.Split(strings.TrimSpace(string(raw)), "\n")
			got := ""
			for i, arg := range args {
				if arg == "--timeout" && i+1 < len(args) {
					got = args[i+1]
				}
			}
			want := tt.wantTimeout
			if gracefulStopSupported() {
				want = tt.wantGlobal
			}
			if got != want {
				t.Errorf("--timeout = %q, want %q (args %q)", got, want, args)
			}
			if req := readNativeConfig(t, reviewer.Home).TimeoutSec; req != tt.wantRequest {
				t.Errorf("timeout_sec = %d, want %d", req, tt.wantRequest)
			}
		})
	}
}

// streamTestReviewer returns a reviewer wired to a fake OCR script that writes
// the given stderr lines and then the given stdout body.
func streamTestReviewer(t *testing.T, stderrLines []string, stdout, exitCode string) *NativeReviewer {
	t.Helper()
	dir := t.TempDir()
	output := filepath.Join(dir, "output.json")
	if err := os.WriteFile(output, []byte(stdout), 0o644); err != nil {
		t.Fatalf("write fake output: %v", err)
	}
	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	for _, line := range stderrLines {
		fmt.Fprintf(&script, "printf '%%s\n' %q >&2\n", line)
	}
	fmt.Fprintf(&script, "cat %q\n", output)
	fmt.Fprintf(&script, "exit %s\n", exitCode)
	bin := filepath.Join(dir, "ocr.sh")
	if err := os.WriteFile(bin, []byte(script.String()), 0o755); err != nil {
		t.Fatalf("write fake ocr: %v", err)
	}
	base := "https://provider.example/v1"
	return &NativeReviewer{
		Bin:       bin,
		RepoDir:   dir,
		Home:      filepath.Join(dir, "home"),
		Profile:   store.LLMProfile{Provider: "custom", Model: "model-x", BaseURL: &base},
		BaseRef:   "base-sha",
		HeadRef:   "head-sha",
		MaxTokens: reviewContextCeiling,
	}
}

func TestReviewStreamsStderrLines(t *testing.T) {
	reviewer := streamTestReviewer(t,
		[]string{"fase: agrupando archivos", "fase: llamando al modelo"},
		`{"status":"complete","summary":{"files_reviewed":1},"manifest":{"terminal_state":"complete"}}`,
		"0")
	var mu sync.Mutex
	var lines []string
	reviewer.OnLog = func(line string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, line)
	}

	res, _, err := reviewer.Review(context.Background())
	if err != nil {
		t.Fatalf("Review() error = %v", err)
	}
	if res.Status != "complete" {
		t.Errorf("status = %q, want complete", res.Status)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 2 || lines[0] != "fase: agrupando archivos" || lines[1] != "fase: llamando al modelo" {
		t.Fatalf("streamed lines = %q, want the two stderr lines in order", lines)
	}
}

// A failing OCR that streams its reason on stderr must still report that reason
// in the error; the streaming path keeps a copy for the diagnostic.
func TestReviewStreamsStderrOnFailure(t *testing.T) {
	reviewer := streamTestReviewer(t, []string{"proveedor caído"}, "", "1")
	var got []string
	reviewer.OnLog = func(line string) { got = append(got, line) }

	_, _, err := reviewer.Review(context.Background())
	if err == nil || !strings.Contains(err.Error(), "proveedor caído") {
		t.Fatalf("Review() error = %v, want it to carry the stderr detail", err)
	}
	if len(got) != 1 || got[0] != "proveedor caído" {
		t.Errorf("streamed lines = %q, want the failure reason", got)
	}
}

func TestParseNativeReviewDetectsSummary(t *testing.T) {
	withSummary, err := parseNativeReview([]byte(`{"status": "complete", "summary": {"total_tokens": 10}}`))
	if err != nil {
		t.Fatalf("parseNativeReview(with summary): %v", err)
	}
	if !withSummary.hasSummary {
		t.Error("hasSummary = false, want true when OCR wrote a summary")
	}

	withoutSummary, err := parseNativeReview([]byte(`{"status": "complete"}`))
	if err != nil {
		t.Fatalf("parseNativeReview(without summary): %v", err)
	}
	if withoutSummary.hasSummary {
		t.Error("hasSummary = true, want false when OCR omitted the summary")
	}
}

func TestNativeFindingsKeepsExistingCode(t *testing.T) {
	findings := nativeFindings([]nativeComment{{
		Path: "a.go", Content: "finding", ExistingCode: "  if err != nil {  ",
		StartLine: 3, EndLine: 3, Severity: "medium",
	}})
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(findings))
	}
	if got := findings[0].ExistingCode; got != "if err != nil {" {
		t.Errorf("ExistingCode = %q, want the trimmed cited code", got)
	}
}
