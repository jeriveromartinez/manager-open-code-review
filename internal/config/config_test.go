package config_test

import (
	"bytes"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/config"
)

func testMasterKey() string {
	return base64.StdEncoding.EncodeToString(make([]byte, 32))
}

func testWorkerKey() string {
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
		"QA_WORKER_KEY":   testWorkerKey(),
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
	if cfg.WorkerHealthAddr != ":9090" {
		t.Errorf("WorkerHealthAddr = %q, want %q", cfg.WorkerHealthAddr, ":9090")
	}
	if cfg.WorkerQueueStale != 10*time.Minute {
		t.Errorf("WorkerQueueStale = %v, want %v", cfg.WorkerQueueStale, 10*time.Minute)
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
			env["QA_MASTER_KEY"] = tt.value

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

func TestLoadOnlyReadsTheEssentials(t *testing.T) {
	cfg, err := config.Load(func(key string) string {
		switch key {
		case "QA_DATABASE_URL":
			return "postgres://x/y"
		case "QA_WORKER_KEY":
			return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("w"), 32))
		case "QA_MASTER_KEY":
			return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("m"), 32))
		case "QA_REVIEW_TIMEOUT":
			return "99m"
		case "QA_PUBLIC_URL":
			return "https://ignored.example.com"
		case "QA_OCR_BIN":
			return "/nope"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ReviewTimeout != 20*time.Minute {
		t.Errorf("ReviewTimeout = %s, want the 20m default: the environment must be ignored", cfg.ReviewTimeout)
	}
	if cfg.PublicURL != "https://ignored.example.com" {
		t.Errorf("PublicURL = %q, want the provided value: it is still read until the setup detects it", cfg.PublicURL)
	}
	if cfg.OCRBin != "ocr" {
		t.Errorf("OCRBin = %q, want the default", cfg.OCRBin)
	}
}

// TestLoadIgnoresEveryLegacyKey cannot accept a silently honoured value: the
// DeepEqual, not the junk, is what proves every retired variable is ignored.
func TestLoadIgnoresEveryLegacyKey(t *testing.T) {
	env := requiredEnv()
	for _, key := range config.LegacyEnvKeys() {
		env[key] = "not a valid value for " + key
	}

	cfg, err := config.Load(getenv(env))
	if err != nil {
		t.Fatalf("Load() error = %v, want the legacy variables to be ignored", err)
	}

	essentials := cfg
	essentials.DatabaseURL = ""
	essentials.PublicURL = ""
	essentials.MasterKey = nil
	essentials.WorkerKey = nil
	if !reflect.DeepEqual(essentials, config.Default()) {
		t.Errorf("Load() = %+v, want the defaults with only the essentials set", essentials)
	}
}

func TestLegacyEnvKeysCoverEveryMovedSetting(t *testing.T) {
	keys := config.LegacyEnvKeys()
	if len(keys) != 22 {
		t.Fatalf("LegacyEnvKeys() has %d entries, want 22", len(keys))
	}
	seen := map[string]bool{}
	for _, key := range keys {
		if seen[key] {
			t.Errorf("duplicate legacy key %q", key)
		}
		seen[key] = true
	}
	for _, want := range []string{"QA_REVIEW_TIMEOUT", "QA_MONITOR_INTERVAL", "QA_WORKER_HEALTH_ADDR", "QA_BACKUP_RETENTION"} {
		if !seen[want] {
			t.Errorf("LegacyEnvKeys() missing %q", want)
		}
	}
	if seen["QA_PUBLIC_URL"] {
		t.Error("QA_PUBLIC_URL is still read until the setup detects it")
	}
	if seen["QA_DATABASE_URL"] || seen["QA_WORKER_KEY"] {
		t.Error("the essentials are not legacy")
	}
}

func TestLoadMissingWorkerKey(t *testing.T) {
	env := requiredEnv()
	delete(env, "QA_WORKER_KEY")

	_, err := config.Load(getenv(env))
	if err == nil {
		t.Fatal("Load() error = nil, want error mentioning QA_WORKER_KEY")
	}
	if !strings.Contains(err.Error(), "QA_WORKER_KEY") {
		t.Errorf("Load() error = %q, want it to mention QA_WORKER_KEY", err.Error())
	}
}

func TestLoadBadWorkerKey(t *testing.T) {
	tests := []struct {
		name   string
		value  string
		wantIn string
	}{
		{name: "not base64", value: "not base64!!", wantIn: "QA_WORKER_KEY"},
		{
			name:   "16 bytes",
			value:  base64.StdEncoding.EncodeToString(make([]byte, 16)),
			wantIn: "32 bytes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := requiredEnv()
			env["QA_WORKER_KEY"] = tt.value

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

func TestLoadWorkerWithoutMaster(t *testing.T) {
	env := requiredEnv()
	delete(env, "QA_MASTER_KEY")

	cfg, err := config.Load(getenv(env))
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if len(cfg.MasterKey) != 0 {
		t.Errorf("len(MasterKey) = %d, want 0", len(cfg.MasterKey))
	}
	if err := cfg.RequireMasterKey(); err == nil {
		t.Fatal("RequireMasterKey() error = nil, want error")
	} else if !strings.Contains(err.Error(), "QA_MASTER_KEY") {
		t.Errorf("RequireMasterKey() error = %q, want it to mention QA_MASTER_KEY", err.Error())
	}
}

func TestRequireMasterKey(t *testing.T) {
	cfg, err := config.Load(getenv(requiredEnv()))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := cfg.RequireMasterKey(); err != nil {
		t.Errorf("RequireMasterKey() error = %v, want nil", err)
	}
}

func TestLoadReviewDefaults(t *testing.T) {
	cfg, err := config.Load(getenv(requiredEnv()))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.ReviewConcurrency != 2 {
		t.Errorf("ReviewConcurrency = %d, want 2", cfg.ReviewConcurrency)
	}
	if cfg.ReviewAgentSteps != 40 {
		t.Errorf("ReviewAgentSteps = %d, want 40", cfg.ReviewAgentSteps)
	}
	if cfg.ReviewTimeout != 20*time.Minute {
		t.Errorf("ReviewTimeout = %v, want %v", cfg.ReviewTimeout, 20*time.Minute)
	}
	if cfg.ReviewStaleAfter != 45*time.Minute {
		t.Errorf("ReviewStaleAfter = %v, want %v", cfg.ReviewStaleAfter, 45*time.Minute)
	}
	if cfg.LLMHealthInterval != 30*time.Minute {
		t.Errorf("LLMHealthInterval = %v, want %v", cfg.LLMHealthInterval, 30*time.Minute)
	}
	if cfg.WorkspaceDir != "/tmp/qa" {
		t.Errorf("WorkspaceDir = %q, want %q", cfg.WorkspaceDir, "/tmp/qa")
	}
	if cfg.OCRBin != "ocr" {
		t.Errorf("OCRBin = %q, want %q", cfg.OCRBin, "ocr")
	}
	if cfg.OpencodeBin != "opencode" {
		t.Errorf("OpencodeBin = %q, want %q", cfg.OpencodeBin, "opencode")
	}
	if cfg.FetchDepthMax != 500 {
		t.Errorf("FetchDepthMax = %d, want 500", cfg.FetchDepthMax)
	}
	if cfg.GitBaseURL != "https://github.com" {
		t.Errorf("GitBaseURL = %q, want %q", cfg.GitBaseURL, "https://github.com")
	}
}
