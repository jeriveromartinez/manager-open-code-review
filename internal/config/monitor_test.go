package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/config"
)

func TestLoadMonitorDefaults(t *testing.T) {
	cfg, err := config.LoadMonitor(getenv(requiredEnv()))
	if err != nil {
		t.Fatalf("LoadMonitor() error = %v", err)
	}
	if cfg.WorkerHealthURL != "http://worker:9090" {
		t.Errorf("WorkerHealthURL = %q, want %q", cfg.WorkerHealthURL, "http://worker:9090")
	}
	if cfg.Interval != time.Minute {
		t.Errorf("Interval = %v, want %v", cfg.Interval, time.Minute)
	}
	if cfg.RenotifyAfter != 30*time.Minute {
		t.Errorf("RenotifyAfter = %v, want %v", cfg.RenotifyAfter, 30*time.Minute)
	}
	if cfg.AlertWebhookURL != "" {
		t.Errorf("AlertWebhookURL = %q, want empty", cfg.AlertWebhookURL)
	}
}

func TestLoadMonitorOverrides(t *testing.T) {
	env := requiredEnv()
	env["QA_WORKER_HEALTH_URL"] = "http://worker.qa:9090/"
	env["QA_MONITOR_INTERVAL"] = "15s"
	env["QA_MONITOR_RENOTIFY_AFTER"] = "2h"
	env["QA_ALERT_WEBHOOK_URL"] = "https://alerts.example.com/hook"

	cfg, err := config.LoadMonitor(getenv(env))
	if err != nil {
		t.Fatalf("LoadMonitor() error = %v", err)
	}
	if cfg.WorkerHealthURL != "http://worker.qa:9090" {
		t.Errorf("WorkerHealthURL = %q, want no trailing slash", cfg.WorkerHealthURL)
	}
	if cfg.Interval != 15*time.Second {
		t.Errorf("Interval = %v, want %v", cfg.Interval, 15*time.Second)
	}
	if cfg.RenotifyAfter != 2*time.Hour {
		t.Errorf("RenotifyAfter = %v, want %v", cfg.RenotifyAfter, 2*time.Hour)
	}
	if cfg.AlertWebhookURL != "https://alerts.example.com/hook" {
		t.Errorf("AlertWebhookURL = %q, want %q", cfg.AlertWebhookURL, "https://alerts.example.com/hook")
	}
}

func TestLoadBadMonitorInterval(t *testing.T) {
	for _, value := range []string{"0s", "-1m", "cada minuto"} {
		t.Run(value, func(t *testing.T) {
			env := requiredEnv()
			env["QA_MONITOR_INTERVAL"] = value
			_, err := config.LoadMonitor(getenv(env))
			if err == nil {
				t.Fatalf("LoadMonitor() error = nil for %q, want error mentioning QA_MONITOR_INTERVAL", value)
			}
			if !strings.Contains(err.Error(), "QA_MONITOR_INTERVAL") {
				t.Errorf("LoadMonitor() error = %q, want it to mention QA_MONITOR_INTERVAL", err.Error())
			}
		})
	}
}

func TestLoadBadMonitorRenotifyAfter(t *testing.T) {
	for _, value := range []string{"0s", "-1m", "media hora"} {
		t.Run(value, func(t *testing.T) {
			env := requiredEnv()
			env["QA_MONITOR_RENOTIFY_AFTER"] = value
			_, err := config.LoadMonitor(getenv(env))
			if err == nil {
				t.Fatalf("LoadMonitor() error = nil for %q, want error mentioning QA_MONITOR_RENOTIFY_AFTER", value)
			}
			if !strings.Contains(err.Error(), "QA_MONITOR_RENOTIFY_AFTER") {
				t.Errorf("LoadMonitor() error = %q, want it to mention QA_MONITOR_RENOTIFY_AFTER", err.Error())
			}
		})
	}
}

// TestLoadMonitorRenotifyShorterThanInterval rejects a re-notify window below
// the poll interval: every tick would alert and the alert loses its meaning.
func TestLoadMonitorRenotifyShorterThanInterval(t *testing.T) {
	env := requiredEnv()
	env["QA_MONITOR_INTERVAL"] = "10m"
	env["QA_MONITOR_RENOTIFY_AFTER"] = "5m"

	_, err := config.LoadMonitor(getenv(env))
	if err == nil {
		t.Fatal("LoadMonitor() error = nil, want error mentioning QA_MONITOR_RENOTIFY_AFTER")
	}
	if !strings.Contains(err.Error(), "QA_MONITOR_RENOTIFY_AFTER") {
		t.Errorf("LoadMonitor() error = %q, want it to mention QA_MONITOR_RENOTIFY_AFTER", err.Error())
	}
}

