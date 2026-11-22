package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ReviewContextCeiling bounds the global input context of one review
// conversation, mirroring Claude Code's /autocompact 270k: it limits each
// conversation, not the total spend of a run. A team policy may override it.
const (
	ReviewContextCeilingDefault = 270000
	ReviewContextCeilingMin     = 4096
	ReviewContextCeilingMax     = 2000000
)

// Config holds the runtime configuration of the QA Control Center. Load reads
// only what a process cannot discover by itself; everything else starts at its
// default and is later overwritten from the settings table.
type Config struct {
	DatabaseURL          string        // QA_DATABASE_URL (requerido)
	PublicURL            string        // public_url en la BD; el setup la detecta y la guarda
	ListenAddr           string        // listen_addr, default ":8080"
	MasterKey            []byte        // QA_MASTER_KEY base64 std, exactamente 32 bytes (opcional en Load)
	WorkerKey            []byte        // QA_WORKER_KEY base64 std, exactamente 32 bytes (requerido)
	WebhooksEnabled      bool          // webhooks_enabled, default true
	ReconcileInterval    time.Duration // reconcile_interval, default 5m
	GitHubURL            string        // github_url, default "https://github.com"
	GitHubAPIURL         string        // github_api_url, default "https://api.github.com"
	ReviewConcurrency    int           // review_concurrency, default 2
	ReviewAgentSteps     int           // review_agent_steps, default 8
	ReviewContextCeiling int           // review_context_ceiling, default 270000
	ReviewTimeout        time.Duration // review_timeout, default 20m
	ReviewStaleAfter     time.Duration // review_stale_after, default 45m
	LLMHealthInterval    time.Duration // llm_health_interval, default 6h
	WorkspaceDir         string        // workspace_dir, default "/tmp/qa"
	WorkerHealthAddr     string        // worker_health_addr, default ":9090"
	WorkerQueueStale     time.Duration // worker_queue_stale_after, default 10m
	OCRBin               string        // ocr_bin, default "ocr"
	OpencodeBin          string        // opencode_bin, default "opencode"
	FetchDepthMax        int           // fetch_depth_max, default 500
	GitBaseURL           string        // git_base_url, default "https://github.com", sin "/" final
	WorkerHealthURL      string        // worker_health_url, default "http://worker:9090", sin "/" final
	MonitorInterval      time.Duration // monitor_interval, default 1m
	MonitorRenotifyAfter time.Duration // monitor_renotify_after, default 30m
	AlertWebhookURL      string        // alert_webhook_url (opcional, vacío = solo log)
	BackupRetention      int           // backup_retention, default 7; deploy/backup.sh lee la BD (Task 7)
	QAValidationSLA      time.Duration // qa_validation_sla, default 4h
}

// RequireMasterKey validates that a master key is available. Commands that
// need it (e.g. runServe) call this explicitly; Load alone does not require it.
func (c Config) RequireMasterKey() error {
	if len(c.MasterKey) == 0 {
		return errors.New("QA_MASTER_KEY is required")
	}
	return nil
}

// LLMHealthStaleAfter is when a profile's health verdict counts as stale: two
// sweep intervals, so one delayed cycle does not mark every profile as stale.
// The zero Config used by tests falls back to the same 6h default as Load.
func (c Config) LLMHealthStaleAfter() time.Duration {
	interval := c.LLMHealthInterval
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	return 2 * interval
}

