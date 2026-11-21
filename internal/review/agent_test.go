package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func fakeOpencodeBin(t *testing.T) string {
	t.Helper()
	bin, err := filepath.Abs("testdata/fakeopencode.sh")
	if err != nil {
		t.Fatalf("resolve fake opencode: %v", err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("fake opencode %q: %v", bin, err)
	}
	return bin
}

func testAgent(t *testing.T, home string) *Agent {
	t.Helper()
	return &Agent{
		Bin:     fakeOpencodeBin(t),
		Dir:     t.TempDir(),
		Home:    home,
		Profile: store.LLMProfile{Provider: "nvidia", Model: "nemotron"},
		APIKey:  "child-secret-key",
		Timeout: 5 * time.Second,
	}
}

func TestAgentEnvKeepsKeyOutOfParent(t *testing.T) {
	previous, existed := os.LookupEnv("QA_LLM_API_KEY")
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv("QA_LLM_API_KEY", previous)
		} else {
			_ = os.Unsetenv("QA_LLM_API_KEY")
		}
	})
	if err := os.Unsetenv("QA_LLM_API_KEY"); err != nil {
		t.Fatalf("unset parent key: %v", err)
	}

	a := testAgent(t, t.TempDir())
	text, _, err := a.Run(context.Background(), "__FAKE_ENV__", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(text, "QA_LLM_API_KEY=child-secret-key") {
		t.Fatalf("child did not receive the API key in its env: %q", text)
	}
	if !strings.Contains(text, "HOME="+a.Home) {
		t.Fatalf("child HOME is not the isolated home: %q", text)
	}
	if _, ok := os.LookupEnv("QA_LLM_API_KEY"); ok {
		t.Fatal("Run mutated the parent QA_LLM_API_KEY")
	}
}

func TestAgentParsesTextEvents(t *testing.T) {
	a := testAgent(t, t.TempDir())
	text, usage, err := a.Run(context.Background(), "__FAKE_EVENTS__", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := `{"ok":true}`; text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
	if len(usage) == 0 || !strings.Contains(string(usage), `"total":26385`) {
		t.Fatalf("usage = %s, want the step_finish token object", usage)
	}
}

func TestAgentErrorDetailFromEventStream(t *testing.T) {
	raw := strings.Join([]string{
		`{"type":"text","text":"ignored"}`,
		`{"type":"error","error":{"name":"AI_APICallError","data":{"message":"401 Unauthorized"}}}`,
		`not json`,
	}, "\n")
	if got := agentErrorDetail([]byte(raw)); got != "AI_APICallError: 401 Unauthorized" {
		t.Errorf("agentErrorDetail() = %q", got)
	}
	if got := agentErrorDetail([]byte(`{"type":"text","text":"hola"}`)); got != "" {
		t.Errorf("agentErrorDetail(no error) = %q, want empty", got)
	}
}

// TestAgentErrorDetailStructured covers the richer error.data shapes emitted
// by OpenCode 1.19 provider errors. Each case asserts the operator-facing words
// that make the failure actionable instead of the generic UnknownError text.
func TestAgentErrorDetailStructured(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "provider request authentication",
			raw: `{"type":"error","error":{"name":"ProviderRequestError","data":` +
				`{"providerID":"nvidia","reason":"authentication","message":"invalid api key","status":401}}}`,
			want: []string{"credencial", "HTTP 401", "API key"},
		},
		{
			name: "model not found",
			raw: `{"type":"error","error":{"name":"ModelNotFoundError","data":` +
				`{"providerID":"nvidia","modelID":"nemotron","message":"model not found"}}}`,
			want: []string{"modelo", "nvidia/nemotron"},
		},
		{
			name: "provider not found",
			raw: `{"type":"error","error":{"name":"ProviderNotFoundError","data":` +
				`{"providerID":"acme","message":"unknown provider"}}}`,
			want: []string{"proveedor", "acme", "soportados"},
		},
		{
			name: "rate limit",
			raw: `{"type":"error","error":{"name":"ProviderRequestError","data":` +
				`{"providerID":"openai","reason":"rate_limit","message":"too many requests","status":429,"retryAfterMs":1500}}}`,
			want: []string{"cuota", "HTTP 429", "1500"},
		},
		{
			name: "unknown error with ref",
			raw: `{"type":"error","error":{"name":"UnknownError","data":` +
				`{"message":"Unexpected server error. Check server logs for details.","ref":"err_abc123"}}}`,
			want: []string{"UnknownError", "Unexpected server error", "ref", "err_abc123", "API key"},
		},
		{
			name: "network dns failure",
			raw: `{"type":"error","error":{"name":"UnknownError","data":` +
				`{"message":"dial tcp: lookup api.example.test: no such host"}}}`,
			want: []string{"red/DNS", "no such host"},
		},
		{
			name: "provider overloaded",
			raw: `{"type":"error","error":{"name":"ProviderRequestError","data":` +
				`{"providerID":"nvidia","reason":"overloaded","message":"busy","status":503}}}`,
			want: []string{"no disponible", "HTTP 503"},
		},
		{
			name: "invalid request",
			raw: `{"type":"error","error":{"name":"ProviderRequestError","data":` +
				`{"providerID":"nvidia","reason":"invalid_request","message":"bad model","status":400,"responseBody":{"error":"bad model"},"url":"https://api.example.test/v1/chat"}}}`,
			want: []string{"rechazó", "HTTP 400", "bad model", "api.example.test"},
		},
		{
			name: "plain fallback stays unchanged",
			raw:  `{"type":"error","error":{"name":"AI_APICallError","data":{"message":"401 Unauthorized"}}}`,
			want: []string{"AI_APICallError: 401 Unauthorized"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := agentErrorDetail([]byte(tc.raw))
			for _, fragment := range tc.want {
				if !strings.Contains(got, fragment) {
					t.Errorf("agentErrorDetail() = %q, want it to contain %q", got, fragment)
				}
			}
			if len(got) > maxAgentErrorDetail {
				t.Errorf("agentErrorDetail() has %d chars, want at most %d", len(got), maxAgentErrorDetail)
			}
		})
	}
}

