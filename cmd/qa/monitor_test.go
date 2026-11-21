package main

import (
	"context"
	"testing"
	"time"
)

func TestMonitorConfigFallsBackToDefaults(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
	}{
		{"no database url", ""},
		// Port 1 refuses the connection immediately, so the store.Open path is
		// exercised without Docker.
		{"unreachable database", "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := monitorConfig(context.Background(), tc.dsn)
			if err != nil {
				t.Fatalf("monitorConfig(%q) = %v, want defaults and no error", tc.dsn, err)
			}
			// Literals, not config.Default(): the test has to catch a change of
			// default that silently alters what the monitor falls back to.
			if cfg.WorkerHealthURL != "http://worker:9090" {
				t.Errorf("WorkerHealthURL = %q, want http://worker:9090", cfg.WorkerHealthURL)
			}
			if cfg.MonitorInterval != time.Minute {
				t.Errorf("MonitorInterval = %s, want 1m", cfg.MonitorInterval)
			}
			if cfg.MonitorRenotifyAfter != 30*time.Minute {
				t.Errorf("MonitorRenotifyAfter = %s, want 30m", cfg.MonitorRenotifyAfter)
			}
			if cfg.AlertWebhookURL != "" {
				t.Errorf("AlertWebhookURL = %q, want empty", cfg.AlertWebhookURL)
			}
		})
	}
}
