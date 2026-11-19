package store_test

import (
	"context"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

func TestMigrateCreatesTables(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()

	tables := []string{
		"app_configs",
		"users",
		"sessions",
		"installations",
		"teams",
		"team_members",
		"repositories",
		"pull_requests",
		"audit_logs",
		"river_job",
	}

	for _, table := range tables {
		if !s.DB.WithContext(ctx).Migrator().HasTable(table) {
			t.Errorf("table %q was not created", table)
		}
	}
}

func TestMigrateIdempotent(t *testing.T) {
	s := testutil.NewStore(t)

	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("second Migrate() returned error: %v", err)
	}
}
