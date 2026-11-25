package review

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestOpsEvidenceSchemaChangeWithoutMigrationIsUnknown(t *testing.T) {
	ops := evaluateOpsEvidence([]opsChangeFile{
		{
			Path: "internal/store/models.go",
			Content: "type User struct {\n" +
				"\tID    uint   `gorm:\"column:id\"`\n" +
				"\tEmail string `gorm:\"column:email\"`\n" +
				"}",
		},
	})
	dim := opsDimensionOf(t, ops, store.OpsDimensionSchema)
	if !dim.Applies {
		t.Fatal("schema dimension did not apply to a model change")
	}
	if dim.Status != store.OpsStatusUnknown {
		t.Fatalf("schema status = %q, want unknown", dim.Status)
	}
	if !containsString(dim.Missing, "migration") {
		t.Errorf("missing = %v, want a migration", dim.Missing)
	}
	if ops.Status != store.OpsStatusUnknown {
		t.Errorf("overall status = %q, want unknown", ops.Status)
	}
	if ops.Controls.WritesDecision || !ops.Controls.Observation {
		t.Errorf("controls = %+v, want observation only", ops.Controls)
	}
}

func TestOpsEvidenceSafeMigrationPasses(t *testing.T) {
	ops := evaluateOpsEvidence([]opsChangeFile{
		{
			Path: "internal/store/migrations/00022_add_user_note.sql",
			Content: "-- +goose Up\n" +
				"ALTER TABLE users ADD COLUMN note text;\n" +
				"-- +goose Down\n" +
				"ALTER TABLE users DROP COLUMN note;\n",
		},
		{
			Path:    "internal/store/models.go",
			Content: "type User struct { Note string `gorm:\"column:note\"` }",
		},
	})
	dim := opsDimensionOf(t, ops, store.OpsDimensionSchema)
	if dim.Status != store.OpsStatusSatisfied {
		t.Fatalf("schema status = %q, want satisfied (reason: %s, missing: %v)", dim.Status, dim.Reason, dim.Missing)
	}
	evidence := strings.Join(dim.Evidence, "\n")
	for _, want := range []string{"ordered migrations", "artifact: migration"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("evidence %q does not contain %q", evidence, want)
		}
	}
	if ops.Status != store.OpsStatusSatisfied {
		t.Errorf("overall status = %q, want satisfied", ops.Status)
	}
}

func TestOpsEvidenceDestructiveMigrationIsFlagged(t *testing.T) {
	ops := evaluateOpsEvidence([]opsChangeFile{
		{
			Path: "migrations/00023_drop_user_email.sql",
			Content: "-- +goose Up\n" +
				"ALTER TABLE users DROP COLUMN email;\n" +
				"-- +goose Down\n" +
				"ALTER TABLE users ADD COLUMN email text;\n",
		},
	})
	dim := opsDimensionOf(t, ops, store.OpsDimensionSchema)
	if dim.Status != store.OpsStatusRisk {
		t.Fatalf("schema status = %q, want risk", dim.Status)
	}
	if !strings.Contains(dim.Reason, "destructive migration") {
		t.Errorf("reason = %q, want a destructive migration explanation", dim.Reason)
	}
	if ops.Status != store.OpsStatusRisk {
		t.Errorf("overall status = %q, want risk", ops.Status)
	}
}

func TestOpsEvidenceDestructiveAlterationsAreFlagged(t *testing.T) {
	statements := map[string]string{
		"rename column":   "ALTER TABLE users RENAME COLUMN email TO email_address;",
		"rename table":    "ALTER TABLE users RENAME TO accounts;",
		"alter type":      "ALTER TABLE users ALTER COLUMN email TYPE text;",
		"set not null":    "ALTER TABLE users ALTER COLUMN email SET NOT NULL;",
		"modify column":   "ALTER TABLE users MODIFY COLUMN email text;",
		"drop index":      "DROP INDEX users_email_idx;",
		"drop constraint": "ALTER TABLE users DROP CONSTRAINT users_email_key;",
		"drop type":       "DROP TYPE status;",
		"drop view":       "DROP VIEW active_users;",
		"delete from":     "DELETE FROM users WHERE email IS NULL;",
		"truncate table":  "TRUNCATE TABLE users;",
	}
	for name, statement := range statements {
		t.Run(name, func(t *testing.T) {
			ops := evaluateOpsEvidence([]opsChangeFile{
				{
					Path: "migrations/00024_change_users.sql",
					Content: "-- +goose Up\n" + statement + "\n" +
						"-- +goose Down\n-- revert\n",
				},
			})
			dim := opsDimensionOf(t, ops, store.OpsDimensionSchema)
			if dim.Status != store.OpsStatusRisk {
				t.Errorf("schema status = %q, want risk for %q", dim.Status, statement)
			}
		})
	}
}

