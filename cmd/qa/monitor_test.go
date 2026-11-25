package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/monitor"
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

// TestMonitorDeliveriesFallsBackToMemory keeps the monitor alerting when it
// has no database: the outbox is not durable, but it is still present.
func TestMonitorDeliveriesFallsBackToMemory(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, dsn := range []string{
		"",
		"postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=1",
	} {
		deliveries, closeStore := monitorDeliveries(context.Background(), dsn, logger)
		if _, ok := deliveries.(*monitor.MemoryDeliveryStore); !ok {
			t.Errorf("deliveries for dsn %q = %T, want the memory fallback", dsn, deliveries)
		}
		if closeStore != nil {
			t.Errorf("closeStore for dsn %q is not nil without a database", dsn)
		}
	}
}
