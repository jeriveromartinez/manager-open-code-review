package settings

import (
	"errors"
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

	cfg = config.Default()
	err = Apply(&cfg, map[string]string{KeyMonitorRenotifyAfter: "30s", KeyMonitorInterval: "1m"})
	if !errors.As(err, &keyErr) || keyErr.Key != KeyMonitorRenotifyAfter {
		t.Fatalf("renotify < interval = %v, want KeyError on %s", err, KeyMonitorRenotifyAfter)
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
	defaults := Defaults()
	fields := Fields()
	if len(defaults) != len(fields) {
		t.Fatalf("Defaults() has %d keys, Fields() has %d", len(defaults), len(fields))
	}
	for _, field := range fields {
		if _, ok := defaults[field.Key]; !ok {
			t.Errorf("field %q missing from Defaults()", field.Key)
		}
		if field.Restart != "" && field.Restart != "app" && field.Restart != "worker" {
			t.Errorf("field %q has invalid restart %q", field.Key, field.Restart)
		}
	}
}

func TestToMapRoundTripsThroughApply(t *testing.T) {
	cfg := config.Default()
	values := ToMap(cfg)
	other := config.Config{}
	if err := Apply(&other, values); err != nil {
		t.Fatalf("Apply(ToMap(default)): %v", err)
	}
	if ToMap(other)["review_timeout"] != "20m0s" && ToMap(other)["review_timeout"] != "20m" {
		t.Errorf("ToMap round trip lost review_timeout: %q", ToMap(other)["review_timeout"])
	}
}