func TestAgentKillsProcessGroupOnTimeout(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	a := testAgent(t, t.TempDir())
	a.Timeout = 100 * time.Millisecond

	_, _, err := a.Run(context.Background(), "__FAKE_SLEEP__="+pidFile, nil)
	if err == nil {
		t.Fatal("Run returned nil error on timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}

	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse child pid %q: %v", data, err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processRunning(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child pid %d survived the process-group kill", pid)
}

// processRunning reports whether pid is a live (non-zombie) process on Unix.
func processRunning(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	state := strings.TrimSpace(string(out))
	return state != "" && !strings.HasPrefix(state, "Z")
}

func TestAgentConfigDeniesTools(t *testing.T) {
	t.Run("with base URL", func(t *testing.T) {
		home := t.TempDir()
		a := testAgent(t, home)
		a.Profile.BaseURL = stringPtr("https://llm.example.test/v1")

		if _, _, err := a.Run(context.Background(), "__FAKE_EVENTS__", nil); err != nil {
			t.Fatalf("Run: %v", err)
		}

		cfg := readAgentConfig(t, home)
		provider, ok := cfg.Provider["nvidia"]
		if !ok {
			t.Fatalf("provider has no nvidia entry: %+v", cfg.Provider)
		}
		if provider.Options.APIKey != "{env:QA_LLM_API_KEY}" {
			t.Fatalf("apiKey = %q, want env placeholder", provider.Options.APIKey)
		}
		if provider.Options.BaseURL != "https://llm.example.test/v1" {
			t.Fatalf("baseURL = %q", provider.Options.BaseURL)
		}
		agent, ok := cfg.Agent["qa-reviewer"]
		if !ok {
			t.Fatalf("agents has no qa-reviewer entry: %+v", cfg.Agent)
		}
		denied := []string{"edit", "bash", "task", "webfetch", "websearch", "external_directory"}
		for _, action := range denied {
			if got := agent.Permission[action]; got != "deny" {
				t.Errorf("agent permission %q = %q, want deny", action, got)
			}
			if got := cfg.Permission[action]; got != "deny" {
				t.Errorf("global permission %q = %q, want deny", action, got)
			}
		}
		if got := agent.Permission["read"]; got != "allow" {
			t.Errorf("agent permission read = %q, want allow", got)
		}
		if got := cfg.Permission["read"]; got != "allow" {
			t.Errorf("global permission read = %q, want allow", got)
		}
	})

	t.Run("without base URL", func(t *testing.T) {
		home := t.TempDir()
		a := testAgent(t, home)
		if _, _, err := a.Run(context.Background(), "__FAKE_EVENTS__", nil); err != nil {
			t.Fatalf("Run: %v", err)
		}
		cfg := readAgentConfig(t, home)
		if got := cfg.Provider["nvidia"].Options.BaseURL; got != "" {
			t.Fatalf("baseURL = %q, want empty when profile has none", got)
		}
	})
}

// TestAgentConfigPlatformBaseURL covers providers that OpenCode 1.19 does not
// know from its catalog (CommandCode, for example). Without options.baseURL the
// engine builds "undefined/chat/completions" and the health check only reports a
// generic UnknownError, so the fixed platform endpoint must be written.
func TestAgentConfigPlatformBaseURL(t *testing.T) {
	t.Run("platform provider gets its fixed endpoint", func(t *testing.T) {
		home := t.TempDir()
		a := testAgent(t, home)
		a.Profile.Provider = "commandcode"
		a.Profile.Model = "deepseek/deepseek-v4.1-flash"

		if _, _, err := a.Run(context.Background(), "__FAKE_EVENTS__", nil); err != nil {
			t.Fatalf("Run: %v", err)
		}

		cfg := readAgentConfig(t, home)
		want := "https://api.commandcode.ai/provider/v1"
		if got := cfg.Provider["commandcode"].Options.BaseURL; got != want {
			t.Fatalf("baseURL = %q, want %q", got, want)
		}
	})

	t.Run("explicit profile base URL wins", func(t *testing.T) {
		home := t.TempDir()
		a := testAgent(t, home)
		a.Profile.Provider = "commandcode"
		a.Profile.Model = "deepseek/deepseek-v4.1-flash"
		a.Profile.BaseURL = stringPtr("https://gateway.example.test/v1")

		if _, _, err := a.Run(context.Background(), "__FAKE_EVENTS__", nil); err != nil {
			t.Fatalf("Run: %v", err)
		}

		cfg := readAgentConfig(t, home)
		if got := cfg.Provider["commandcode"].Options.BaseURL; got != "https://gateway.example.test/v1" {
			t.Fatalf("baseURL = %q, want the profile override", got)
		}
	})
}

func TestAgentConfigStepBudget(t *testing.T) {
	home := t.TempDir()
	a := testAgent(t, home)
	if _, _, err := a.Run(context.Background(), "__FAKE_EVENTS__", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw := readAgentConfigRaw(t, home)

	var cfg struct {
		Agent map[string]map[string]json.RawMessage `json:"agent"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode generated config: %v\n%s", err, raw)
	}
	reviewer, ok := cfg.Agent["qa-reviewer"]
	if !ok {
		t.Fatalf("agent has no qa-reviewer entry: %s", raw)
	}

	steps, ok := reviewer["steps"]
	if !ok {
		t.Fatalf("agent.qa-reviewer has no steps field: %s", raw)
	}
	var value int
	if err := json.Unmarshal(steps, &value); err != nil {
		t.Fatalf("agent.qa-reviewer.steps is not an integer: %s", steps)
	}
	if value != defaultReviewerSteps {
		t.Errorf("agent.qa-reviewer.steps = %d, want %d", value, defaultReviewerSteps)
	}
	if value <= 0 || value > 200 {
		t.Errorf("agent.qa-reviewer.steps = %d, want a per-pass cap in the tens", value)
	}
	if _, deprecated := reviewer["maxSteps"]; deprecated {
		t.Errorf("generated config must not emit the deprecated maxSteps: %s", raw)
	}
}

func TestAgentConfigStepBudgetFromAgent(t *testing.T) {
	home := t.TempDir()
	a := testAgent(t, home)
	a.Steps = 17
	if _, _, err := a.Run(context.Background(), "__FAKE_EVENTS__", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var cfg struct {
		Agent map[string]struct {
			Steps int `json:"steps"`
		} `json:"agent"`
	}
	raw := readAgentConfigRaw(t, home)
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode generated config: %v\n%s", err, raw)
	}
	if got := cfg.Agent["qa-reviewer"].Steps; got != 17 {
		t.Errorf("agent.qa-reviewer.steps = %d, want 17", got)
	}
}

func TestAgentCommandFlags(t *testing.T) {
	a := testAgent(t, t.TempDir())
	a.Profile.Variant = stringPtr("fast")

	text, _, err := a.Run(context.Background(), "__FAKE_ARGS__", []string{"a.go", "b.go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := fmt.Sprintf("run --pure --format json --agent qa-reviewer --model nvidia/nemotron --variant fast --dir %s __FAKE_ARGS__ -f a.go b.go", a.Dir)
	if text != want {
		t.Fatalf("argv = %q, want %q", text, want)
	}
}

type generatedConfig struct {
	Provider map[string]struct {
		Options struct {
			APIKey  string `json:"apiKey"`
			BaseURL string `json:"baseURL"`
		} `json:"options"`
	} `json:"provider"`
	Permission map[string]string `json:"permission"`
	Agent      map[string]struct {
		Permission map[string]string `json:"permission"`
	} `json:"agent"`
}

func readAgentConfig(t *testing.T, home string) generatedConfig {
	t.Helper()
	var cfg generatedConfig
	if err := json.Unmarshal(readAgentConfigRaw(t, home), &cfg); err != nil {
		t.Fatalf("generated config is not JSON: %v", err)
	}
	return cfg
}

func readAgentConfigRaw(t *testing.T, home string) []byte {
	t.Helper()
	path := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated config %q: %v", path, err)
	}
	return raw
}

func TestAgentConfigDeclaresSelectedModel(t *testing.T) {
	home := t.TempDir()
	a := testAgent(t, home)
	if _, _, err := a.Run(context.Background(), "__FAKE_ARGS__", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var cfg struct {
		Provider map[string]struct {
			Models map[string]struct {
				Name string `json:"name"`
			} `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(readAgentConfigRaw(t, home), &cfg); err != nil {
		t.Fatalf("decode generated config: %v", err)
	}
	model, ok := cfg.Provider["nvidia"].Models["nemotron"]
	if !ok || model.Name != "nemotron" {
		t.Fatalf("provider.nvidia.models = %#v, want nemotron", cfg.Provider["nvidia"].Models)
	}
}

func stringPtr(s string) *string { return &s }
