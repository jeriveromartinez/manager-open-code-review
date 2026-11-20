package settings

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/config"
)

func TestApplyRejectsInvalidValuesByKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
		raw  string
		want string
	}{
		{"zero duration", KeyReviewTimeout, "0s", KeyReviewTimeout},
		{"garbage duration", KeyReconcileInterval, "pronto", KeyReconcileInterval},
		{"negative int", KeyReviewConcurrency, "-1", KeyReviewConcurrency},
		{"garbage int", KeyFetchDepthMax, "muchos", KeyFetchDepthMax},
		{"bad bool", KeyWebhooksEnabled, "si", KeyWebhooksEnabled},
		{"relative url", KeyPublicURL, "/solo-ruta", KeyPublicURL},
		{"url trimmed to empty", KeyGitBaseURL, "/", KeyGitBaseURL},
		{"empty bin", KeyOCRBin, "  ", KeyOCRBin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			err := Apply(&cfg, map[string]string{tc.key: tc.raw})
			var keyErr *KeyError
			if !errors.As(err, &keyErr) {
				t.Fatalf("Apply(%s=%q) = %v, want *KeyError", tc.key, tc.raw, err)
			}
			if keyErr.Key != tc.want {
				t.Errorf("KeyError.Key = %q, want %q", keyErr.Key, tc.want)
			}
		})
	}
}

func TestApplyRejectsIncoherentPairs(t *testing.T) {
	cfg := config.Default()
	err := Apply(&cfg, map[string]string{KeyReviewTimeout: "60m"})
	var keyErr *KeyError
	if !errors.As(err, &keyErr) || keyErr.Key != KeyReviewStaleAfter {
		t.Fatalf("timeout 60m with stale 45m = %v, want KeyError on %s", err, KeyReviewStaleAfter)
	}

	// Equality is the whole point of the rule: the watchdog would kill a run at
	// the same instant the review's own timeout expires.
	cfg = config.Default()
	err = Apply(&cfg, map[string]string{KeyReviewTimeout: "45m"})
	if !errors.As(err, &keyErr) || keyErr.Key != KeyReviewStaleAfter {
		t.Fatalf("timeout 45m with stale 45m = %v, want KeyError on %s", err, KeyReviewStaleAfter)
	}

	cfg = config.Default()
	err = Apply(&cfg, map[string]string{KeyMonitorRenotifyAfter: "30s", KeyMonitorInterval: "1m"})
	if !errors.As(err, &keyErr) || keyErr.Key != KeyMonitorRenotifyAfter {
		t.Fatalf("renotify < interval = %v, want KeyError on %s", err, KeyMonitorRenotifyAfter)
	}
}

func TestApplyRejectsUnknownKeys(t *testing.T) {
	cfg := config.Default()
	err := Apply(&cfg, map[string]string{"review_timout": "30m"})
	var keyErr *KeyError
	if !errors.As(err, &keyErr) || keyErr.Key != "review_timout" {
		t.Fatalf("Apply with a typo = %v, want KeyError naming the key", err)
	}
}

func TestApplyClearsTheOptionalWebhook(t *testing.T) {
	cfg := config.Default()
	cfg.AlertWebhookURL = "https://hooks.example.com/old"
	if err := Apply(&cfg, map[string]string{KeyAlertWebhookURL: "  "}); err != nil {
		t.Fatalf("clearing alert_webhook_url = %v, want it accepted: an empty webhook means log only", err)
	}
	if cfg.AlertWebhookURL != "" {
		t.Errorf("AlertWebhookURL = %q, want empty", cfg.AlertWebhookURL)
	}
}