func TestOpsEvidenceReversibleRenameIsStillIncompatible(t *testing.T) {
	ops := evaluateOpsEvidence([]opsChangeFile{
		{
			Path: "migrations/00025_rename_email.sql",
			Content: "-- +goose Up\n" +
				"ALTER TABLE users RENAME COLUMN email TO email_address;\n" +
				"-- +goose Down\n" +
				"ALTER TABLE users RENAME COLUMN email_address TO email;\n",
		},
	})
	dim := opsDimensionOf(t, ops, store.OpsDimensionSchema)
	if dim.Status != store.OpsStatusRisk {
		t.Fatalf("schema status = %q, want risk: a rename is incompatible with the old version", dim.Status)
	}
}

func TestOpsEvidenceDropInDownSectionIsNotDestructive(t *testing.T) {
	ops := evaluateOpsEvidence([]opsChangeFile{
		{
			Path: "migrations/00026_add_user_note.sql",
			Content: "-- +goose Up\n" +
				"ALTER TABLE users ADD COLUMN note text;\n" +
				"-- +goose Down\n" +
				"ALTER TABLE users DROP COLUMN note;\n",
		},
	})
	dim := opsDimensionOf(t, ops, store.OpsDimensionSchema)
	if dim.Status != store.OpsStatusSatisfied {
		t.Fatalf("schema status = %q, want satisfied (reason: %s)", dim.Status, dim.Reason)
	}
}

func TestOpsEvidenceUnorderedMigrationIsUnknown(t *testing.T) {
	ops := evaluateOpsEvidence([]opsChangeFile{
		{
			Path: "migrations/add_user_note.sql",
			Content: "-- +goose Up\n" +
				"ALTER TABLE users ADD COLUMN note text;\n" +
				"-- +goose Down\n" +
				"ALTER TABLE users DROP COLUMN note;\n",
		},
	})
	dim := opsDimensionOf(t, ops, store.OpsDimensionSchema)
	if dim.Status != store.OpsStatusUnknown {
		t.Fatalf("schema status = %q, want unknown without a numeric prefix", dim.Status)
	}
	if !containsString(dim.Missing, "ordered migration") {
		t.Errorf("missing = %v, want the migration order", dim.Missing)
	}
	if !strings.Contains(dim.Reason, "order") {
		t.Errorf("reason = %q, want an ordering explanation", dim.Reason)
	}
}

func TestOpsEvidenceMigrationWithoutRollbackIsUnknown(t *testing.T) {
	ops := evaluateOpsEvidence([]opsChangeFile{
		{
			Path: "migrations/00031_add_user_note.sql",
			Content: "-- +goose Up\n" +
				"ALTER TABLE users ADD COLUMN note text;\n",
		},
	})
	dim := opsDimensionOf(t, ops, store.OpsDimensionSchema)
	if dim.Status != store.OpsStatusUnknown {
		t.Fatalf("schema status = %q, want unknown without a rollback path", dim.Status)
	}
	missing := strings.Join(dim.Missing, ",")
	if !strings.Contains(missing, "reversible migration") {
		t.Errorf("missing = %v, want the rollback path", dim.Missing)
	}
	if !strings.Contains(dim.Reason, "rollback") {
		t.Errorf("reason = %q, want a rollback explanation", dim.Reason)
	}
}

func TestOpsEvidenceCompatibleExpansionWithTransitionPlanPasses(t *testing.T) {
	ops := evaluateOpsEvidence([]opsChangeFile{
		{
			Path: "internal/store/migrations/00032_add_user_note.sql",
			Content: "-- +goose Up\n" +
				"ALTER TABLE users ADD COLUMN note text;\n" +
				"-- +goose Down\n" +
				"ALTER TABLE users DROP COLUMN note;\n",
		},
		{
			Path: "deploy/docker-compose.yml",
			Content: "services:\n" +
				"  app:\n" +
				"    strategy:\n" +
				"      type: RollingUpdate\n" +
				"    rollback: automatic\n" +
				"    metrics: prometheus\n",
		},
	})
	schema := opsDimensionOf(t, ops, store.OpsDimensionSchema)
	if schema.Status != store.OpsStatusSatisfied {
		t.Fatalf("schema status = %q, want satisfied for an additive migration (reason: %s)", schema.Status, schema.Reason)
	}
	topology := opsDimensionOf(t, ops, store.OpsDimensionTopology)
	if topology.Status != store.OpsStatusSatisfied {
		t.Fatalf("topology status = %q, want satisfied with a transition plan (missing: %v)", topology.Status, topology.Missing)
	}
	if ops.Status != store.OpsStatusSatisfied {
		t.Errorf("overall status = %q, want satisfied", ops.Status)
	}
}

