package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config holds the runtime configuration of the QA Control Center, loaded from
// environment variables.
type Config struct {
	DatabaseURL          string        // QA_DATABASE_URL (requerido)
	PublicURL            string        // QA_PUBLIC_URL (requerido, sin "/" final)
	ListenAddr           string        // QA_LISTEN_ADDR, default ":8080"
	MasterKey            []byte        // QA_MASTER_KEY base64 std, exactamente 32 bytes (opcional en Load)
	WorkerKey            []byte        // QA_WORKER_KEY base64 std, exactamente 32 bytes (requerido)
	WebhooksEnabled      bool          // QA_WEBHOOKS_ENABLED, default true
	ReconcileInterval    time.Duration // QA_RECONCILE_INTERVAL, default 5m
	GitHubURL            string        // QA_GITHUB_URL, default "https://github.com"
	GitHubAPIURL         string        // QA_GITHUB_API_URL, default "https://api.github.com"
	ReviewConcurrency    int           // QA_REVIEW_CONCURRENCY, default 2
	ReviewAgentSteps     int           // QA_REVIEW_AGENT_STEPS, default 40
	ReviewTimeout        time.Duration // QA_REVIEW_TIMEOUT, default 20m
	ReviewStaleAfter     time.Duration // QA_REVIEW_STALE_AFTER, default 45m
	LLMHealthInterval    time.Duration // QA_LLM_HEALTH_INTERVAL, default 30m
	WorkspaceDir         string        // QA_WORKSPACE_DIR, default "/tmp/qa"
	WorkerHealthAddr     string        // QA_WORKER_HEALTH_ADDR, default ":9090"
	WorkerQueueStale     time.Duration // QA_WORKER_QUEUE_STALE_AFTER, default 10m
	OCRBin               string        // QA_OCR_BIN, default "ocr"
	OpencodeBin          string        // QA_OPENCODE_BIN, default "opencode"
	FetchDepthMax        int           // QA_FETCH_DEPTH_MAX, default 500
	GitBaseURL           string        // QA_GIT_BASE_URL, default "https://github.com", sin "/" final
	WorkerHealthURL      string        // QA_WORKER_HEALTH_URL, default "http://worker:9090", sin "/" final
	MonitorInterval      time.Duration // QA_MONITOR_INTERVAL, default 1m
	MonitorRenotifyAfter time.Duration // QA_MONITOR_RENOTIFY_AFTER, default 30m
	AlertWebhookURL      string        // QA_ALERT_WEBHOOK_URL (opcional, vacío = solo log)
}

// MonitorConfig is what `qa monitor` needs. It is deliberately separate from
// Config: the monitor only speaks HTTP, so requiring the database URL and the
// worker key (as Load does for the other commands) would make it unusable as
// its own container.
type MonitorConfig struct {
	WorkerHealthURL string
	Interval        time.Duration
	RenotifyAfter   time.Duration
	AlertWebhookURL string
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
// The zero Config used by tests falls back to the same 30m default as Load.
func (c Config) LLMHealthStaleAfter() time.Duration {
	interval := c.LLMHealthInterval
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	return 2 * interval
}

// LoadMonitor builds only the monitor settings. `qa monitor` has no database
// nor keys to validate, so it never calls Load: that would reject a perfectly
// working monitor container for missing QA_WORKER_KEY.
func LoadMonitor(getenv func(string) string) (MonitorConfig, error) {
	url, interval, renotify, webhook, err := loadMonitorSettings(getenv)
	if err != nil {
		return MonitorConfig{}, err
	}
	return MonitorConfig{
		WorkerHealthURL: url,
		Interval:        interval,
		RenotifyAfter:   renotify,
		AlertWebhookURL: webhook,
	}, nil
}

// loadMonitorSettings is shared by Load and LoadMonitor so both commands read
// the same variables with the same rules.
func loadMonitorSettings(getenv func(string) string) (string, time.Duration, time.Duration, string, error) {
	healthURL := "http://worker:9090"
	if raw := getenv("QA_WORKER_HEALTH_URL"); raw != "" {
		v := strings.TrimRight(strings.TrimSpace(raw), "/")
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "", 0, 0, "", fmt.Errorf("QA_WORKER_HEALTH_URL must be an absolute http(s) URL, got %q", raw)
		}
		healthURL = v
	}

	interval := time.Minute
	if v := strings.TrimSpace(getenv("QA_MONITOR_INTERVAL")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return "", 0, 0, "", fmt.Errorf("QA_MONITOR_INTERVAL must be a duration: %w", err)
		}
		if d <= 0 {
			return "", 0, 0, "", fmt.Errorf("QA_MONITOR_INTERVAL must be greater than zero, got %q", v)
		}
		interval = d
	}

	renotify := 30 * time.Minute
	if v := strings.TrimSpace(getenv("QA_MONITOR_RENOTIFY_AFTER")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return "", 0, 0, "", fmt.Errorf("QA_MONITOR_RENOTIFY_AFTER must be a duration: %w", err)
		}
		if d <= 0 {
			return "", 0, 0, "", fmt.Errorf("QA_MONITOR_RENOTIFY_AFTER must be greater than zero, got %q", v)
		}
		renotify = d
	}
	// Re-notifying faster than the poll interval would make every tick an
	// alert, which is the same as having no state machine at all.
	if renotify < interval {
		return "", 0, 0, "", fmt.Errorf(
			"QA_MONITOR_RENOTIFY_AFTER must be greater than or equal to QA_MONITOR_INTERVAL, got %s < %s",
			renotify, interval)
	}

	webhook := strings.TrimSpace(getenv("QA_ALERT_WEBHOOK_URL"))
	if webhook != "" {
		u, err := url.Parse(webhook)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "", 0, 0, "", fmt.Errorf("QA_ALERT_WEBHOOK_URL must be an absolute http(s) URL, got %q", webhook)
		}
	}

	return healthURL, interval, renotify, webhook, nil
}

