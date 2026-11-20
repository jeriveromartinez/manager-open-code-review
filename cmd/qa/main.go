package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/api"
	"github.com/jeriveromartinez/qa-control-center/internal/auth"
	"github.com/jeriveromartinez/qa-control-center/internal/config"
	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/jobs"
	"github.com/jeriveromartinez/qa-control-center/internal/publish"
	"github.com/jeriveromartinez/qa-control-center/internal/review"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/syncer"
	"github.com/jeriveromartinez/qa-control-center/internal/web"
	"github.com/jeriveromartinez/qa-control-center/internal/worker"
)

func usage() string {
	return "usage: qa <serve|worker|migrate|monitor|healthcheck>"
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage())
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe()
	case "worker":
		err = runWorker()
	case "migrate":
		err = runMigrate()
	case "monitor":
		err = runMonitor(logger)
	case "healthcheck":
		err = runHealthcheck(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, usage())
		os.Exit(2)
	}

	if err != nil {
		logger.Error("command failed", "command", os.Args[1], "error", err)
		os.Exit(1)
	}
}

func runServe() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := cfg.RequireMasterKey(); err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if err := loadSettings(ctx, st, &cfg); err != nil {
		return fmt.Errorf("load settings: %w", err)
	}

	box, err := crypto.NewBox(cfg.MasterKey)
	if err != nil {
		return fmt.Errorf("create crypto box: %w", err)
	}
	workerBox, err := crypto.NewBox(cfg.WorkerKey)
	if err != nil {
		return fmt.Errorf("create worker key box: %w", err)
	}
	apps := github.NewStoreAppProvider(st, box, cfg.GitHubAPIURL)

	publisher := events.NewPGPublisher(st.Pool)
	broker := events.NewBroker(st.Pool)
	go func() {
		if err := broker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("event broker stopped", "error", err)
		}
	}()

	checks := &github.Checks{Store: st, Apps: apps}
	s := &syncer.Syncer{Store: st, Apps: apps, Events: publisher, Checks: checks}
	// The same worker starts/cancels checks for the trigger and publishes
	// finished runs from the publish queue. It never executes a review.
	publisherWorker := &publish.Worker{
		Store: st, Apps: apps, Box: workerBox,
		Events: publisher, PublicURL: cfg.PublicURL,
	}
	trigger := &review.Trigger{
		Store: st, Apps: apps, Box: workerBox, Events: publisher, Checks: publisherWorker,
	}
	client, err := jobs.NewControlClient(st.Pool, s, cfg.ReconcileInterval, jobs.ControlDeps{
		Trigger: trigger, Publisher: publisherWorker,
	})
	if err != nil {
		return fmt.Errorf("create control client: %w", err)
	}
	// Attach the enqueuer before Start: the reconcile job runs on start and
	// must never observe a trigger with a nil Jobs field.
	enqueuer := jobs.NewEnqueuer(client)
	trigger.Jobs = enqueuer

	if err := client.Start(ctx); err != nil {
		return fmt.Errorf("start control client: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := client.Stop(shutdownCtx); err != nil {
			slog.Error("stop control client", "error", err)
		}
	}()

	// A fresh instance has no App credentials. The setup window is open: the
	// form asks only for the App name, and QA_SETUP_TOKEN locks it down when
	// the operator sets it.
	setupToken := strings.TrimSpace(os.Getenv("QA_SETUP_TOKEN"))
	if _, err := st.LoadAppConfig(ctx); errors.Is(err, store.ErrNotConfigured) {
		if cfg.PublicURL == "" {
			slog.Warn("setup required", "url", "/setup",
				"detail", "the public URL will be detected when the wizard runs")
		} else {
			slog.Warn("setup required", "url", strings.TrimRight(cfg.PublicURL, "/")+"/setup")
		}
	}

	authn := &auth.Auth{
		Store:     st,
		Apps:      apps,
		GitHubURL: cfg.GitHubURL,
		APIURL:    cfg.GitHubAPIURL,
		PublicURL: cfg.PublicURL,
		HTTP:      &http.Client{Timeout: 15 * time.Second},
	}
	webhooks := &github.WebhookHandler{
		Store: st, Apps: apps, Events: publisher, Jobs: enqueuer, Reviews: trigger,
		Findings: publisherWorker, Comments: publisherWorker, Checks: checks,
	}
	router := api.NewRouter(api.Deps{
		Cfg:        cfg,
		Store:      st,
		Box:        box,
		WorkerBox:  workerBox,
		Apps:       apps,
		Auth:       authn,
		Webhooks:   webhooks,
		Broker:     broker,
		Events:     publisher,
		Jobs:       enqueuer,
		Health:     enqueuer,
		Reviews:    trigger,
		Findings:   publisherWorker,
		SetupToken: setupToken,
		Static:     web.Handler(),
	})

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.ListenAndServe() }()
	slog.Info("qa control center listening", "addr", cfg.ListenAddr)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown server: %w", err)
		}
		return nil
	case err := <-serverErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	}
}

