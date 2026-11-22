package review

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

// The token bank runs the real OpenCode 1.19 binary against a fake
// OpenAI-compatible provider. It measures calls and context instead of spending
// tokens, and skips when the worker image (or a matching local binary) is not
// available. Run it from the worker image as described in docs/e2e.md.

func requireOpenCode119(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Skip("opencode is not installed; run the token bank inside qa-control-center-worker:local (see docs/e2e.md)")
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Skipf("opencode --version failed: %v; run the token bank inside the worker image", err)
	}
	version := strings.TrimSpace(string(out))
	if !strings.Contains(version, "1.19.") {
		t.Skipf("opencode %q is not 1.19.x; the token bank targets the worker image", version)
	}
	return bin
}

func bankProfile(provider *testutil.FakeProvider) store.LLMProfile {
	base := provider.URL()
	// A provider that OpenCode does not know from its catalog uses the
	// OpenAI-compatible /chat/completions path. The built-in openai provider
	// may select the Responses API instead.
	return store.LLMProfile{Provider: "commandcode", Model: "fake-model", BaseURL: &base}
}

func bankAgent(t *testing.T, bin string, provider *testutil.FakeProvider, dir string) *Agent {
	t.Helper()
	return &Agent{
		Bin:     bin,
		Dir:     dir,
		Home:    filepath.Join(dir, "home"),
		Profile: bankProfile(provider),
		APIKey:  "bank-key",
		Timeout: 2 * time.Minute,
	}
}

func bankDiff(size int) string {
	const header = "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1,1 +1,1 @@\n"
	var b strings.Builder
	b.WriteString(header)
	for b.Len() < size {
		b.WriteString("-old line\n+new line\n")
	}
	return b.String()
}

func writeBankFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func bankPrompt(t *testing.T, diffPath string) string {
	t.Helper()
	return BuildPrompt(store.PullRequest{
		Title:   "Token bank",
		BaseRef: "main",
		HeadRef: "feature",
		HeadSHA: "0123456789abcdef",
	}, nil, nil, "", diffPath)
}

// TestTokenBankNoReadsMakesOneRequest is the acceptance test for #70: a pass
// that does not need to read the repository must cost exactly one model call.
// Before the title agent is disabled, OpenCode adds a second request that sends
// the prompt and the attached diff again.
func TestTokenBankNoReadsMakesOneRequest(t *testing.T) {
	bin := requireOpenCode119(t)
	provider := testutil.NewFakeProvider(t, func(testutil.ProviderRequest) testutil.ProviderReply {
		return testutil.ProviderReply{
			Text: `{"summary":"ok","coverage":{"reviewed_files":1,"skipped_files":0},"previous_findings":[],"findings":[]}`,
		}
	})
	dir := t.TempDir()
	diffPath := filepath.Join(dir, "review.patch")
	writeBankFile(t, diffPath, bankDiff(30*1024))
	writeBankFile(t, filepath.Join(dir, "main.go"), "package main\n")

	agent := bankAgent(t, bin, provider, dir)
	text, _, err := agent.Run(context.Background(), bankPrompt(t, diffPath), []string{diffPath})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(text, `"summary"`) {
		t.Fatalf("agent text = %q, want the JSON answer", text)
	}

	requests := provider.Requests()
	if len(requests) != 1 {
		t.Fatalf("chat requests = %d, want 1 (the title call must not exist)", len(requests))
	}
	if got := requests[0].Tools; len(got) != 1 || got[0] != "read" {
		t.Errorf("declared tools = %v, want only read", got)
	}
	if requests[0].EstimatedInputTokens > 270_000 {
		t.Errorf("first request context = %d tokens, want at most 270000", requests[0].EstimatedInputTokens)
	}
}

// TestTokenBankSendsStablePromptCacheKey is the #66 check: OpenCode only sends
// a prompt cache key for a fixed list of provider SDKs, so the generated config
// opts in explicitly. Every step must reuse the same key, and the provider's
// cached_tokens must reach the run usage instead of being lost.
func TestTokenBankSendsStablePromptCacheKey(t *testing.T) {
	bin := requireOpenCode119(t)
	dir := t.TempDir()
	writeBankFile(t, filepath.Join(dir, "main.go"), "package main\n")
	diffPath := filepath.Join(dir, "review.patch")
	writeBankFile(t, diffPath, bankDiff(8*1024))

	provider := testutil.NewFakeProvider(t, func(req testutil.ProviderRequest) testutil.ProviderReply {
		if req.ToolResultCount() == 0 {
			return testutil.ProviderReply{ToolCalls: []testutil.ProviderToolCall{{
				ID:        "read-0",
				Name:      "read",
				Arguments: fmt.Sprintf(`{"filePath":%q}`, filepath.Join(dir, "main.go")),
			}}}
		}
		return testutil.ProviderReply{
			Text:  `{"summary":"ok","coverage":{"reviewed_files":1,"skipped_files":0},"previous_findings":[],"findings":[]}`,
			Usage: testutil.ProviderUsage{CachedTokens: 900},
		}
	})

	agent := bankAgent(t, bin, provider, dir)
	_, rawUsage, err := agent.Run(context.Background(), bankPrompt(t, diffPath), []string{diffPath})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var usage tokenUsage
	if err := json.Unmarshal(rawUsage, &usage); err != nil {
		t.Fatalf("decode usage %q: %v", rawUsage, err)
	}

	requests := provider.Requests()
	if len(requests) != 2 {
		t.Fatalf("chat requests = %d, want 2 (one read plus the answer)", len(requests))
	}
	key := requests[0].PromptCacheKey
	if key == "" {
		t.Fatal("first request has no prompt_cache_key; the model option did not reach the request body")
	}
	for i, req := range requests[1:] {
		if req.PromptCacheKey != key {
			t.Errorf("request %d cache key = %q, want the run key %q", i+1, req.PromptCacheKey, key)
		}
	}
	if usage.Cache.Read != 900 {
		t.Errorf("run cache read = %d, want the 900 cached tokens reported by the provider", usage.Cache.Read)
	}
}

