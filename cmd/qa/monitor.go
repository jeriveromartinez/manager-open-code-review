package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/config"
	"github.com/jeriveromartinez/qa-control-center/internal/monitor"
	"github.com/jeriveromartinez/qa-control-center/internal/settings"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// runMonitor polls the worker and delivers alerts. It never calls config.Load:
// the monitor has no worker key, so the essentials of serve/worker would
// reject a valid container.
func runMonitor(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := strings.TrimSpace(os.Getenv("QA_DATABASE_URL"))
	cfg, _ := monitorConfig(ctx, dsn)
	deliveries, closeDeliveries := monitorDeliveries(ctx, dsn, logger)
	if closeDeliveries != nil {
		defer closeDeliveries()
	}

	client := &http.Client{Timeout: 10 * time.Second}
	notifiers := monitor.MultiNotifier{monitor.NewLogNotifier(logger)}
	if cfg.AlertWebhookURL != "" {
		notifiers = append(notifiers, monitor.NewWebhookNotifier(cfg.AlertWebhookURL, client, logger))
	}

	poller := &monitor.Poller{
		Source:     monitor.NewFetcher(cfg.WorkerHealthURL, client),
		Notifier:   notifiers,
		Tracker:    monitor.NewTracker(cfg.MonitorRenotifyAfter),
		Deliveries: deliveries,
		Logger:     logger,
	}

	logger.Info("worker monitor started",
		"worker", cfg.WorkerHealthURL,
		"interval", cfg.MonitorInterval.String(),
		"renotify_after", cfg.MonitorRenotifyAfter.String(),
		"webhook", cfg.AlertWebhookURL != "")
	return poller.Run(ctx, cfg.MonitorInterval)
}

// monitorConfig reads the stored settings, because the monitor is configured
// from the panel like everything else. A missing or unreachable database must
// not stop it: a monitor that refuses to start when PostgreSQL is down cannot
// report that PostgreSQL is down, so it falls back to the defaults. The error
// return is part of the planned signature and is reserved for a future hard
// failure; today every path succeeds.
func monitorConfig(ctx context.Context, dsn string) (config.Config, error) {
	cfg := config.Default()
	if dsn == "" {
		slog.Warn("no database URL; the monitor uses the default settings")
		return cfg, nil
	}
	st, err := store.Open(ctx, dsn)
	if err != nil {
		slog.Warn("database unavailable; the monitor uses the default settings", "error", err)
		return cfg, nil
	}
	defer st.Close()

	values, err := st.Settings(ctx)
	if err != nil {
		slog.Warn("cannot read settings; the monitor uses the defaults", "error", err)
		return cfg, nil
	}
	if adjusted, err := settings.ApplyStored(&cfg, values); err != nil {
		slog.Warn("stored settings are invalid; the monitor uses the defaults", "error", err)
		return config.Default(), nil
	} else if len(adjusted) > 0 {
		// Same repair the app logs at start, so the two processes watch the same
		// orphan threshold.
		slog.Warn("stored settings were raised to meet the current minimums; review them in the panel",
			"settings", adjusted)
	}
	return cfg, nil
}

// monitorDeliveries opens the durable alert outbox. Without a reachable
// database the monitor still runs, but pending alerts only survive until the
// process exits.
func monitorDeliveries(ctx context.Context, dsn string, logger *slog.Logger) (monitor.DeliveryStore, func()) {
	if dsn == "" {
		return monitor.NewMemoryDeliveryStore(), nil
	}
	st, err := store.Open(ctx, dsn)
	if err != nil {
		logger.Warn("alert deliveries will not survive a restart", "error", err)
		return monitor.NewMemoryDeliveryStore(), nil
	}
	return monitor.NewPGDeliveryStore(st.Pool), st.Close
}

// healthcheckURL resolves the target of `qa healthcheck`. The default is the
// app's own loopback: the container image is debian-slim without curl, so the
// binary has to be its own healthcheck client.
func healthcheckURL(args []string) (string, error) {
	if len(args) > 1 {
		return "", fmt.Errorf("usage: qa healthcheck [url]")
	}
	if len(args) == 1 {
		if args[0] == "" {
			return "", fmt.Errorf("healthcheck url must not be empty")
		}
		return args[0], nil
	}
	return "http://127.0.0.1:8080/healthz", nil
}

func runHealthcheck(args []string) error {
	url, err := healthcheckURL(args)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build health request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck %s: %w", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck %s answered %d", url, resp.StatusCode)
	}
	return nil
}
