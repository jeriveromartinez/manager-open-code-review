package review

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func fakeOCRBin(t *testing.T) string {
	t.Helper()
	bin, err := filepath.Abs(filepath.Join("testdata", "fakeocr.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatal(err)
	}
	return bin
}

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
		Timeout:        90 * time.Second,
	})
	want := []string{
		"review",
		"--repo", "/work/run-1",
		"--from", "base-sha",
		"--to", "head-sha",
		"--format", "json",
		"--audience", "agent",
		"--provider", nativeProviderName,
		"--model", "model-x",
		"--effort", "high",
		"--max-tokens", "270000",
		"--background-file", "/work/run-1/home-ocr/background.md",
		"--timeout", "2",
		"--exclude", "vendor/**,**/*.pb.go",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args =\n%q\nwant\n%q", got, want)
	}

	withoutTimeout := buildReviewArgs(nativeInvocation{
		RepoDir: "/r", BaseRef: "a", HeadRef: "b", BackgroundFile: "/bg",
		Profile: store.LLMProfile{Model: "m"}, Effort: "low", MaxTokens: 1,
	})
	for _, arg := range withoutTimeout {
		if arg == "--timeout" {
			t.Fatalf("unexpected --timeout in %q", withoutTimeout)
		}
	}
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
		want        string
		code        string
	}{
		{name: "complete", status: "complete", want: store.RunSucceeded},
		{name: "skipped", status: "skipped", want: store.RunSucceeded},
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
			name: "failed timeout", status: "failed",
			failed: []nativeCoverageItem{{Classification: "timeout"}},
			want:   store.RunInconclusive, code: "timeout",
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
	}{
		{
			file: "ocr-review-complete.json", state: "complete", comments: 2,
			reviewed: 2, excluded: 1, totalTokens: 4960, status: store.RunSucceeded,
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
			reviewed: 0, skipped: 0, excluded: 1, status: store.RunSucceeded,
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
		})
	}
}

func TestNativeTitleFingerprintStable(t *testing.T) {
	comment := nativeComment{
		Path: "src/a.go", Content: "Nil pointer dereference\n\nmore detail",
		StartLine: 7, EndLine: 7, Category: "bug", Severity: "high",
	}
	first := mergeNativeFindings([]nativeComment{comment}).Findings
	second := mergeNativeFindings([]nativeComment{comment}).Findings
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("findings = %d/%d, want 1/1", len(first), len(second))
	}
	fp1, fp2 := Fingerprint(first[0], nil), Fingerprint(second[0], nil)
	if fp1 != fp2 {
		t.Fatalf("fingerprint is not deterministic: %q != %q", fp1, fp2)
	}
	// OCR's title is derived from the first non-empty line; surrounding
	// whitespace must not fork the fingerprint.
	spaced := comment
	spaced.Content = "  Nil pointer dereference  \n\ndifferent details"
	if got := Fingerprint(mergeNativeFindings([]nativeComment{spaced}).Findings[0], nil); got != fp1 {
		t.Errorf("spaced title fingerprint = %q, want %q", got, fp1)
	}
}

func TestNativeConfigKeepsKeyOffDisk(t *testing.T) {
	home := t.TempDir()
	base := "https://provider.example/v1"
	profile := store.LLMProfile{Provider: "custom", Model: "model-x", BaseURL: &base}
	keyPath, err := writeNativeKeyHelper(home)
	if err != nil {
		t.Fatalf("writeNativeKeyHelper: %v", err)
	}
	if err := writeNativeConfig(home, profile, keyPath); err != nil {
		t.Fatalf("writeNativeConfig: %v", err)
	}
	configPath := filepath.Join(home, ".opencodereview", "config.json")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-secret") {
		t.Fatalf("config contains the API key: %s", raw)
	}
	for _, want := range []string{`"api_key_cmd"`, `"prompt_cache_key": "{ocr_session_key}"`, base, "model-x"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("config missing %q: %s", want, raw)
		}
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