// Default returns the configuration used when nothing is configured yet. Load
// starts from it and the database overrides only what an administrator saved.
func Default() Config {
	return Config{
		ListenAddr:           ":8080",
		WebhooksEnabled:      true,
		ReconcileInterval:    5 * time.Minute,
		GitHubURL:            "https://github.com",
		GitHubAPIURL:         "https://api.github.com",
		ReviewConcurrency:    2,
		ReviewAgentSteps:     8,
		ReviewContextCeiling: ReviewContextCeilingDefault,
		ReviewTimeout:        20 * time.Minute,
		WorkspaceDir:         "/tmp/qa",
		WorkerHealthAddr:     ":9090",
		// A job waiting ten minutes with nothing running means the worker is
		// not consuming, not that the queue is merely busy.
		WorkerQueueStale: 10 * time.Minute,
		OCRBin:           "ocr",
		OpencodeBin:      "opencode",
		FetchDepthMax:    500,
		GitBaseURL:       "https://github.com",
		// Las alertas viven fuera del worker: si el worker muere, nada dentro
		// del contenedor puede avisar.
		WorkerHealthURL:      "http://worker:9090",
		MonitorInterval:      time.Minute,
		MonitorRenotifyAfter: 30 * time.Minute,
		// A review budget is 20m plus the River retry window, so a shorter
		// threshold would fail healthy runs on a slow LLM.
		ReviewStaleAfter: 45 * time.Minute,
		// A provider credential changes rarely and the probe is a single tiny
		// request, so six hours is frequent enough without polling the provider.
		LLMHealthInterval: 6 * time.Hour,
		BackupRetention:   7,
		// Half a working day: a PR ready in the morning is validated before
		// the end of the day.
		QAValidationSLA: 4 * time.Hour,
	}
}

// LegacyEnvKeys lists the variables that used to configure behaviour and are
// now ignored: they live in the database and are edited from the panel.
func LegacyEnvKeys() []string {
	return []string{
		"QA_LISTEN_ADDR", "QA_WEBHOOKS_ENABLED", "QA_RECONCILE_INTERVAL",
		"QA_GITHUB_URL", "QA_GITHUB_API_URL",
		"QA_REVIEW_CONCURRENCY", "QA_REVIEW_AGENT_STEPS", "QA_REVIEW_TIMEOUT",
		"QA_REVIEW_STALE_AFTER", "QA_LLM_HEALTH_INTERVAL", "QA_FETCH_DEPTH_MAX", "QA_GIT_BASE_URL",
		"QA_WORKSPACE_DIR", "QA_OCR_BIN", "QA_OPENCODE_BIN",
		"QA_WORKER_HEALTH_ADDR", "QA_WORKER_QUEUE_STALE_AFTER",
		"QA_WORKER_HEALTH_URL", "QA_MONITOR_INTERVAL", "QA_MONITOR_RENOTIFY_AFTER", "QA_ALERT_WEBHOOK_URL",
		"QA_BACKUP_RETENTION",
		// Set by the setup wizard from the incoming request and stored in
		// app_settings, so a process never reads it from the environment.
		"QA_PUBLIC_URL",
	}
}

// Load builds a Config from the values returned by getenv. Only the essentials
// are read: they cannot be stored in the database before connecting to it, and
// the keys decrypt it. Everything else keeps its Default until the database
// overrides it. It returns an error when an essential is missing or malformed.
func Load(getenv func(string) string) (Config, error) {
	cfg := Default()

	cfg.DatabaseURL = strings.TrimSpace(getenv("QA_DATABASE_URL"))
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("QA_DATABASE_URL is required")
	}

	if raw := strings.TrimSpace(getenv("QA_MASTER_KEY")); raw != "" {
		key, err := decodeKey(raw, "QA_MASTER_KEY")
		if err != nil {
			return Config{}, err
		}
		cfg.MasterKey = key
	}

	rawWorkerKey := strings.TrimSpace(getenv("QA_WORKER_KEY"))
	if rawWorkerKey == "" {
		return Config{}, errors.New("QA_WORKER_KEY is required")
	}
	workerKey, err := decodeKey(rawWorkerKey, "QA_WORKER_KEY")
	if err != nil {
		return Config{}, err
	}
	cfg.WorkerKey = workerKey

	return cfg, nil
}

// decodeKey enforces the key format every consumer relies on: base64 std and
// exactly 32 bytes, which is the size the ciphers take.
func decodeKey(raw, name string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%s must be valid base64: %w", name, err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%s must decode to exactly 32 bytes, got %d", name, len(key))
	}
	return key, nil
}
