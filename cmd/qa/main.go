package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/api"
	"github.com/jeriveromartinez/qa-control-center/internal/auth"
	"github.com/jeriveromartinez/qa-control-center/internal/config"
	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/jobs"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/syncer"
	"github.com/jeriveromartinez/qa-control-center/internal/web"
)

func usage() string {
	return "usage: qa <serve|worker|migrate>"
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
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	box, err := crypto.NewBox(cfg.MasterKey)
	if err != nil {
		return fmt.Errorf("create crypto box: %w", err)
	}
	apps := github.NewStoreAppProvider(st, box, cfg.GitHubAPIURL)

	publisher := events.NewPGPublisher(st.Pool)
	broker := events.NewBroker(st.Pool)
	go func() {
		if err := broker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("event broker stopped", "error", err)
		}
	}()

	s := &syncer.Syncer{Store: st, Apps: apps, Events: publisher}
	client, err := jobs.NewControlClient(st.Pool, s, cfg.ReconcileInterval)
	if err != nil {
		return fmt.Errorf("create control client: %w", err)
	}
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
	enqueuer := jobs.NewEnqueuer(client)

	// A fresh instance has no App credentials. Generate a one-time setup token
	// so only an operator with access to the logs can create the App.
	setupToken := ""
	if _, err := st.LoadAppConfig(ctx); errors.Is(err, store.ErrNotConfigured) {
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			return fmt.Errorf("generate setup token: %w", err)
		}
		setupToken = hex.EncodeToString(raw)
		slog.Warn("setup required", "setup_token", setupToken, "url", cfg.PublicURL+"/setup")
	}

	authn := &auth.Auth{
		Store:     st,
		Apps:      apps,
		GitHubURL: cfg.GitHubURL,
		APIURL:    cfg.GitHubAPIURL,
		PublicURL: cfg.PublicURL,
		HTTP:      &http.Client{Timeout: 15 * time.Second},
	}
	webhooks := &github.WebhookHandler{Store: st, Apps: apps, Events: publisher, Jobs: enqueuer}
	router := api.NewRouter(api.Deps{
		Cfg:        cfg,
		Store:      st,
		Box:        box,
		Apps:       apps,
		Auth:       authn,
		Webhooks:   webhooks,
		Broker:     broker,
		Events:     publisher,
		Jobs:       enqueuer,
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

func runWorker() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	slog.Info("worker idle: review queue arrives in phase 2")
	<-ctx.Done()
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
