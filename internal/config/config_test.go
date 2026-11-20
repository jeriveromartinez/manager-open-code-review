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

func TestLoadOverrides(t *testing.T) {
	env := requiredEnv()
	env["QA_LISTEN_ADDR"] = "127.0.0.1:9090"
	env["QA_WEBHOOKS_ENABLED"] = "false"
	env["QA_RECONCILE_INTERVAL"] = "30s"
	env["QA_GITHUB_URL"] = "https://ghe.example.com"
	env["QA_GITHUB_API_URL"] = "https://ghe.example.com/api/v3"
	env["QA_WORKER_HEALTH_ADDR"] = "127.0.0.1:9091"
	env["QA_WORKER_QUEUE_STALE_AFTER"] = "2m"

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
	if cfg.WorkerHealthAddr != "127.0.0.1:9091" {
		t.Errorf("WorkerHealthAddr = %q, want %q", cfg.WorkerHealthAddr, "127.0.0.1:9091")
	}
	if cfg.WorkerQueueStale != 2*time.Minute {
		t.Errorf("WorkerQueueStale = %v, want %v", cfg.WorkerQueueStale, 2*time.Minute)
	}
}

func TestLoadBadWorkerQueueStaleAfter(t *testing.T) {
	for _, value := range []string{"nope", "0s", "-5m"} {
		t.Run(value, func(t *testing.T) {
			env := requiredEnv()
			env["QA_WORKER_QUEUE_STALE_AFTER"] = value
			if _, err := config.Load(getenv(env)); err == nil {
				t.Fatalf("Load() with %q: esperaba error", value)
			}
		})
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

func TestLoadNonPositiveReconcileInterval(t *testing.T) {
	for _, value := range []string{"0s", "-1m"} {
		t.Run(value, func(t *testing.T) {
			env := requiredEnv()
			env["QA_RECONCILE_INTERVAL"] = value

			_, err := config.Load(getenv(env))
			if err == nil {
				t.Fatalf("Load() error = nil for %q, want error mentioning QA_RECONCILE_INTERVAL", value)
			}
			if !strings.Contains(err.Error(), "QA_RECONCILE_INTERVAL") {
				t.Errorf("Load() error = %q, want it to mention QA_RECONCILE_INTERVAL", err.Error())
			}
		})
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

func TestLoadReviewOverrides(t *testing.T) {
	env := requiredEnv()
	env["QA_REVIEW_CONCURRENCY"] = "8"
	env["QA_REVIEW_AGENT_STEPS"] = "12"
	env["QA_REVIEW_TIMEOUT"] = "45s"
	env["QA_REVIEW_STALE_AFTER"] = "2h"
	env["QA_WORKSPACE_DIR"] = "/var/qa"
	env["QA_OCR_BIN"] = "/usr/bin/ocr"
	env["QA_OPENCODE_BIN"] = "/usr/bin/opencode"
	env["QA_FETCH_DEPTH_MAX"] = "1000"
	env["QA_GIT_BASE_URL"] = "https://ghe.example.com/"

	cfg, err := config.Load(getenv(env))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.ReviewConcurrency != 8 {
		t.Errorf("ReviewConcurrency = %d, want 8", cfg.ReviewConcurrency)
	}
	if cfg.ReviewAgentSteps != 12 {
		t.Errorf("ReviewAgentSteps = %d, want 12", cfg.ReviewAgentSteps)
	}
	if cfg.ReviewTimeout != 45*time.Second {
		t.Errorf("ReviewTimeout = %v, want %v", cfg.ReviewTimeout, 45*time.Second)
	}
	if cfg.ReviewStaleAfter != 2*time.Hour {
		t.Errorf("ReviewStaleAfter = %v, want %v", cfg.ReviewStaleAfter, 2*time.Hour)
	}
	if cfg.WorkspaceDir != "/var/qa" {
		t.Errorf("WorkspaceDir = %q, want %q", cfg.WorkspaceDir, "/var/qa")
	}
	if cfg.OCRBin != "/usr/bin/ocr" {
		t.Errorf("OCRBin = %q, want %q", cfg.OCRBin, "/usr/bin/ocr")
	}
	if cfg.OpencodeBin != "/usr/bin/opencode" {
		t.Errorf("OpencodeBin = %q, want %q", cfg.OpencodeBin, "/usr/bin/opencode")
	}
	if cfg.FetchDepthMax != 1000 {
		t.Errorf("FetchDepthMax = %d, want %d", cfg.FetchDepthMax, 1000)
	}
	if cfg.GitBaseURL != "https://ghe.example.com" {
		t.Errorf("GitBaseURL = %q, want %q (no trailing slash)", cfg.GitBaseURL, "https://ghe.example.com")
	}
}

func TestLoadBadReviewConcurrency(t *testing.T) {
	for _, value := range []string{"0", "-1", "x"} {
		t.Run(value, func(t *testing.T) {
			env := requiredEnv()
			env["QA_REVIEW_CONCURRENCY"] = value

			_, err := config.Load(getenv(env))
			if err == nil {
				t.Fatalf("Load() error = nil for %q, want error mentioning QA_REVIEW_CONCURRENCY", value)
			}
			if !strings.Contains(err.Error(), "QA_REVIEW_CONCURRENCY") {
				t.Errorf("Load() error = %q, want it to mention QA_REVIEW_CONCURRENCY", err.Error())
			}
		})
	}
}

func TestLoadBadReviewAgentSteps(t *testing.T) {
	for _, value := range []string{"0", "-1", "x"} {
		t.Run(value, func(t *testing.T) {
			env := requiredEnv()
			env["QA_REVIEW_AGENT_STEPS"] = value

			_, err := config.Load(getenv(env))
			if err == nil {
				t.Fatalf("Load() error = nil for %q, want error mentioning QA_REVIEW_AGENT_STEPS", value)
			}
			if !strings.Contains(err.Error(), "QA_REVIEW_AGENT_STEPS") {
				t.Errorf("Load() error = %q, want it to mention QA_REVIEW_AGENT_STEPS", err.Error())
			}
		})
	}
}

func TestLoadBadReviewTimeout(t *testing.T) {
	for _, value := range []string{"0s", "-1m", "10 minutos"} {
		t.Run(value, func(t *testing.T) {
			env := requiredEnv()
			env["QA_REVIEW_TIMEOUT"] = value

			_, err := config.Load(getenv(env))
			if err == nil {
				t.Fatalf("Load() error = nil for %q, want error mentioning QA_REVIEW_TIMEOUT", value)
			}
			if !strings.Contains(err.Error(), "QA_REVIEW_TIMEOUT") {
				t.Errorf("Load() error = %q, want it to mention QA_REVIEW_TIMEOUT", err.Error())
			}
		})
	}
}

func TestLoadBadReviewStaleAfter(t *testing.T) {
	for _, value := range []string{"0s", "-1m", "una hora"} {
		t.Run(value, func(t *testing.T) {
			env := requiredEnv()
			env["QA_REVIEW_STALE_AFTER"] = value

			_, err := config.Load(getenv(env))
			if err == nil {
				t.Fatalf("Load() error = nil for %q, want error mentioning QA_REVIEW_STALE_AFTER", value)
			}
			if !strings.Contains(err.Error(), "QA_REVIEW_STALE_AFTER") {
				t.Errorf("Load() error = %q, want it to mention QA_REVIEW_STALE_AFTER", err.Error())
			}
		})
	}
}

func TestLoadBadFetchDepthMax(t *testing.T) {
	for _, value := range []string{"0", "-1", "x"} {
		t.Run(value, func(t *testing.T) {
			env := requiredEnv()
			env["QA_FETCH_DEPTH_MAX"] = value

			_, err := config.Load(getenv(env))
			if err == nil {
				t.Fatalf("Load() error = nil for %q, want error mentioning QA_FETCH_DEPTH_MAX", value)
			}
			if !strings.Contains(err.Error(), "QA_FETCH_DEPTH_MAX") {
				t.Errorf("Load() error = %q, want it to mention QA_FETCH_DEPTH_MAX", err.Error())
			}
		})
	}
}

func TestLoadEmptyReviewPaths(t *testing.T) {
	for _, key := range []string{"QA_WORKSPACE_DIR", "QA_OCR_BIN", "QA_OPENCODE_BIN"} {
		t.Run(key, func(t *testing.T) {
			env := requiredEnv()
			env[key] = "   "

			_, err := config.Load(getenv(env))
			if err == nil {
				t.Fatalf("Load() error = nil for %q, want error mentioning %s", key, key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("Load() error = %q, want it to mention %s", err.Error(), key)
			}
		})
	}
}

func TestLoadEmptyGitBaseURL(t *testing.T) {
	env := requiredEnv()
	env["QA_GIT_BASE_URL"] = "/"

	_, err := config.Load(getenv(env))
	if err == nil {
		t.Fatal("Load() error = nil, want error mentioning QA_GIT_BASE_URL")
	}
	if !strings.Contains(err.Error(), "QA_GIT_BASE_URL") {
		t.Errorf("Load() error = %q, want it to mention QA_GIT_BASE_URL", err.Error())
	}
}
