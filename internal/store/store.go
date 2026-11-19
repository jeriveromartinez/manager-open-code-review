package store

import (
	"context"
	"embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store is the PostgreSQL-backed data access layer. DB is the GORM handle and
// Pool is the underlying pgx pool shared by GORM, goose and River.
type Store struct {
	DB   *gorm.DB
	Pool *pgxpool.Pool
}

// Open creates a connection pool to url and a GORM handle on top of it.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("create pgx pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	sqlDB := stdlib.OpenDBFromPool(pool)
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{})
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("open gorm: %w", err)
	}

	return &Store{DB: db, Pool: pool}, nil
}

// Migrate applies the embedded goose migrations and then River's own schema
// migrations. It is safe to call more than once.
func (s *Store) Migrate(ctx context.Context) error {
	sqlDB, err := s.DB.DB()
	if err != nil {
		return fmt.Errorf("get sql.DB: %w", err)
	}

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	migrator, err := rivermigrate.New(riverpgxv5.New(s.Pool), nil)
	if err != nil {
		return fmt.Errorf("create river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("run river migrations: %w", err)
	}

	return nil
}

// Close releases the connection pool.
func (s *Store) Close() {
	if s == nil || s.Pool == nil {
		return
	}
	s.Pool.Close()
}
