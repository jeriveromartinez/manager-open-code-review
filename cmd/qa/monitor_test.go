package main

import (
	"context"
	"testing"
	"time"
)

func TestMonitorConfigFallsBackToDefaults(t *testing.T) {
	cfg, err := monitorConfig(context.Background(), "")
	if err != nil {
		t.Fatalf("monitorConfig without a database = %v, want defaults and no error", err)
	}
	if cfg.WorkerHealthURL != "http://worker:9090" || cfg.MonitorInterval != time.Minute {
		t.Errorf("fallback = %+v, want the defaults", cfg)
	}
}
