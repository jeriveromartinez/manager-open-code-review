package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
		provider, ok := cfg.Providers["nvidia"]
		if !ok {
			t.Fatalf("providers has no nvidia entry: %+v", cfg.Providers)
		}
		if !slices.Contains(provider.Env, "QA_LLM_API_KEY") {
			t.Fatalf("provider env = %v, want QA_LLM_API_KEY", provider.Env)
		}
		if provider.Options.BaseURL != "https://llm.example.test/v1" {
			t.Fatalf("baseURL = %q", provider.Options.BaseURL)
		}
		agent, ok := cfg.Agent["qa-reviewer"]
		if !ok {
			t.Fatalf("agents has no qa-reviewer entry: %+v", cfg.Agent)
		}
		for _, action := range []string{"edit", "bash", "webfetch", "external_directory"} {
			if got := agent.Permission[action]; got != "deny" {
				t.Errorf("permission %q = %q, want deny", action, got)
			}
		}
		if got := agent.Permission["read"]; got != "allow" {
			t.Errorf("permission read = %q, want allow", got)
		}
	})

	t.Run("without base URL", func(t *testing.T) {
		home := t.TempDir()
		a := testAgent(t, home)
		if _, _, err := a.Run(context.Background(), "__FAKE_EVENTS__", nil); err != nil {
			t.Fatalf("Run: %v", err)
		}
		cfg := readAgentConfig(t, home)
		if got := cfg.Providers["nvidia"].Options.BaseURL; got != "" {
			t.Fatalf("baseURL = %q, want empty when profile has none", got)
		}
	})
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
	Providers map[string]struct {
		Env     []string `json:"env"`
		Options struct {
			BaseURL string `json:"baseURL"`
		} `json:"options"`
	} `json:"providers"`
	Agent map[string]struct {
		Permission map[string]string `json:"permission"`
	} `json:"agent"`
}

func readAgentConfig(t *testing.T, home string) generatedConfig {
	t.Helper()
	path := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated config %q: %v", path, err)
	}
	var cfg generatedConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("generated config is not JSON: %v\n%s", err, raw)
	}
	return cfg
}

func stringPtr(s string) *string { return &s }