func TestApplySetsEveryField(t *testing.T) {
	cfg := config.Default()
	err := Apply(&cfg, map[string]string{
		KeyPublicURL:            "https://qa.example.com/",
		KeyWebhooksEnabled:      "false",
		KeyReconcileInterval:    "2m",
		KeyGitHubURL:            "https://ghe.example.com/",
		KeyGitHubAPIURL:         "https://ghe.example.com/api/v3/",
		KeyListenAddr:           ":9091",
		KeyReviewConcurrency:    "4",
		KeyReviewAgentSteps:     "60",
		KeyReviewTimeout:        "30m",
		KeyReviewStaleAfter:     "90m",
		KeyLLMHealthInterval:    "10m",
		KeyFetchDepthMax:        "900",
		KeyGitBaseURL:           "https://git.example.com/",
		KeyWorkspaceDir:         "/var/qa",
		KeyOCRBin:               "/usr/local/bin/ocr",
		KeyOpencodeBin:          "/usr/local/bin/opencode",
		KeyWorkerHealthAddr:     "127.0.0.1:9092",
		KeyWorkerQueueStale:     "20m",
		KeyWorkerHealthURL:      "http://worker:9090/",
		KeyMonitorInterval:      "30s",
		KeyMonitorRenotifyAfter: "5m",
		KeyAlertWebhookURL:      "https://hooks.example.com/qa",
		KeyBackupRetention:      "14",
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if cfg.PublicURL != "https://qa.example.com" {
		t.Errorf("PublicURL = %q", cfg.PublicURL)
	}
	if cfg.WebhooksEnabled {
		t.Error("WebhooksEnabled = true, want false")
	}
	if cfg.ReconcileInterval != 2*time.Minute || cfg.ReviewTimeout != 30*time.Minute ||
		cfg.ReviewStaleAfter != 90*time.Minute || cfg.LLMHealthInterval != 10*time.Minute ||
		cfg.WorkerQueueStale != 20*time.Minute || cfg.MonitorRenotifyAfter != 5*time.Minute {
		t.Error("one of the durations was not applied")
	}
	if cfg.ReviewConcurrency != 4 || cfg.ReviewAgentSteps != 60 || cfg.FetchDepthMax != 900 || cfg.BackupRetention != 14 {
		t.Error("one of the integers was not applied")
	}
	if cfg.WorkerHealthURL != "http://worker:9090" || cfg.GitHubURL != "https://ghe.example.com" ||
		cfg.GitHubAPIURL != "https://ghe.example.com/api/v3" || cfg.GitBaseURL != "https://git.example.com" {
		t.Error("one of the URLs was not trimmed")
	}
	if cfg.WorkspaceDir != "/var/qa" || cfg.OCRBin != "/usr/local/bin/ocr" ||
		cfg.OpencodeBin != "/usr/local/bin/opencode" || cfg.ListenAddr != ":9091" ||
		cfg.WorkerHealthAddr != "127.0.0.1:9092" || cfg.AlertWebhookURL != "https://hooks.example.com/qa" {
		t.Error("one of the strings was not applied")
	}
}

func TestDefaultsAndFieldsAgree(t *testing.T) {
	valid := map[string]bool{"": true, "app": true, "worker": true, "monitor": true}
	fields := Fields()
	if len(fields) != 23 {
		t.Fatalf("Fields() has %d keys, want 23", len(fields))
	}
	seen := map[string]bool{}
	for _, field := range fields {
		if seen[field.Key] {
			t.Errorf("duplicate key %q", field.Key)
		}
		seen[field.Key] = true
		if !valid[field.Restart] {
			t.Errorf("field %q has invalid restart %q", field.Key, field.Restart)
		}
	}
}

func TestRestartNamesTheProcessThatReadsTheKey(t *testing.T) {
	want := map[string]string{
		KeyWorkerHealthURL:      "monitor",
		KeyMonitorInterval:      "monitor",
		KeyMonitorRenotifyAfter: "monitor",
		KeyAlertWebhookURL:      "monitor",
		KeyBackupRetention:      "",
	}
	for _, field := range Fields() {
		if target, ok := want[field.Key]; ok && field.Restart != target {
			t.Errorf("%q restarts %q, want %q", field.Key, field.Restart, target)
		}
	}
}

// TestDefaultsMatchConfigDefault is the only thing that keeps the panel's
// defaults and the process's defaults from drifting: Fields() cannot be derived
// from config.Default() without importing every consumer, so a mistake in one
// place has to fail here instead of silently changing behaviour on an install
// that never saved anything.
func TestDefaultsMatchConfigDefault(t *testing.T) {
	stored := Defaults()
	effective := ToMap(config.Default())
	for key, want := range effective {
		raw, ok := stored[key]
		if !ok {
			t.Errorf("%q is in config.Default() but not in Fields()", key)
			continue
		}
		if !equalSetting(raw, want) {
			t.Errorf("%q default = %q, config.Default() uses %q", key, raw, want)
		}
	}
	for key := range stored {
		if _, ok := effective[key]; !ok {
			t.Errorf("%q is in Fields() but not in config.Default()", key)
		}
	}
}

// equalSetting compares durations and integers by value, not by spelling:
// "20m" and "20m0s" are the same setting, and both forms reach the panel.
func equalSetting(a, b string) bool {
	if d, err := time.ParseDuration(a); err == nil {
		if other, err := time.ParseDuration(b); err == nil {
			return d == other
		}
	}
	if n, err := strconv.Atoi(a); err == nil {
		if other, err := strconv.Atoi(b); err == nil {
			return n == other
		}
	}
	return a == b
}

func TestToMapRoundTripsThroughApply(t *testing.T) {
	values := ToMap(config.Default())
	other := config.Config{}
	if err := Apply(&other, values); err != nil {
		t.Fatalf("Apply(ToMap(default)): %v", err)
	}
	if got, want := ToMap(other)["review_timeout"], values["review_timeout"]; got != want {
		t.Errorf("ToMap round trip changed review_timeout from %q to %q", want, got)
	}
}