// TestTokenBankStopsAtStepBudget verifies that the default cap bounds a loop
// that never produces a final answer. Before #70 the cap was 40 steps; the
// oversized history then exceeded the context ceiling.
func TestTokenBankStopsAtStepBudget(t *testing.T) {
	bin := requireOpenCode119(t)
	dir := t.TempDir()
	const files = 40
	for i := 0; i < files; i++ {
		writeBankFile(t, filepath.Join(dir, fmt.Sprintf("bank-%02d.go", i)), bankSource(16*1024))
	}
	diffPath := filepath.Join(dir, "review.patch")
	writeBankFile(t, diffPath, bankDiff(30*1024))

	provider := testutil.NewFakeProvider(t, func(req testutil.ProviderRequest) testutil.ProviderReply {
		// OpenCode appends this prompt on the last allowed step. A real model
		// is expected to answer with text; the fake one must do the same or the
		// loop would keep calling tools and the bank would measure nothing.
		if bankLastStep(req) {
			return testutil.ProviderReply{Text: `{"summary":"ok","coverage":{"reviewed_files":40,"skipped_files":0},"previous_findings":[],"findings":[]}`}
		}
		n := req.ToolResultCount()
		return testutil.ProviderReply{ToolCalls: []testutil.ProviderToolCall{{
			ID:   fmt.Sprintf("read-%d", n),
			Name: "read",
			Arguments: fmt.Sprintf(`{"filePath":%q}`,
				filepath.Join(dir, fmt.Sprintf("bank-%02d.go", n))),
		}}}
	})

	agent := bankAgent(t, bin, provider, dir)
	_, _, _ = agent.Run(context.Background(), bankPrompt(t, diffPath), []string{diffPath})

	requests := provider.Requests()
	if len(requests) > 8 {
		t.Fatalf("chat requests = %d, want at most 8 (the default step cap)", len(requests))
	}
	largest := 0
	for _, req := range requests {
		if req.EstimatedInputTokens > largest {
			largest = req.EstimatedInputTokens
		}
	}
	if largest > 270_000 {
		t.Errorf("largest request context = %d tokens, want at most 270000", largest)
	}
	t.Logf("step-budget bank: requests=%d max_context=%d total=%d", len(requests), largest, provider.TotalEstimatedTokens())
}

// TestTokenBankReferenceFigures reproduces the diagnosis from #76 with the same
// scripted loop: 12 file reads with a 30 KB diff. The tolerance is wide because
// the token estimate is byte-based; the point is to catch an order-of-magnitude
// regression, not to pin an exact provider count.
func TestTokenBankReferenceFigures(t *testing.T) {
	bin := requireOpenCode119(t)
	dir := t.TempDir()
	const reads = 12
	for i := 0; i < reads; i++ {
		writeBankFile(t, filepath.Join(dir, fmt.Sprintf("bank-%02d.go", i)), bankSource(12*1024))
	}
	diffPath := filepath.Join(dir, "review.patch")
	writeBankFile(t, diffPath, bankDiff(30*1024))

	provider := testutil.NewFakeProvider(t, func(req testutil.ProviderRequest) testutil.ProviderReply {
		n := req.ToolResultCount()
		if n >= reads {
			return testutil.ProviderReply{Text: `{"summary":"ok","coverage":{"reviewed_files":12,"skipped_files":0},"previous_findings":[],"findings":[]}`}
		}
		return testutil.ProviderReply{ToolCalls: []testutil.ProviderToolCall{{
			ID:   fmt.Sprintf("read-%d", n),
			Name: "read",
			Arguments: fmt.Sprintf(`{"filePath":%q}`,
				filepath.Join(dir, fmt.Sprintf("bank-%02d.go", n))),
		}}}
	})

	agent := bankAgent(t, bin, provider, dir)
	agent.Steps = 40
	_, _, err := agent.Run(context.Background(), bankPrompt(t, diffPath), []string{diffPath})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	requests := provider.Requests()
	if len(requests) != reads+1 {
		t.Fatalf("chat requests = %d, want %d", len(requests), reads+1)
	}
	total := provider.TotalEstimatedTokens()
	if total < 200_000 || total > 500_000 {
		t.Fatalf("reference total = %d estimated tokens, want roughly 327000 (200000-500000)", total)
	}
	largest := 0
	for _, req := range requests {
		if req.EstimatedInputTokens > largest {
			largest = req.EstimatedInputTokens
		}
	}
	if largest > 270_000 {
		t.Errorf("largest request context = %d tokens, want at most 270000", largest)
	}
	t.Logf("reference bank: requests=%d max_context=%d total=%d", len(requests), largest, total)
}

func bankLastStep(req testutil.ProviderRequest) bool {
	for _, message := range req.Messages {
		if strings.Contains(message.Content, "MAXIMUM STEPS REACHED") {
			return true
		}
	}
	return false
}

func bankSource(size int) string {
	var b strings.Builder
	b.WriteString("package main\n\n")
	for b.Len() < size {
		b.WriteString("// padding line for the token bank\n")
	}
	return b.String()
}