func TestLoadBadWorkerHealthURL(t *testing.T) {
	for _, value := range []string{"worker:9090", "ftp://worker:9090", "http://"} {
		t.Run(value, func(t *testing.T) {
			env := requiredEnv()
			env["QA_WORKER_HEALTH_URL"] = value
			_, err := config.LoadMonitor(getenv(env))
			if err == nil {
				t.Fatalf("LoadMonitor() error = nil for %q, want error mentioning QA_WORKER_HEALTH_URL", value)
			}
			if !strings.Contains(err.Error(), "QA_WORKER_HEALTH_URL") {
				t.Errorf("LoadMonitor() error = %q, want it to mention QA_WORKER_HEALTH_URL", err.Error())
			}
		})
	}
}

func TestLoadBadAlertWebhookURL(t *testing.T) {
	env := requiredEnv()
	env["QA_ALERT_WEBHOOK_URL"] = "alerts.example.com/hook"

	_, err := config.LoadMonitor(getenv(env))
	if err == nil {
		t.Fatal("LoadMonitor() error = nil, want error mentioning QA_ALERT_WEBHOOK_URL")
	}
	if !strings.Contains(err.Error(), "QA_ALERT_WEBHOOK_URL") {
		t.Errorf("LoadMonitor() error = %q, want it to mention QA_ALERT_WEBHOOK_URL", err.Error())
	}
}

// TestLoadMonitorOnly is the blocker this feature had to solve: `qa monitor`
// speaks HTTP only, so requiring the database URL or QA_WORKER_KEY would make
// it impossible to run as its own container.
func TestLoadMonitorOnly(t *testing.T) {
	env := map[string]string{
		"QA_WORKER_HEALTH_URL":      "http://worker:9090",
		"QA_ALERT_WEBHOOK_URL":      "https://alerts.example.com/hook",
		"QA_MONITOR_INTERVAL":       "30s",
		"QA_MONITOR_RENOTIFY_AFTER": "15m",
	}

	mon, err := config.LoadMonitor(getenv(env))
	if err != nil {
		t.Fatalf("LoadMonitor() error = %v, want nil without database or worker keys", err)
	}
	if mon.WorkerHealthURL != "http://worker:9090" {
		t.Errorf("WorkerHealthURL = %q", mon.WorkerHealthURL)
	}
	if mon.Interval != 30*time.Second {
		t.Errorf("Interval = %v, want 30s", mon.Interval)
	}
	if mon.RenotifyAfter != 15*time.Minute {
		t.Errorf("RenotifyAfter = %v, want 15m", mon.RenotifyAfter)
	}
	if mon.AlertWebhookURL != "https://alerts.example.com/hook" {
		t.Errorf("AlertWebhookURL = %q", mon.AlertWebhookURL)
	}
}

func TestLoadMonitorOnlyDefaultsAndGarbage(t *testing.T) {
	mon, err := config.LoadMonitor(getenv(nil))
	if err != nil {
		t.Fatalf("LoadMonitor() error = %v", err)
	}
	if mon.WorkerHealthURL != "http://worker:9090" || mon.Interval != time.Minute || mon.RenotifyAfter != 30*time.Minute {
		t.Fatalf("defaults = %+v, want the documented ones", mon)
	}
	if mon.AlertWebhookURL != "" {
		t.Errorf("AlertWebhookURL = %q, want empty (log-only)", mon.AlertWebhookURL)
	}

	if _, err := config.LoadMonitor(getenv(map[string]string{"QA_MONITOR_INTERVAL": "0"})); err == nil {
		t.Fatal("LoadMonitor() error = nil for a zero interval, want an error")
	}
}

// TestLoadLeavesTheMonitorSettingsAlone pins the split this task introduces:
// Load owns the essentials only, and the monitor settings come from the
// database like every other behaviour setting.
func TestLoadLeavesTheMonitorSettingsAlone(t *testing.T) {
	env := requiredEnv()
	env["QA_MONITOR_INTERVAL"] = "45s"
	env["QA_MONITOR_RENOTIFY_AFTER"] = "90s"
	env["QA_WORKER_HEALTH_URL"] = "http://worker.qa:9091"
	env["QA_ALERT_WEBHOOK_URL"] = "https://hooks.example.com/qa"

	cfg, err := config.Load(getenv(env))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	defaults := config.Default()
	if cfg.MonitorInterval != defaults.MonitorInterval || cfg.MonitorRenotifyAfter != defaults.MonitorRenotifyAfter ||
		cfg.WorkerHealthURL != defaults.WorkerHealthURL || cfg.AlertWebhookURL != defaults.AlertWebhookURL {
		t.Errorf("Load() = %+v, want the monitor settings at their defaults", cfg)
	}
}
