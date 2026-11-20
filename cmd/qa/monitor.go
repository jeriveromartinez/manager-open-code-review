package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/config"
	"github.com/jeriveromartinez/qa-control-center/internal/monitor"
)

// runMonitor polls the worker and delivers alerts. It loads its own
// configuration instead of config.Load: the monitor has no database and no
// keys, so the required variables of serve/worker would reject a valid
// container.
func runMonitor(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.LoadMonitor(os.Getenv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	notifiers := monitor.MultiNotifier{monitor.NewLogNotifier(logger)}
	if cfg.AlertWebhookURL != "" {
		notifiers = append(notifiers, monitor.NewWebhookNotifier(cfg.AlertWebhookURL, client, logger))
	}

	poller := &monitor.Poller{
		Source:   monitor.NewFetcher(cfg.WorkerHealthURL, client),
		Notifier: notifiers,
		Tracker:  monitor.NewTracker(cfg.RenotifyAfter),
		Logger:   logger,
	}

	logger.Info("worker monitor started",
		"worker", cfg.WorkerHealthURL,
		"interval", cfg.Interval.String(),
		"renotify_after", cfg.RenotifyAfter.String(),
		"webhook", cfg.AlertWebhookURL != "")
	return poller.Run(ctx, cfg.Interval)
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
