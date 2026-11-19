// Package testutil provides helpers for tests that need a real PostgreSQL
// database.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const (
	containerImage = "postgres:17"
	adminDatabase  = "qa_test"
	adminUser      = "qa"
	adminPassword  = "qa"

	// allowSkipEnv opts a test run into skipping when Docker is unavailable.
	// Without it, DB tests fail so schema verification cannot silently vanish.
	allowSkipEnv = "TESTCONTAINERS_ALLOW_SKIP"
)

var (
	containerOnce sync.Once
	container     *postgres.PostgresContainer
	adminURL      string
	containerErr  error
)

// NewStore starts (once per package) a PostgreSQL container, creates a fresh
// database for the test, applies the migrations and returns a Store. The Store
// is closed and the database dropped when the test finishes.
//
// If Docker is unavailable the test fails, unless TESTCONTAINERS_ALLOW_SKIP=1
// is set, in which case it is skipped.
func NewStore(t *testing.T) *store.Store {
	t.Helper()

	ensureDocker(t)

	containerOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()

		container, containerErr = postgres.Run(ctx, containerImage,
			postgres.WithDatabase(adminDatabase),
			postgres.WithUsername(adminUser),
			postgres.WithPassword(adminPassword),
			postgres.BasicWaitStrategies(),
		)
		if containerErr != nil {
			return
		}
		adminURL, containerErr = container.ConnectionString(ctx, "sslmode=disable")
	})
	if containerErr != nil {
		t.Fatalf("start postgres container: %v", containerErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	database := "test_" + randomSuffix(t)
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to admin database: %v", err)
	}
	_, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize())
	closeErr := admin.Close(ctx)
	if err != nil {
		t.Fatalf("create test database: %v", err)
	}
	if closeErr != nil {
		t.Fatalf("close admin connection: %v", closeErr)
	}

	dsn, err := withDatabase(adminURL, database)
	if err != nil {
		t.Fatalf("build test database DSN: %v", err)
	}

	s, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		s.Close()
		t.Fatalf("migrate test database: %v", err)
	}

	t.Cleanup(func() {
		s.Close()
		dropDatabase(t, database)
	})

	return s
}

// ensureDocker verifies that a Docker daemon is reachable. It fails the test by
// default so schema verification cannot silently disappear; setting
// TESTCONTAINERS_ALLOW_SKIP=1 downgrades the failure to a skip.
func ensureDocker(t *testing.T) {
	t.Helper()

	// GetProvider can panic when the environment is malformed.
	defer func() {
		if r := recover(); r != nil {
			dockerUnavailable(t, fmt.Errorf("panic: %v", r))
		}
	}()

	provider, err := testcontainers.ProviderDefault.GetProvider()
	if err != nil {
		dockerUnavailable(t, err)
		return
	}
	t.Cleanup(func() { _ = provider.Close() })

	if err := provider.Health(context.Background()); err != nil {
		dockerUnavailable(t, err)
	}
}

// dockerUnavailable fails the test, or skips it when TESTCONTAINERS_ALLOW_SKIP=1.
func dockerUnavailable(t *testing.T, err error) {
	t.Helper()

	msg := fmt.Sprintf("docker is not available for testcontainers: %v\n"+
		"database tests need a Docker daemon; set %s=1 to skip them instead of failing.\n"+
		"hint: this machine's daemon uses a non-default socket, "+
		"DOCKER_HOST=unix:///Users/julio/.kiri/run/docker.sock", err, allowSkipEnv)

	if os.Getenv(allowSkipEnv) == "1" {
		t.Skip(msg)
	}
	t.Fatal(msg)
}

// dropDatabase removes the per-test database, forcing connections closed.
func dropDatabase(t *testing.T, database string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Logf("drop test database %q: connect: %v", database, err)
		return
	}
	defer func() { _ = conn.Close(ctx) }()

	stmt := "DROP DATABASE IF EXISTS " + pgx.Identifier{database}.Sanitize() + " WITH (FORCE)"
	if _, err := conn.Exec(ctx, stmt); err != nil {
		t.Logf("drop test database %q: %v", database, err)
	}
}

// withDatabase returns dsn pointing at another database on the same server.
func withDatabase(dsn, database string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse DSN: %w", err)
	}
	u.Path = "/" + database
	return u.String(), nil
}

// randomSuffix returns a random hex string to make database names unique.
func randomSuffix(t *testing.T) string {
	t.Helper()

	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("generate database suffix: %v", err)
	}
	return hex.EncodeToString(b[:])
}