// checkBinary fails fast when a worker dependency is missing or broken. The
// captured stderr is included so an operator can see why the tool refused to
// start.
func checkBinary(ctx context.Context, bin string) (string, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("binary %q --version failed: %w; stderr: %s", bin, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(stdout)), nil
}

func runWorker() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Load only requires QA_WORKER_KEY: the worker must never see the master
	// key used to decrypt GitHub App credentials.
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := loadSettings(ctx, st, &cfg); err != nil {
		return fmt.Errorf("load settings: %w", err)
	}

	workerBox, err := crypto.NewBox(cfg.WorkerKey)
	if err != nil {
		return fmt.Errorf("create worker key box: %w", err)
	}
	for _, bin := range []string{cfg.OCRBin, cfg.OpencodeBin} {
		version, err := checkBinary(ctx, bin)
		if err != nil {
			return err
		}
		slog.Info("worker dependency ready", "binary", bin, "version", version)
	}

	eventsPublisher := events.NewPGPublisher(st.Pool)
	runner := &review.Runner{
		Store: st, Apps: nil, Box: workerBox, Events: eventsPublisher,
		Cfg: review.RunnerConfig{
			WorkspaceDir:  cfg.WorkspaceDir,
			OCRBin:        cfg.OCRBin,
			OpencodeBin:   cfg.OpencodeBin,
			GitBaseURL:    cfg.GitBaseURL,
			FetchDepthMax: cfg.FetchDepthMax,
			Timeout:       cfg.ReviewTimeout,
			Steps:         cfg.ReviewAgentSteps,
		},
	}
	checker := &review.ProfileChecker{
		Store: st, Box: workerBox, Bin: cfg.OpencodeBin, Timeout: cfg.ReviewTimeout,
	}
	watchdog := &review.Watchdog{
		Store: st, Events: eventsPublisher, StaleAfter: cfg.ReviewStaleAfter,
	}
	// A verdict is refreshed by the worker every QA_LLM_HEALTH_INTERVAL; the
	// panel marks it stale at twice that, so one delayed cycle is not alarming.
	healthStaleAfter := cfg.LLMHealthStaleAfter()
	sweeper := &review.HealthSweeper{Store: st, StaleAfter: healthStaleAfter}
	client, err := jobs.NewReviewClient(st.Pool, runner, checker, cfg.ReviewConcurrency, jobs.ReviewDeps{
		Watchdog:       watchdog,
		Health:         sweeper,
		HealthInterval: cfg.LLMHealthInterval,
	})
	if err != nil {
		return fmt.Errorf("create review client: %w", err)
	}
	// The enqueuer must exist before Start: the runner needs it to schedule
	// publication and further health checks while processing a review, and the
	// watchdog uses it to publish interrupted runs.
	enqueuer := jobs.NewEnqueuer(client)
	runner.Jobs = enqueuer
	watchdog.Jobs = enqueuer
	sweeper.Enqueue = enqueuer.EnqueueLLMHealth

	if err := client.Start(ctx); err != nil {
		return fmt.Errorf("start review client: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := client.Stop(shutdownCtx); err != nil {
			slog.Error("stop review client", "error", err)
		}
	}()

	slog.Info("review worker started", "concurrency", cfg.ReviewConcurrency)

	health := worker.New(worker.Config{
		Stats:           st,
		StatsWindow:     time.Hour,
		StaleQueueAfter: cfg.WorkerQueueStale,
		Dependencies: []worker.Dependency{
			{Name: cfg.OCRBin, Version: func(ctx context.Context) (string, error) {
				return checkBinary(ctx, cfg.OCRBin)
			}},
			{Name: cfg.OpencodeBin, Version: func(ctx context.Context) (string, error) {
				return checkBinary(ctx, cfg.OpencodeBin)
			}},
		},
	})
	healthSrv := &http.Server{
		Addr:              cfg.WorkerHealthAddr,
		Handler:           health.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	healthErr := make(chan error, 1)
	go func() {
		slog.Info("worker health listening", "addr", cfg.WorkerHealthAddr)
		if err := healthSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			healthErr <- err
		}
		close(healthErr)
	}()

	select {
	case <-ctx.Done():
	case err := <-healthErr:
		if err != nil {
			return fmt.Errorf("worker health server: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := healthSrv.Shutdown(shutdownCtx); err != nil {
		slog.Error("stop worker health server", "error", err)
	}
	return nil
}

func runMigrate() error {
	ctx := context.Background()

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	s, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer s.Close()

	return s.Migrate(ctx)
}