func TestOpsEvidenceNonModelGormIsNotSchema(t *testing.T) {
	ops := evaluateOpsEvidence([]opsChangeFile{
		{
			Path:    "internal/store/review.go",
			Content: "type internal struct { Field string `gorm:\"column:field\"` }",
		},
	})
	dim := opsDimensionOf(t, ops, store.OpsDimensionSchema)
	if dim.Applies {
		t.Fatalf("ordinary source with a gorm tag was treated as a schema change: %+v", dim)
	}
	if ops.Status != store.OpsStatusNotApplicable {
		t.Errorf("overall status = %q, want not_applicable", ops.Status)
	}
}

func TestOpsEvidenceTopologyAndPermissionsAreDetected(t *testing.T) {
	ops := evaluateOpsEvidence([]opsChangeFile{
		{Path: "deploy/docker-compose.yml", Content: "services:\n  app:\n    image: app:v2\n"},
		{Path: "internal/auth/token.go", Content: "package auth\n"},
	})
	topology := opsDimensionOf(t, ops, store.OpsDimensionTopology)
	if !topology.Applies || topology.Status != store.OpsStatusUnknown {
		t.Errorf("topology = %+v, want applicable and unknown", topology)
	}
	permissions := opsDimensionOf(t, ops, store.OpsDimensionPermissions)
	if !permissions.Applies || permissions.Status != store.OpsStatusUnknown {
		t.Errorf("permissions = %+v, want applicable and unknown", permissions)
	}
	if ops.Status != store.OpsStatusUnknown {
		t.Errorf("overall status = %q, want unknown", ops.Status)
	}
}

func TestOpsEvidenceMissingEvidenceStaysUnknown(t *testing.T) {
	ops := evaluateOpsEvidence([]opsChangeFile{
		{
			Path:    "deploy/docker-compose.yml",
			Content: "services:\n  app:\n    image: app:v2\n",
		},
	})
	topology := opsDimensionOf(t, ops, store.OpsDimensionTopology)
	if topology.Status != store.OpsStatusUnknown {
		t.Fatalf("topology status = %q, want unknown", topology.Status)
	}
	for _, missing := range []string{"rollout strategy", "rollback/restore", "observability/alerts"} {
		if !containsString(topology.Missing, missing) {
			t.Errorf("missing = %v, want %q", topology.Missing, missing)
		}
	}
}

func TestOpsEvidenceIncidentalMarkersDoNotSatisfy(t *testing.T) {
	ops := evaluateOpsEvidence([]opsChangeFile{
		{
			Path: "deploy/docker-compose.yml",
			Content: "# rollback restore backup metrics alert prometheus\n" +
				"services:\n  app:\n    image: app:v2\n",
		},
		{Path: "internal/auth/token.go", Content: "package auth\n"},
		{
			Path:    "internal/auth/token_test.go",
			Content: "// scope role permission rbac\nfunc TestToken(t *testing.T) { if got != want { t.Fatal() } }\n",
		},
		{Path: "internal/worker/pool.go", Content: "package worker\n"},
		{
			Path:    "internal/worker/pool_test.go",
			Content: "// metrics latency p99 capacity\nfunc TestPool(t *testing.T) { if got != want { t.Fatal() } }\n",
		},
	})
	for _, name := range []string{
		store.OpsDimensionTopology,
		store.OpsDimensionPermissions,
		store.OpsDimensionLoad,
	} {
		dim := opsDimensionOf(t, ops, name)
		if dim.Status != store.OpsStatusUnknown {
			t.Errorf("%s = %+v, want unknown for incidental text", name, dim)
		}
	}
}

