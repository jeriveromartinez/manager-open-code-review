package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Config holds the runtime configuration of the QA Control Center, loaded from
// environment variables.
type Config struct {
	DatabaseURL       string        // QA_DATABASE_URL (requerido)
	PublicURL         string        // QA_PUBLIC_URL (requerido, sin "/" final)
	ListenAddr        string        // QA_LISTEN_ADDR, default ":8080"
	MasterKey         []byte        // QA_MASTER_KEY base64 std, exactamente 32 bytes (opcional en Load)
	WorkerKey         []byte        // QA_WORKER_KEY base64 std, exactamente 32 bytes (requerido)
	WebhooksEnabled   bool          // QA_WEBHOOKS_ENABLED, default true
	ReconcileInterval time.Duration // QA_RECONCILE_INTERVAL, default 5m
	GitHubURL         string        // QA_GITHUB_URL, default "https://github.com"
	GitHubAPIURL      string        // QA_GITHUB_API_URL, default "https://api.github.com"
	ReviewConcurrency int           // QA_REVIEW_CONCURRENCY, default 2
	ReviewTimeout     time.Duration // QA_REVIEW_TIMEOUT, default 20m
	WorkspaceDir      string        // QA_WORKSPACE_DIR, default "/tmp/qa"
	OCRBin            string        // QA_OCR_BIN, default "ocr"
	OpencodeBin       string        // QA_OPENCODE_BIN, default "opencode"
	FetchDepthMax     int           // QA_FETCH_DEPTH_MAX, default 500
	GitBaseURL        string        // QA_GIT_BASE_URL, default "https://github.com", sin "/" final
}

// RequireMasterKey validates that a master key is available. Commands that
// need it (e.g. runServe) call this explicitly; Load alone does not require it.
func (c Config) RequireMasterKey() error {
	if len(c.MasterKey) == 0 {
		return errors.New("QA_MASTER_KEY is required")
	}
	return nil
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
		ReviewTimeout:     20 * time.Minute,
		WorkspaceDir:      "/tmp/qa",
		OCRBin:            "ocr",
		OpencodeBin:       "opencode",
		FetchDepthMax:     500,
		GitBaseURL:        "https://github.com",
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

	if raw := getenv("QA_WORKSPACE_DIR"); raw != "" {
		v := strings.TrimSpace(raw)
		if v == "" {
			return Config{}, errors.New("QA_WORKSPACE_DIR must not be empty")
		}
		cfg.WorkspaceDir = v
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

	return cfg, nil
}
