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
	MasterKey         []byte        // QA_MASTER_KEY base64 std, exactamente 32 bytes
	WebhooksEnabled   bool          // QA_WEBHOOKS_ENABLED, default true
	ReconcileInterval time.Duration // QA_RECONCILE_INTERVAL, default 5m
	GitHubURL         string        // QA_GITHUB_URL, default "https://github.com"
	GitHubAPIURL      string        // QA_GITHUB_API_URL, default "https://api.github.com"
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
	}

	cfg.DatabaseURL = strings.TrimSpace(getenv("QA_DATABASE_URL"))
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("QA_DATABASE_URL is required")
	}

	cfg.PublicURL = strings.TrimRight(strings.TrimSpace(getenv("QA_PUBLIC_URL")), "/")
	if cfg.PublicURL == "" {
		return Config{}, errors.New("QA_PUBLIC_URL is required")
	}

	rawMasterKey := strings.TrimSpace(getenv("QA_MASTER_KEY"))
	if rawMasterKey == "" {
		return Config{}, errors.New("QA_MASTER_KEY is required")
	}
	masterKey, err := base64.StdEncoding.DecodeString(rawMasterKey)
	if err != nil {
		return Config{}, fmt.Errorf("QA_MASTER_KEY must be valid base64: %w", err)
	}
	if len(masterKey) != 32 {
		return Config{}, fmt.Errorf("QA_MASTER_KEY must decode to exactly 32 bytes, got %d", len(masterKey))
	}
	cfg.MasterKey = masterKey

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
		cfg.ReconcileInterval = interval
	}

	if v := strings.TrimSpace(getenv("QA_GITHUB_URL")); v != "" {
		cfg.GitHubURL = strings.TrimRight(v, "/")
	}

	if v := strings.TrimSpace(getenv("QA_GITHUB_API_URL")); v != "" {
		cfg.GitHubAPIURL = strings.TrimRight(v, "/")
	}

	return cfg, nil
}