// Load builds a Config from the values returned by getenv. It returns an error
// when a required variable is missing or malformed.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		ListenAddr:        ":8080",
		WebhooksEnabled:   true,
		ReconcileInterval: 5 * time.Minute,
		GitHubURL:         "https://github.com",
		GitHubAPIURL:      "https://api.github.com",
		ReviewConcurrency: 2,
		ReviewAgentSteps:  40,
		ReviewTimeout:     20 * time.Minute,
		WorkspaceDir:      "/tmp/qa",
		WorkerHealthAddr:  ":9090",
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
		// A provider credential changes rarely, so half an hour is frequent
		// enough to notice a broken key without polling the provider.
		LLMHealthInterval: 30 * time.Minute,
	}

	cfg.DatabaseURL = strings.TrimSpace(getenv("QA_DATABASE_URL"))
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("QA_DATABASE_URL is required")
	}

	cfg.PublicURL = strings.TrimRight(strings.TrimSpace(getenv("QA_PUBLIC_URL")), "/")
	if cfg.PublicURL == "" {
		return Config{}, errors.New("QA_PUBLIC_URL is required")
	}

	if rawMasterKey := strings.TrimSpace(getenv("QA_MASTER_KEY")); rawMasterKey != "" {
		masterKey, err := base64.StdEncoding.DecodeString(rawMasterKey)
		if err != nil {
			return Config{}, fmt.Errorf("QA_MASTER_KEY must be valid base64: %w", err)
		}
		if len(masterKey) != 32 {
			return Config{}, fmt.Errorf("QA_MASTER_KEY must decode to exactly 32 bytes, got %d", len(masterKey))
		}
		cfg.MasterKey = masterKey
	}

	rawWorkerKey := strings.TrimSpace(getenv("QA_WORKER_KEY"))
	if rawWorkerKey == "" {
		return Config{}, errors.New("QA_WORKER_KEY is required")
	}
	workerKey, err := base64.StdEncoding.DecodeString(rawWorkerKey)
	if err != nil {
		return Config{}, fmt.Errorf("QA_WORKER_KEY must be valid base64: %w", err)
	}
	if len(workerKey) != 32 {
		return Config{}, fmt.Errorf("QA_WORKER_KEY must decode to exactly 32 bytes, got %d", len(workerKey))
	}
	cfg.WorkerKey = workerKey

	if v := strings.TrimSpace(getenv("QA_LISTEN_ADDR")); v != "" {
		cfg.ListenAddr = v
	}

	if v := strings.TrimSpace(getenv("QA_WEBHOOKS_ENABLED")); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("QA_WEBHOOKS_ENABLED must be a boolean: %w", err)
		}
		cfg.WebhooksEnabled = enabled
	}

	if v := strings.TrimSpace(getenv("QA_RECONCILE_INTERVAL")); v != "" {
		interval, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("QA_RECONCILE_INTERVAL must be a duration: %w", err)
		}
		if interval <= 0 {
			return Config{}, fmt.Errorf("QA_RECONCILE_INTERVAL must be greater than zero, got %q", v)
		}
		cfg.ReconcileInterval = interval
	}

	if v := strings.TrimSpace(getenv("QA_GITHUB_URL")); v != "" {
		cfg.GitHubURL = strings.TrimRight(v, "/")
	}

	if v := strings.TrimSpace(getenv("QA_GITHUB_API_URL")); v != "" {
		cfg.GitHubAPIURL = strings.TrimRight(v, "/")
	}

	if v := strings.TrimSpace(getenv("QA_REVIEW_CONCURRENCY")); v != "" {
		concurrency, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("QA_REVIEW_CONCURRENCY must be an integer: %w", err)
		}
		if concurrency <= 0 {
			return Config{}, fmt.Errorf("QA_REVIEW_CONCURRENCY must be greater than zero, got %q", v)
		}
		cfg.ReviewConcurrency = concurrency
	}

	if v := strings.TrimSpace(getenv("QA_REVIEW_AGENT_STEPS")); v != "" {
		steps, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("QA_REVIEW_AGENT_STEPS must be an integer: %w", err)
		}
		if steps <= 0 {
			return Config{}, fmt.Errorf("QA_REVIEW_AGENT_STEPS must be greater than zero, got %q", v)
		}
		cfg.ReviewAgentSteps = steps
	}

	if v := strings.TrimSpace(getenv("QA_REVIEW_TIMEOUT")); v != "" {
		timeout, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("QA_REVIEW_TIMEOUT must be a duration: %w", err)
		}
		if timeout <= 0 {
			return Config{}, fmt.Errorf("QA_REVIEW_TIMEOUT must be greater than zero, got %q", v)
		}
		cfg.ReviewTimeout = timeout
	}

	if v := strings.TrimSpace(getenv("QA_REVIEW_STALE_AFTER")); v != "" {
		stale, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("QA_REVIEW_STALE_AFTER must be a duration: %w", err)
		}
		if stale <= 0 {
			return Config{}, fmt.Errorf("QA_REVIEW_STALE_AFTER must be greater than zero, got %q", v)
		}
		cfg.ReviewStaleAfter = stale
	}

	if v := strings.TrimSpace(getenv("QA_LLM_HEALTH_INTERVAL")); v != "" {
		interval, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("QA_LLM_HEALTH_INTERVAL must be a duration: %w", err)
		}
		if interval <= 0 {
			return Config{}, fmt.Errorf("QA_LLM_HEALTH_INTERVAL must be greater than zero, got %q", v)
		}
		cfg.LLMHealthInterval = interval
	}

	if raw := getenv("QA_WORKSPACE_DIR"); raw != "" {
		v := strings.TrimSpace(raw)
		if v == "" {
			return Config{}, errors.New("QA_WORKSPACE_DIR must not be empty")
		}
		cfg.WorkspaceDir = v
	}

	if raw := getenv("QA_WORKER_HEALTH_ADDR"); raw != "" {
		v := strings.TrimSpace(raw)
		if v == "" {
			return Config{}, errors.New("QA_WORKER_HEALTH_ADDR must not be empty")
		}
		cfg.WorkerHealthAddr = v
	}

	if raw := getenv("QA_WORKER_QUEUE_STALE_AFTER"); raw != "" {
		v := strings.TrimSpace(raw)
		stale, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("QA_WORKER_QUEUE_STALE_AFTER must be a duration: %w", err)
		}
		if stale <= 0 {
			return Config{}, fmt.Errorf("QA_WORKER_QUEUE_STALE_AFTER must be greater than zero, got %q", v)
		}
		cfg.WorkerQueueStale = stale
	}

	if raw := getenv("QA_OCR_BIN"); raw != "" {
		v := strings.TrimSpace(raw)
		if v == "" {
			return Config{}, errors.New("QA_OCR_BIN must not be empty")
		}
		cfg.OCRBin = v
	}

	if raw := getenv("QA_OPENCODE_BIN"); raw != "" {
		v := strings.TrimSpace(raw)
		if v == "" {
			return Config{}, errors.New("QA_OPENCODE_BIN must not be empty")
		}
		cfg.OpencodeBin = v
	}

	if v := strings.TrimSpace(getenv("QA_FETCH_DEPTH_MAX")); v != "" {
		depth, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("QA_FETCH_DEPTH_MAX must be an integer: %w", err)
		}
		if depth <= 0 {
			return Config{}, fmt.Errorf("QA_FETCH_DEPTH_MAX must be greater than zero, got %q", v)
		}
		cfg.FetchDepthMax = depth
	}

	if raw := getenv("QA_GIT_BASE_URL"); raw != "" {
		v := strings.TrimRight(strings.TrimSpace(raw), "/")
		if v == "" {
			return Config{}, errors.New("QA_GIT_BASE_URL must not be empty")
		}
		cfg.GitBaseURL = v
	}

	healthURL, interval, renotify, webhook, err := loadMonitorSettings(getenv)
	if err != nil {
		return Config{}, err
	}
	cfg.WorkerHealthURL = healthURL
	cfg.MonitorInterval = interval
	cfg.MonitorRenotifyAfter = renotify
	cfg.AlertWebhookURL = webhook

	return cfg, nil
}
