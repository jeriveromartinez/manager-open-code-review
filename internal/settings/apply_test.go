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
		{"zero monitor interval", KeyMonitorInterval, "0s", KeyMonitorInterval},
		{"negative monitor interval", KeyMonitorInterval, "-1m", KeyMonitorInterval},
		{"zero renotify window", KeyMonitorRenotifyAfter, "0s", KeyMonitorRenotifyAfter},
		{"negative renotify window", KeyMonitorRenotifyAfter, "-1m", KeyMonitorRenotifyAfter},
		{"garbage duration", KeyReconcileInterval, "pronto", KeyReconcileInterval},
		{"negative int", KeyReviewConcurrency, "-1", KeyReviewConcurrency},
		{"garbage int", KeyFetchDepthMax, "muchos", KeyFetchDepthMax},
		{"context ceiling below range", KeyReviewContextCeiling, "4095", KeyReviewContextCeiling},
		{"context ceiling above range", KeyReviewContextCeiling, "2000001", KeyReviewContextCeiling},
		{"context ceiling garbage", KeyReviewContextCeiling, "270k", KeyReviewContextCeiling},
		{"retention above the backup script's range", KeyBackupRetention, "1000", KeyBackupRetention},
		{"bad bool", KeyWebhooksEnabled, "si", KeyWebhooksEnabled},
		{"relative url", KeyPublicURL, "/solo-ruta", KeyPublicURL},
		{"relative worker health url", KeyWorkerHealthURL, "worker:9090", KeyWorkerHealthURL},
		{"non-http worker health url", KeyWorkerHealthURL, "ftp://worker:9090", KeyWorkerHealthURL},
		{"worker health url without host", KeyWorkerHealthURL, "http://", KeyWorkerHealthURL},
		{"relative webhook url", KeyAlertWebhookURL, "alerts.example.com/hook", KeyAlertWebhookURL},
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

// backup.sh honours 1..999 and falls back to 7 for anything else, so the panel
// must not store a value the nightly job would silently replace.
func TestApplyAcceptsTheBackupRetentionBoundary(t *testing.T) {
	cfg := config.Default()
	if err := Apply(&cfg, map[string]string{KeyBackupRetention: "999"}); err != nil {
		t.Fatalf("Apply(backup_retention=999) = %v, want it accepted", err)
	}
	if cfg.BackupRetention != 999 {
		t.Errorf("BackupRetention = %d, want 999", cfg.BackupRetention)
	}
}

// The pair rules also reject some negative durations (e.g. a negative renotify
// window is below every interval), which would hide a setDuration that lost its
// own check. Pin the branch directly.
func TestSetDurationRejectsNonPositive(t *testing.T) {
	for _, raw := range []string{"0s", "-1m"} {
		var d time.Duration
		if err := setDuration(&d, KeyMonitorInterval, raw); err == nil {
			t.Errorf("setDuration(%q) = %v, want an error", raw, d)
		}
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
		KeyReviewContextCeiling: "300000",
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
		KeyQAValidationSLA:      "6h",
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
		cfg.WorkerQueueStale != 20*time.Minute || cfg.MonitorRenotifyAfter != 5*time.Minute ||
		cfg.MonitorInterval != 30*time.Second || cfg.QAValidationSLA != 6*time.Hour {
		t.Error("one of the durations was not applied")
	}
	if cfg.ReviewConcurrency != 4 || cfg.ReviewAgentSteps != 60 || cfg.ReviewContextCeiling != 300000 ||
		cfg.FetchDepthMax != 900 || cfg.BackupRetention != 14 {
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
	if len(fields) != 25 {
		t.Fatalf("Fields() has %d keys, want 25", len(fields))
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
		KeyPublicURL:            "app",
		KeyWebhooksEnabled:      "app",
		KeyWorkerHealthURL:      "monitor",
		KeyMonitorInterval:      "monitor",
		KeyMonitorRenotifyAfter: "monitor",
		KeyAlertWebhookURL:      "monitor",
		KeyBackupRetention:      "",
		KeyQAValidationSLA:      "app",
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