func TestOpsEvidenceCompleteDimensionsPass(t *testing.T) {
	ops := evaluateOpsEvidence([]opsChangeFile{
		{
			Path: "deploy/docker-compose.yml",
			Content: "services:\n" +
				"  app:\n" +
				"    strategy:\n" +
				"      type: RollingUpdate\n" +
				"    rollback: automatic\n" +
				"    metrics: prometheus\n" +
				"    alert: page on-call\n",
		},
		{Path: "internal/auth/token.go", Content: "package auth\n"},
		{
			Path:    "internal/auth/token_test.go",
			Content: "func TestPermissionScope(t *testing.T) { if got != want { t.Fatal() } }\n",
		},
		{Path: "internal/worker/pool.go", Content: "package worker\n"},
		{Path: "internal/worker/pool_test.go", Content: "func BenchmarkPool(b *testing.B) {}\n"},
	})
	for _, name := range []string{
		store.OpsDimensionTopology,
		store.OpsDimensionPermissions,
		store.OpsDimensionLoad,
	} {
		dim := opsDimensionOf(t, ops, name)
		if dim.Status != store.OpsStatusSatisfied {
			t.Errorf("%s = %+v, want satisfied", name, dim)
		}
	}
	assertEvidenceTag(t, ops, store.OpsDimensionTopology, "artifact:")
	assertEvidenceTag(t, ops, store.OpsDimensionPermissions, "test:")
	assertEvidenceTag(t, ops, store.OpsDimensionLoad, "measurement:")
}

// assertEvidenceTag requires the dimension to cite its evidence with the tag
// that separates a measurement or a test from a hypothesis.
func assertEvidenceTag(t *testing.T, ops store.OpsEvidence, name, tag string) {
	t.Helper()
	dim := opsDimensionOf(t, ops, name)
	if !opsContainsAny(strings.Join(dim.Evidence, "\n"), []string{tag}) {
		t.Errorf("%s evidence %v does not carry the %q tag", name, dim.Evidence, tag)
	}
}

func TestOpsEvidenceUnreadableEvidenceIsUnknown(t *testing.T) {
	ops := evaluateOpsEvidence([]opsChangeFile{
		{
			Path:       "migrations/00027_add_user_note.sql",
			Unreadable: true,
			ReadError:  "permission denied",
		},
	})
	dim := opsDimensionOf(t, ops, store.OpsDimensionSchema)
	if !dim.Applies {
		t.Fatal("schema dimension did not apply to an unreadable migration")
	}
	if dim.Status != store.OpsStatusUnknown {
		t.Fatalf("schema status = %q, want unknown", dim.Status)
	}
	missing := strings.Join(dim.Missing, ",")
	if !strings.Contains(missing, "readable") || !strings.Contains(missing, "permission denied") {
		t.Errorf("missing = %v, want the read error surfaced", dim.Missing)
	}
}

func TestOpsEvidenceIsReproducible(t *testing.T) {
	changes := []opsChangeFile{
		{
			Path: "migrations/00028_add_user_note.sql",
			Content: "-- +goose Up\n" +
				"ALTER TABLE users ADD COLUMN note text;\n" +
				"-- +goose Down\n" +
				"ALTER TABLE users DROP COLUMN note;\n",
		},
		{Path: "deploy/docker-compose.yml", Content: "services:\n  app:\n    image: app:v2\n"},
	}
	first := evaluateOpsEvidence(changes)
	second := evaluateOpsEvidence(changes)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("evaluation is not deterministic:\nfirst  = %+v\nsecond = %+v", first, second)
	}
}

func TestEvaluateRunOpsEvidenceRunsWithoutSignal(t *testing.T) {
	dir := t.TempDir()
	modelDir := filepath.Join(dir, "internal", "store")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	model := "type User struct { Email string `gorm:\"column:email\"` }"
	if err := os.WriteFile(filepath.Join(modelDir, "models.go"), []byte(model), 0o644); err != nil {
		t.Fatal(err)
	}
	inventory := []changedFile{{Path: "internal/store/models.go", Kind: changedModified}}

	ops, evaluated := evaluateRunOpsEvidence(store.ArchitectureAnalysisObservation, nil, dir, inventory)
	if !evaluated {
		t.Fatal("ops evidence was not evaluated in observation mode")
	}
	dim := opsDimensionOf(t, ops, store.OpsDimensionSchema)
	if !dim.Applies || dim.Status != store.OpsStatusUnknown {
		t.Errorf("schema = %+v, want applicable and unknown", dim)
	}

	if _, evaluated := evaluateRunOpsEvidence(store.ArchitectureAnalysisOff, nil, dir, inventory); evaluated {
		t.Error("ops evidence was evaluated with the mode off")
	}

	failed, evaluated := evaluateRunOpsEvidence(store.ArchitectureAnalysisObservation, errors.New("inventory failed"), dir, nil)
	if !evaluated {
		t.Fatal("a failed inventory did not produce an ops verdict")
	}
	if failed.Status != store.OpsStatusUnknown || !strings.Contains(failed.Error, "inventory failed") {
		t.Errorf("failed inventory verdict = %+v, want unknown with the error", failed)
	}
	if !failed.HasContent() {
		t.Error("failed inventory verdict has no content to publish")
	}
}

func TestOpsSafePathRejectsTraversalAndSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "internal", "x.go"), []byte("package internal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := opsSafePathFromRoot(root, "../outside.txt"); ok {
		t.Error("lexical traversal was accepted")
	}
	if _, _, ok := opsSafePathFromRoot(root, "internal/x.go"); !ok {
		t.Error("a normal path inside the checkout was rejected")
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	if _, _, ok := opsSafePathFromRoot(root, "link/secret.txt"); ok {
		t.Error("a parent symlink escaped the checkout")
	}
}

func TestCollectOpsChangesOnlyReadsEvidenceFiles(t *testing.T) {
	dir := t.TempDir()
	for path, content := range map[string]string{
		"src/app.go":                   "package src\n",
		"internal/store/models.go":     "type User struct { Email string `gorm:\"column:email\"` }\n",
		"internal/worker/pool_test.go": "func BenchmarkPool(b *testing.B) {}\n",
	} {
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	changes := collectOpsChanges(dir, []changedFile{
		{Path: "src/app.go", Kind: changedModified},
		{Path: "internal/store/models.go", Kind: changedModified},
		{Path: "internal/worker/pool_test.go", Kind: changedModified},
		{Path: "internal/store/migrations/00029_missing.sql", Kind: changedModified},
	})
	byPath := make(map[string]opsChangeFile, len(changes))
	for _, change := range changes {
		byPath[change.Path] = change
	}
	if byPath["src/app.go"].Content != "" {
		t.Error("an ordinary source file was read as evidence")
	}
	if byPath["internal/store/models.go"].Content == "" {
		t.Error("a model file was not read")
	}
	if byPath["internal/worker/pool_test.go"].Content == "" {
		t.Error("a benchmark test file was not read")
	}
	missing := byPath["internal/store/migrations/00029_missing.sql"]
	if !missing.Unreadable || missing.ReadError == "" {
		t.Errorf("a missing evidence file = %+v, want unreadable with an error", missing)
	}
}

func TestOpsEvidenceDoesNotChangeTheVerdictOrOutcome(t *testing.T) {
	verdict := store.ArchitectureVerdict{
		Status:    store.ArchitectureComplete,
		Requested: true,
		Mode:      store.ArchitectureAnalysisObservation,
		Risks:     []string{"engine risk"},
	}
	ops := evaluateOpsEvidence([]opsChangeFile{
		{
			Path: "migrations/00030_drop_user_email.sql",
			Content: "-- +goose Up\n" +
				"ALTER TABLE users DROP COLUMN email;\n" +
				"-- +goose Down\n" +
				"ALTER TABLE users ADD COLUMN email text;\n",
		},
	})
	got := applyOpsEvidence(verdict, ops)
	if got.Status != verdict.Status {
		t.Errorf("status changed from %q to %q", verdict.Status, got.Status)
	}
	if got.OpsEvidence == nil || got.OpsEvidence.Status != store.OpsStatusRisk {
		t.Errorf("ops evidence = %+v, want a risk verdict attached", got.OpsEvidence)
	}
	if !containsString(got.Risks, "engine risk") {
		t.Errorf("engine risk was dropped: %v", got.Risks)
	}
	if !opsContainsAny(strings.Join(got.Risks, "\n"), []string{"ops/schema"}) {
		t.Errorf("ops risk not folded into the verdict: %v", got.Risks)
	}
	if got.OpsEvidence.Controls.WritesDecision {
		t.Error("ops evidence claims a write decision")
	}

	// Applying twice must not duplicate the entries.
	again := applyOpsEvidence(got, ops)
	if len(again.Risks) != len(got.Risks) || len(again.Uncertainties) != len(got.Uncertainties) {
		t.Errorf("apply is not idempotent: %v/%v", again.Risks, again.Uncertainties)
	}

	// The review outcome is computed from the review status alone.
	status, code := nativeOutcome(nativeReview{Status: "complete"})
	if status != store.RunSucceeded || code != "" {
		t.Errorf("review outcome = %s/%q, want succeeded", status, code)
	}
}

func opsDimensionOf(t *testing.T, ops store.OpsEvidence, name string) store.OpsDimension {
	t.Helper()
	for _, dimension := range ops.Dimensions {
		if dimension.Name == name {
			return dimension
		}
	}
	t.Fatalf("dimension %q not found in %+v", name, ops.Dimensions)
	return store.OpsDimension{}
}
