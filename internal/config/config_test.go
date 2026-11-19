package config_test

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/config"
)

func testMasterKey() string {
	return base64.StdEncoding.EncodeToString(make([]byte, 32))
}

func getenv(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

func requiredEnv() map[string]string {
	return map[string]string{
		"QA_DATABASE_URL": "postgres://localhost/qa",
		"QA_PUBLIC_URL":   "https://qa.example.com",
		"QA_MASTER_KEY":   testMasterKey(),
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := config.Load(getenv(requiredEnv()))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, ":8080")
	}
	if cfg.ReconcileInterval != 5*time.Minute {
		t.Errorf("ReconcileInterval = %v, want %v", cfg.ReconcileInterval, 5*time.Minute)
	}
	if !cfg.WebhooksEnabled {
		t.Error("WebhooksEnabled = false, want true")
	}
	if cfg.GitHubURL != "https://github.com" {
		t.Errorf("GitHubURL = %q, want %q", cfg.GitHubURL, "https://github.com")
	}
	if cfg.GitHubAPIURL != "https://api.github.com" {
		t.Errorf("GitHubAPIURL = %q, want %q", cfg.GitHubAPIURL, "https://api.github.com")
	}
	if len(cfg.MasterKey) != 32 {
		t.Errorf("len(MasterKey) = %d, want 32", len(cfg.MasterKey))
	}
	if cfg.DatabaseURL != "postgres://localhost/qa" {
		t.Errorf("DatabaseURL = %q, want %q", cfg.DatabaseURL, "postgres://localhost/qa")
	}
}

func TestLoadMissingRequired(t *testing.T) {
	tests := []struct {
		name    string
		missing string
	}{
		{name: "database url", missing: "QA_DATABASE_URL"},
		{name: "public url", missing: "QA_PUBLIC_URL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := requiredEnv()
			delete(env, tt.missing)

			_, err := config.Load(getenv(env))
			if err == nil {
				t.Fatalf("Load() error = nil, want error mentioning %s", tt.missing)
			}
			if !strings.Contains(err.Error(), tt.missing) {
				t.Errorf("Load() error = %q, want it to mention %s", err.Error(), tt.missing)
			}
		})
	}
}

func TestLoadBadMasterKey(t *testing.T) {
	tests := []struct {
		name   string
		value  string
		wantIn string
	}{
		{name: "missing", value: "", wantIn: "QA_MASTER_KEY"},
		{name: "not base64", value: "not base64!!", wantIn: "QA_MASTER_KEY"},
		{
			name:   "16 bytes",
			value:  base64.StdEncoding.EncodeToString(make([]byte, 16)),
			wantIn: "32 bytes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := requiredEnv()
			if tt.value == "" {
				delete(env, "QA_MASTER_KEY")
			} else {
				env["QA_MASTER_KEY"] = tt.value
			}

			_, err := config.Load(getenv(env))
			if err == nil {
				t.Fatalf("Load() error = nil, want error mentioning %s", tt.wantIn)
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("Load() error = %q, want it to mention %s", err.Error(), tt.wantIn)
			}
		})
	}
}

func TestLoadTrimsPublicURL(t *testing.T) {
	env := requiredEnv()
	env["QA_PUBLIC_URL"] = "https://qa.example.com/"

	cfg, err := config.Load(getenv(env))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.PublicURL != "https://qa.example.com" {
		t.Errorf("PublicURL = %q, want %q", cfg.PublicURL, "https://qa.example.com")
	}
}

func TestLoadOverrides(t *testing.T) {
	env := requiredEnv()
	env["QA_LISTEN_ADDR"] = "127.0.0.1:9090"
	env["QA_WEBHOOKS_ENABLED"] = "false"
	env["QA_RECONCILE_INTERVAL"] = "30s"
	env["QA_GITHUB_URL"] = "https://ghe.example.com"
	env["QA_GITHUB_API_URL"] = "https://ghe.example.com/api/v3"

	cfg, err := config.Load(getenv(env))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.ListenAddr != "127.0.0.1:9090" {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, "127.0.0.1:9090")
	}
	if cfg.WebhooksEnabled {
		t.Error("WebhooksEnabled = true, want false")
	}
	if cfg.ReconcileInterval != 30*time.Second {
		t.Errorf("ReconcileInterval = %v, want %v", cfg.ReconcileInterval, 30*time.Second)
	}
	if cfg.GitHubURL != "https://ghe.example.com" {
		t.Errorf("GitHubURL = %q, want %q", cfg.GitHubURL, "https://ghe.example.com")
	}
	if cfg.GitHubAPIURL != "https://ghe.example.com/api/v3" {
		t.Errorf("GitHubAPIURL = %q, want %q", cfg.GitHubAPIURL, "https://ghe.example.com/api/v3")
	}
}

func TestLoadBadReconcileInterval(t *testing.T) {
	env := requiredEnv()
	env["QA_RECONCILE_INTERVAL"] = "10 minutos"

	_, err := config.Load(getenv(env))
	if err == nil {
		t.Fatal("Load() error = nil, want error mentioning QA_RECONCILE_INTERVAL")
	}
	if !strings.Contains(err.Error(), "QA_RECONCILE_INTERVAL") {
		t.Errorf("Load() error = %q, want it to mention QA_RECONCILE_INTERVAL", err.Error())
	}
}
