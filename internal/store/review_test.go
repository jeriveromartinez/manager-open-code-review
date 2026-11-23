package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

func TestMigrateCreatesPhase2Tables(t *testing.T) {
	st := testutil.NewStore(t)
	for _, table := range []string{
		"llm_profiles", "qa_policies", "review_runs", "findings",
		"finding_events", "github_artifacts",
	} {
		if !st.DB.Migrator().HasTable(table) {
			t.Fatalf("missing table %s", table)
		}
	}
	if !st.DB.Migrator().HasColumn(&store.PullRequest{}, "is_fork") {
		t.Fatal("pull_requests.is_fork missing")
	}
	if !st.DB.Migrator().HasColumn(&store.PullRequest{}, "head_repo_full_name") {
		t.Fatal("pull_requests.head_repo_full_name missing")
	}
}

func TestOneActiveRunPerPullRequest(t *testing.T) {
	f := newFixture(t)

	installationID := f.nextID()
	f.installation(installationID)
	f.repo(installationID, installationID, "acme/app", nil, true)
	pr := f.openPR(installationID, 7, store.StatusPending, false)

	const insertRun = `INSERT INTO review_runs (pull_request_id, head_sha, "trigger", status, attempt)
		VALUES ($1, 'head', 'webhook', 'queued', 1)`
	if err := f.s.DB.WithContext(f.ctx).Exec(insertRun, pr.ID).Error; err != nil {
		t.Fatalf("insert first queued run: %v", err)
	}

	requireUniqueViolation(t, f.s.DB.WithContext(f.ctx), "second queued run",
		"idx_review_runs_active", insertRun, pr.ID)

	if err := f.s.DB.WithContext(f.ctx).Exec(
		`UPDATE review_runs SET status = 'superseded' WHERE pull_request_id = $1`, pr.ID,
	).Error; err != nil {
		t.Fatalf("supersede first run: %v", err)
	}
	if err := f.s.DB.WithContext(f.ctx).Exec(insertRun, pr.ID).Error; err != nil {
		t.Fatalf("insert run after supersede: %v", err)
	}
}

func TestArtifactPartialUniqueIndexes(t *testing.T) {
	f := newFixture(t)
	db := f.s.DB.WithContext(f.ctx)

	installationID := f.nextID()
	f.installation(installationID)
	f.repo(installationID, installationID, "acme/app", nil, true)
	pr := f.openPR(installationID, 13, store.StatusPending, false)

	const insertArtifact = `INSERT INTO github_artifacts
		(pull_request_id, kind, head_sha, external_id) VALUES ($1, $2, $3, $4)`
	insert := func(kind, sha, externalID string) {
		t.Helper()
		if err := db.Exec(insertArtifact, pr.ID, kind, sha, externalID).Error; err != nil {
			t.Fatalf("insert %s artifact for %s: %v", kind, sha, err)
		}
	}
	reject := func(label, kind, sha, constraint string) {
		t.Helper()
		requireUniqueViolation(t, db, label, constraint, insertArtifact, pr.ID, kind, sha, "dup")
	}

	insert(store.ArtifactSummary, "sha-a", "1")
	// The summary index ignores head_sha: only one summary per PR.
	reject("second summary", store.ArtifactSummary, "sha-b", "idx_artifacts_summary")

	insert(store.ArtifactReview, "sha-a", "2")
	insert(store.ArtifactReview, "sha-b", "3")
	reject("duplicate review sha", store.ArtifactReview, "sha-a", "idx_artifacts_sha")
	// A different kind may publish an artifact for the same SHA.
	insert(store.ArtifactCheck, "sha-a", "4")
}

func TestCascadeDeletePullRequestRemovesReviewRows(t *testing.T) {
	f := newFixture(t)
	db := f.s.DB.WithContext(f.ctx)

	installationID := f.nextID()
	f.installation(installationID)
	f.repo(installationID, installationID, "acme/app", nil, true)
	pr := f.openPR(installationID, 17, store.StatusPending, false)

	run := store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, Status: store.RunQueued, Attempt: 1,
	}
	if err := db.Create(&run).Error; err != nil {
		t.Fatalf("create review run: %v", err)
	}
	finding := store.Finding{
		ID: "cascade-1", PullRequestID: pr.ID, Fingerprint: "fp",
		Status: store.FindingOpen, FirstSeenRunID: run.ID, LastSeenRunID: run.ID,
	}
	if err := db.Create(&finding).Error; err != nil {
		t.Fatalf("create finding: %v", err)
	}
	event := store.FindingEvent{FindingID: finding.ID, ReviewRunID: run.ID, Kind: "created"}
	if err := db.Create(&event).Error; err != nil {
		t.Fatalf("create finding event: %v", err)
	}
	artifact := store.GitHubArtifact{
		PullRequestID: pr.ID, Kind: store.ArtifactSummary, HeadSHA: pr.HeadSHA,
	}
	if err := db.Create(&artifact).Error; err != nil {
		t.Fatalf("create github artifact: %v", err)
	}

	for _, table := range []string{"review_runs", "findings", "finding_events", "github_artifacts"} {
		requireCount(t, db, table, 1)
	}

	if err := db.Delete(&store.PullRequest{}, pr.ID).Error; err != nil {
		t.Fatalf("delete pull request %d: %v", pr.ID, err)
	}
	for _, table := range []string{"review_runs", "findings", "finding_events", "github_artifacts"} {
		requireCount(t, db, table, 0)
	}
}

func requireCount(t *testing.T, db *gorm.DB, table string, want int64) {
	t.Helper()

	var got int64
	if err := db.Table(table).Count(&got).Error; err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if got != want {
		t.Errorf("%s rows = %d, want %d", table, got, want)
	}
}

// requireUniqueViolation runs a statement expected to fail on a named unique
// index. A silent logger keeps the expected database error out of test output.
func requireUniqueViolation(t *testing.T, db *gorm.DB, label, constraint, query string, args ...any) {
	t.Helper()

	quiet := db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
	err := quiet.Exec(query, args...).Error
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("%s: got error %v, want unique violation", label, err)
	}
	if pgErr.ConstraintName != constraint {
		t.Fatalf("%s: violated constraint %q, want %q", label, pgErr.ConstraintName, constraint)
	}
}

func TestPhase2ModelsPersist(t *testing.T) {
	f := newFixture(t)
	db := f.s.DB.WithContext(f.ctx)

	team := f.team("qa")
	profile := store.LLMProfile{
		Name: "claude", Provider: "anthropic", Model: "claude-sonnet",
		Mode: "api_key", APIKeyEnc: "sealed", Concurrency: 2, Enabled: true,
		HealthStatus: store.HealthUnknown,
	}
	if err := db.Create(&profile).Error; err != nil {
		t.Fatalf("create llm profile: %v", err)
	}

	policy := store.QAPolicy{
		TeamID: team.ID, LLMProfileID: profile.ID,
		BlockingThreshold: "major",
		ExcludeGlobs:      datatypes.JSON([]byte(`["*.md"]`)),
		MaxFilesPerPass:   40, MaxDiffLinesPerPass: 3000, MaxPasses: 4,
	}
	if err := db.Create(&policy).Error; err != nil {
		t.Fatalf("create qa policy: %v", err)
	}

	installationID := f.nextID()
	f.installation(installationID)
	f.repo(installationID, installationID, "acme/app", &team.ID, true)
	pr := f.openPR(installationID, 11, store.StatusPending, false)

	run := store.ReviewRun{
		PullRequestID: pr.ID, ProfileID: profile.ID, HeadSHA: pr.HeadSHA,
		Trigger: "webhook", Status: store.RunQueued, Attempt: 1,
		Coverage: datatypes.JSON([]byte(`{"reviewed_files":1}`)),
		Usage:    datatypes.JSON([]byte(`{"input_tokens":1}`)),
	}
	if err := db.Create(&run).Error; err != nil {
		t.Fatalf("create review run: %v", err)
	}

	finding := store.Finding{
		ID: "finding-1", PullRequestID: pr.ID, Fingerprint: "fp",
		Path: "main.go", StartLine: 1, EndLine: 2,
		Severity: "major", Status: store.FindingOpen,
		FirstSeenRunID: run.ID, LastSeenRunID: run.ID,
	}
	if err := db.Create(&finding).Error; err != nil {
		t.Fatalf("create finding: %v", err)
	}

	event := store.FindingEvent{
		FindingID: finding.ID, ReviewRunID: run.ID, Kind: "created",
		Data: datatypes.JSON([]byte(`{}`)),
	}
	if err := db.Create(&event).Error; err != nil {
		t.Fatalf("create finding event: %v", err)
	}

	artifact := store.GitHubArtifact{
		PullRequestID: pr.ID, Kind: store.ArtifactSummary, HeadSHA: pr.HeadSHA,
		ExternalID: "123", URL: "https://github.com/comment",
	}
	if err := db.Create(&artifact).Error; err != nil {
		t.Fatalf("create github artifact: %v", err)
	}

	var gotPolicy store.QAPolicy
	if err := db.Take(&gotPolicy, policy.ID).Error; err != nil {
		t.Fatalf("load qa policy: %v", err)
	}
	assertJSONEqual(t, "exclude_globs", gotPolicy.ExcludeGlobs, `["*.md"]`)

	var gotRun store.ReviewRun
	if err := db.Take(&gotRun, run.ID).Error; err != nil {
		t.Fatalf("load review run: %v", err)
	}
	assertJSONEqual(t, "coverage", gotRun.Coverage, `{"reviewed_files":1}`)
	assertJSONEqual(t, "usage", gotRun.Usage, `{"input_tokens":1}`)

	var gotFinding store.Finding
	if err := db.Where("id = ?", finding.ID).Take(&gotFinding).Error; err != nil {
		t.Fatalf("load finding: %v", err)
	}
	if gotFinding.ID != "finding-1" || gotFinding.StartLine != 1 || gotFinding.EndLine != 2 {
		t.Fatalf("finding round-trip mismatch: %+v", gotFinding)
	}

	var gotArtifact store.GitHubArtifact
	if err := db.Take(&gotArtifact, artifact.ID).Error; err != nil {
		t.Fatalf("load github artifact: %v", err)
	}
	if gotArtifact.Kind != store.ArtifactSummary {
		t.Fatalf("artifact kind = %q, want %q", gotArtifact.Kind, store.ArtifactSummary)
	}

	var gotPR store.PullRequest
	if err := db.Take(&gotPR, pr.ID).Error; err != nil {
		t.Fatalf("load pull request: %v", err)
	}
	if gotPR.IsFork || gotPR.HeadRepoFullName != "" {
		t.Fatalf("unexpected fork fields: %+v", gotPR)
	}
}

// assertJSONEqual compares JSON semantically: jsonb normalizes whitespace and
// key order on the way to the database.
func assertJSONEqual(t *testing.T, field string, got datatypes.JSON, want string) {
	t.Helper()

	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("%s is not valid JSON: %v", field, err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("want %s is not valid JSON: %v", field, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("%s = %s, want %s", field, got, want)
	}
}

func TestPhase2Constants(t *testing.T) {
	cases := []struct{ name, got, want string }{
		{"RunQueued", store.RunQueued, "queued"},
		{"RunRunning", store.RunRunning, "running"},
		{"RunSucceeded", store.RunSucceeded, "succeeded"},
		{"RunInconclusive", store.RunInconclusive, "inconclusive"},
		{"RunFailed", store.RunFailed, "failed"},
		{"RunSuperseded", store.RunSuperseded, "superseded"},
		{"HealthUnknown", store.HealthUnknown, "unknown"},
		{"HealthOK", store.HealthOK, "ok"},
		{"HealthError", store.HealthError, "error"},
		{"FindingOpen", store.FindingOpen, "open"},
		{"FindingResolved", store.FindingResolved, "resolved"},
		{"FindingWithdrawn", store.FindingWithdrawn, "withdrawn"},
		{"ArtifactSummary", store.ArtifactSummary, "summary_comment"},
		{"ArtifactReview", store.ArtifactReview, "review"},
		{"ArtifactCheck", store.ArtifactCheck, "check_run"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestLLMProfileCRUD(t *testing.T) {
	f := newFixture(t)

	baseURL := "https://llm.example.com/v1"
	variant := "latest"
	zeta, err := f.s.CreateLLMProfile(f.ctx, store.LLMProfile{
		Name: "zeta", Provider: "anthropic", Model: "claude-sonnet",
		Variant: &variant, Mode: "api_key", APIKeyEnc: "sealed-worker-key",
		BaseURL: &baseURL, Concurrency: 2, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateLLMProfile(zeta): %v", err)
	}
	if zeta.ID == 0 {
		t.Fatal("CreateLLMProfile returned ID 0")
	}
	if zeta.HealthStatus != store.HealthUnknown {
		t.Errorf("new profile health = %q, want %q", zeta.HealthStatus, store.HealthUnknown)
	}

	if _, err := f.s.CreateLLMProfile(f.ctx, store.LLMProfile{
		Name: "alpha", Provider: "openai", Model: "gpt-test",
		APIKeyEnc: "sealed-other-key", Concurrency: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateLLMProfile(alpha): %v", err)
	}

	got, err := f.s.LLMProfile(f.ctx, zeta.ID)
	if err != nil {
		t.Fatalf("LLMProfile(%d): %v", zeta.ID, err)
	}
	if got.Name != "zeta" || got.Provider != "anthropic" || got.Model != "claude-sonnet" {
		t.Errorf("LLMProfile() identity mismatch: %+v", got)
	}
	if got.APIKeyEnc != "sealed-worker-key" {
		t.Errorf("APIKeyEnc = %q, want the stored ciphertext unchanged", got.APIKeyEnc)
	}
	if got.BaseURL == nil || *got.BaseURL != baseURL {
		t.Errorf("BaseURL = %v, want %q", got.BaseURL, baseURL)
	}
	if got.Variant == nil || *got.Variant != variant {
		t.Errorf("Variant = %v, want %q", got.Variant, variant)
	}
	if got.Concurrency != 2 || !got.Enabled {
		t.Errorf("Concurrency/Enabled = %d/%v, want 2/true", got.Concurrency, got.Enabled)
	}

	profiles, err := f.s.ListLLMProfiles(f.ctx)
	if err != nil {
		t.Fatalf("ListLLMProfiles: %v", err)
	}
	if len(profiles) != 2 {
		t.Fatalf("ListLLMProfiles returned %d profiles, want 2", len(profiles))
	}
	// Created zeta first: sorted output proves ListLLMProfiles orders by name.
	if profiles[0].Name != "alpha" || profiles[1].Name != "zeta" {
		t.Errorf("ListLLMProfiles order = %q, %q; want alpha, zeta", profiles[0].Name, profiles[1].Name)
	}

	got.Name = "zeta-renamed"
	got.Concurrency = 5
	if err := f.s.UpdateLLMProfile(f.ctx, got); err != nil {
		t.Fatalf("UpdateLLMProfile: %v", err)
	}
	updated, err := f.s.LLMProfile(f.ctx, zeta.ID)
	if err != nil {
		t.Fatalf("LLMProfile(%d) after update: %v", zeta.ID, err)
	}
	if updated.Name != "zeta-renamed" || updated.Concurrency != 5 {
		t.Errorf("after update name/concurrency = %q/%d, want zeta-renamed/5", updated.Name, updated.Concurrency)
	}
	if updated.APIKeyEnc != "sealed-worker-key" {
		t.Errorf("after update APIKeyEnc = %q, want ciphertext preserved", updated.APIKeyEnc)
	}
	if updated.HealthStatus != store.HealthUnknown {
		t.Errorf("after update health = %q, want it left at %q", updated.HealthStatus, store.HealthUnknown)
	}

	if err := f.s.DeleteLLMProfile(f.ctx, zeta.ID); err != nil {
		t.Fatalf("DeleteLLMProfile(%d): %v", zeta.ID, err)
	}
	// GORM logs a miss as an error; keep the expected miss out of test output.
	quiet := &store.Store{
		DB:   f.s.DB.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}),
		Pool: f.s.Pool,
	}
	if _, err := quiet.LLMProfile(f.ctx, zeta.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LLMProfile(deleted) error = %v, want ErrNotFound", err)
	}
	profiles, err = f.s.ListLLMProfiles(f.ctx)
	if err != nil {
		t.Fatalf("ListLLMProfiles after delete: %v", err)
	}
	if len(profiles) != 1 || profiles[0].Name != "alpha" {
		t.Errorf("ListLLMProfiles after delete = %+v, want only alpha", profiles)
	}
}

func TestUpdateLLMProfilePreservesKeyWhenOmitted(t *testing.T) {
	f := newFixture(t)

	created, err := f.s.CreateLLMProfile(f.ctx, store.LLMProfile{
		Name: "keyed", Provider: "anthropic", Model: "claude-sonnet",
		APIKeyEnc: "sealed-do-not-wipe", Concurrency: 2, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateLLMProfile: %v", err)
	}

	got, err := f.s.LLMProfile(f.ctx, created.ID)
	if err != nil {
		t.Fatalf("LLMProfile(%d): %v", created.ID, err)
	}
	got.Name = "keyed-renamed"
	got.Concurrency = 7
	got.APIKeyEnc = ""
	if err := f.s.UpdateLLMProfile(f.ctx, got); err != nil {
		t.Fatalf("UpdateLLMProfile: %v", err)
	}

	updated, err := f.s.LLMProfile(f.ctx, created.ID)
	if err != nil {
		t.Fatalf("LLMProfile(%d) after update: %v", created.ID, err)
	}
	if updated.APIKeyEnc != "sealed-do-not-wipe" {
		t.Errorf("APIKeyEnc = %q, want the stored ciphertext preserved", updated.APIKeyEnc)
	}
	if updated.Name != "keyed-renamed" || updated.Concurrency != 7 {
		t.Errorf("after update name/concurrency = %q/%d, want keyed-renamed/7", updated.Name, updated.Concurrency)
	}
}

func TestDeleteProfileInUse(t *testing.T) {
	f := newFixture(t)

	profile, err := f.s.CreateLLMProfile(f.ctx, store.LLMProfile{
		Name: "used", APIKeyEnc: "sealed", Concurrency: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateLLMProfile: %v", err)
	}
	team := f.team("qa")
	policy := store.QAPolicy{TeamID: team.ID, LLMProfileID: profile.ID}
	if err := f.s.DB.WithContext(f.ctx).Create(&policy).Error; err != nil {
		t.Fatalf("create qa policy: %v", err)
	}

	if err := f.s.DeleteLLMProfile(f.ctx, profile.ID); !errors.Is(err, store.ErrProfileInUse) {
		t.Fatalf("DeleteLLMProfile(in use) error = %v, want ErrProfileInUse", err)
	}
	if _, err := f.s.LLMProfile(f.ctx, profile.ID); err != nil {
		t.Errorf("profile disappeared after refused delete: %v", err)
	}
}

func TestSetLLMHealth(t *testing.T) {
	f := newFixture(t)

	profile, err := f.s.CreateLLMProfile(f.ctx, store.LLMProfile{
		Name: "health", APIKeyEnc: "sealed", Concurrency: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateLLMProfile: %v", err)
	}

	if err := f.s.SetLLMHealth(f.ctx, profile.ID, store.HealthError, "timeout contacting provider"); err != nil {
		t.Fatalf("SetLLMHealth: %v", err)
	}
	got, err := f.s.LLMProfile(f.ctx, profile.ID)
	if err != nil {
		t.Fatalf("LLMProfile(%d): %v", profile.ID, err)
	}
	if got.HealthStatus != store.HealthError || got.HealthError != "timeout contacting provider" {
		t.Errorf("health = %q/%q, want error/timeout contacting provider", got.HealthStatus, got.HealthError)
	}
	if got.HealthCheckedAt == nil || time.Since(*got.HealthCheckedAt) > time.Minute {
		t.Errorf("HealthCheckedAt = %v, want a recent timestamp", got.HealthCheckedAt)
	}

	if err := f.s.SetLLMHealth(f.ctx, 999999, store.HealthOK, ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetLLMHealth(unknown) error = %v, want ErrNotFound", err)
	}
}

// TestLockProfileSerializes keeps its original intent (one profile, one agent
// at a time) against the slot API: at concurrency 1 the second caller is
// refused instead of blocking, and unlock stays idempotent.
func TestLockProfileSerializes(t *testing.T) {
	f := newFixture(t)

	profile, err := f.s.CreateLLMProfile(f.ctx, store.LLMProfile{
		Name: "locked", APIKeyEnc: "sealed", Concurrency: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateLLMProfile: %v", err)
	}

	release, acquired, err := f.s.AcquireProfileSlot(f.ctx, profile.ID)
	if err != nil || !acquired {
		t.Fatalf("first AcquireProfileSlot() = %v/%v", acquired, err)
	}

	if _, acquired, err := f.s.AcquireProfileSlot(f.ctx, profile.ID); err != nil {
		t.Fatalf("second AcquireProfileSlot(): %v", err)
	} else if acquired {
		t.Fatal("second AcquireProfileSlot acquired while the first caller held the only slot")
	}

	// release must be idempotent: a second call must not free the slot now
	// held by another caller.
	release()
	release()

	next, acquired, err := f.s.AcquireProfileSlot(f.ctx, profile.ID)
	if err != nil || !acquired {
		t.Fatalf("AcquireProfileSlot() after release = %v/%v, want acquired", acquired, err)
	}
	next()
}

// llmProfile creates a profile directly so tests can choose its enabled state.
func (f *fixture) llmProfile(name string, enabled bool) store.LLMProfile {
	f.t.Helper()

	p := store.LLMProfile{
		Name: name, Provider: "anthropic", Model: "claude-test",
		Mode: "api_key", APIKeyEnc: "sealed", Concurrency: 1,
		Enabled: enabled, HealthStatus: store.HealthUnknown,
	}
	if err := f.s.DB.WithContext(f.ctx).Create(&p).Error; err != nil {
		f.t.Fatalf("create llm profile %q: %v", name, err)
	}
	return p
}

func requireNoReviewConfig(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, store.ErrNoReviewConfig) {
		t.Fatalf("error = %v, want ErrNoReviewConfig", err)
	}
}

func requireNotFound(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestSavePolicyDefaults(t *testing.T) {
	f := newFixture(t)
	team := f.team("qa")
	profile := f.llmProfile("policy-profile", true)

	got, err := f.s.SaveQAPolicy(f.ctx, store.QAPolicy{
		TeamID: team.ID, LLMProfileID: profile.ID,
	})
	if err != nil {
		t.Fatalf("SaveQAPolicy: %v", err)
	}
	if got.ID == 0 || got.TeamID != team.ID || got.LLMProfileID != profile.ID {
		t.Fatalf("SaveQAPolicy returned wrong identity: %+v", got)
	}
	if got.BlockingThreshold != "major" || got.MaxFilesPerPass != 40 ||
		got.MaxDiffLinesPerPass != 3000 || got.MaxPasses != 4 {
		t.Errorf("defaults = %q/%d/%d/%d, want major/40/3000/4",
			got.BlockingThreshold, got.MaxFilesPerPass, got.MaxDiffLinesPerPass, got.MaxPasses)
	}
	assertJSONEqual(t, "exclude_globs", got.ExcludeGlobs, `[]`)

	// Saving again for the same team is an upsert: one row, same primary key.
	updated, err := f.s.SaveQAPolicy(f.ctx, store.QAPolicy{
		TeamID: team.ID, LLMProfileID: profile.ID,
		BlockingThreshold: "critical", MaxFilesPerPass: 12,
	})
	if err != nil {
		t.Fatalf("SaveQAPolicy(second call): %v", err)
	}
	if updated.ID != got.ID {
		t.Errorf("upsert changed policy ID from %d to %d", got.ID, updated.ID)
	}
	if updated.BlockingThreshold != "critical" || updated.MaxFilesPerPass != 12 {
		t.Errorf("upserted values = %q/%d, want critical/12", updated.BlockingThreshold, updated.MaxFilesPerPass)
	}
	if updated.MaxDiffLinesPerPass != 3000 || updated.MaxPasses != 4 {
		t.Errorf("upsert lost defaults: %+v", updated)
	}
	requireCount(t, f.s.DB.WithContext(f.ctx), "qa_policies", 1)

	// Explicit values, including globs, are kept as given.
	globs := datatypes.JSON([]byte(`["*.md","vendor/**"]`))
	withGlobs, err := f.s.SaveQAPolicy(f.ctx, store.QAPolicy{
		TeamID: team.ID, LLMProfileID: profile.ID, ExcludeGlobs: globs,
	})
	if err != nil {
		t.Fatalf("SaveQAPolicy(globs): %v", err)
	}
	assertJSONEqual(t, "exclude_globs", withGlobs.ExcludeGlobs, `["*.md","vendor/**"]`)
	requireCount(t, f.s.DB.WithContext(f.ctx), "qa_policies", 1)

	loaded, err := f.s.QAPolicyForTeam(f.ctx, team.ID)
	if err != nil {
		t.Fatalf("QAPolicyForTeam: %v", err)
	}
	if loaded.ID != got.ID || loaded.BlockingThreshold != "major" {
		t.Errorf("QAPolicyForTeam = %+v, want the persisted policy", loaded)
	}
	requireNotFound(t, func() error {
		_, err := f.s.QAPolicyForTeam(f.ctx, 999999)
		return err
	}())
}

// TestSavePolicyReviewControls pins the per-team cost controls added in #72:
// zero values become low/0/no override, an upsert preserves and clears them,
// and the engine's documented bounds are enforced before they reach a CHECK.
func TestSavePolicyReviewControls(t *testing.T) {
	f := newFixture(t)
	team := f.team("cost")
	profile := f.llmProfile("cost-profile", true)

	got, err := f.s.SaveQAPolicy(f.ctx, store.QAPolicy{
		TeamID: team.ID, LLMProfileID: profile.ID,
	})
	if err != nil {
		t.Fatalf("SaveQAPolicy: %v", err)
	}
	if got.ReviewEffort != store.ReviewEffortLow || got.ReviewTokenBudget != 0 ||
		got.ReviewContextCeiling != nil {
		t.Errorf("zero-value controls = %q/%d/%v, want low/0/nil",
			got.ReviewEffort, got.ReviewTokenBudget, got.ReviewContextCeiling)
	}

	ceiling := 120000
	saved, err := f.s.SaveQAPolicy(f.ctx, store.QAPolicy{
		TeamID: team.ID, LLMProfileID: profile.ID,
		ReviewContextCeiling: &ceiling, ReviewEffort: store.ReviewEffortHigh,
		ReviewTokenBudget: 750000,
	})
	if err != nil {
		t.Fatalf("SaveQAPolicy(controls): %v", err)
	}
	if saved.ReviewContextCeiling == nil || *saved.ReviewContextCeiling != ceiling ||
		saved.ReviewEffort != store.ReviewEffortHigh || saved.ReviewTokenBudget != 750000 {
		t.Errorf("saved controls = %+v, want ceiling=%d/high/750000", saved, ceiling)
	}

	// A later save is a full replacement: omitting the ceiling clears the
	// override and the other controls take their new values.
	cleared, err := f.s.SaveQAPolicy(f.ctx, store.QAPolicy{
		TeamID: team.ID, LLMProfileID: profile.ID,
		ReviewEffort: store.ReviewEffortMedium, ReviewTokenBudget: 0,
	})
	if err != nil {
		t.Fatalf("SaveQAPolicy(clear): %v", err)
	}
	if cleared.ID != saved.ID {
		t.Errorf("upsert changed policy ID from %d to %d", saved.ID, cleared.ID)
	}
	if cleared.ReviewContextCeiling != nil || cleared.ReviewEffort != store.ReviewEffortMedium ||
		cleared.ReviewTokenBudget != 0 {
		t.Errorf("cleared controls = %+v, want nil/medium/0", cleared)
	}

	for _, tc := range []struct {
		name   string
		policy store.QAPolicy
	}{
		{"unknown effort", store.QAPolicy{TeamID: team.ID, LLMProfileID: profile.ID, ReviewEffort: "extreme"}},
		{"negative budget", store.QAPolicy{TeamID: team.ID, LLMProfileID: profile.ID, ReviewTokenBudget: -1}},
		{"budget above max", store.QAPolicy{TeamID: team.ID, LLMProfileID: profile.ID, ReviewTokenBudget: store.ReviewTokenBudgetMax + 1}},
		{"ceiling below min", store.QAPolicy{TeamID: team.ID, LLMProfileID: profile.ID, ReviewContextCeiling: intPtr(store.ReviewContextCeilingMin - 1)}},
		{"ceiling above max", store.QAPolicy{TeamID: team.ID, LLMProfileID: profile.ID, ReviewContextCeiling: intPtr(store.ReviewContextCeilingMax + 1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.s.SaveQAPolicy(f.ctx, tc.policy); err == nil {
				t.Error("SaveQAPolicy accepted an out-of-range control")
			}
		})
	}
}

// The GORM default tag keeps a direct zero-value insert from violating the
// review_effort CHECK constraint, which is what makes non-API writers safe.
func TestQAPolicyEffortColumnDefault(t *testing.T) {
	f := newFixture(t)
	team := f.team("direct")
	profile := f.llmProfile("direct-profile", true)

	row := store.QAPolicy{
		TeamID: team.ID, LLMProfileID: profile.ID,
		BlockingThreshold: "major", MaxFilesPerPass: 40, MaxDiffLinesPerPass: 3000, MaxPasses: 4,
	}
	if err := f.s.DB.WithContext(f.ctx).Create(&row).Error; err != nil {
		t.Fatalf("Create(zero-value policy): %v", err)
	}
	loaded, err := f.s.QAPolicyForTeam(f.ctx, team.ID)
	if err != nil {
		t.Fatalf("QAPolicyForTeam: %v", err)
	}
	if loaded.ReviewEffort != store.ReviewEffortLow {
		t.Errorf("ReviewEffort = %q, want the column default %q", loaded.ReviewEffort, store.ReviewEffortLow)
	}
}

// The API validates these bounds first, so only a direct writer reaches the
// CHECKs. Pinning them keeps a hand-written UPDATE from storing a value the
// engine cannot honour.
func TestQAPolicyReviewControlChecks(t *testing.T) {
	f := newFixture(t)
	team := f.team("checks")
	profile := f.llmProfile("checks-profile", true)
	if _, err := f.s.SaveQAPolicy(f.ctx, store.QAPolicy{
		TeamID: team.ID, LLMProfileID: profile.ID,
	}); err != nil {
		t.Fatalf("SaveQAPolicy: %v", err)
	}

	reject := func(label, constraint, query string, args ...any) {
		t.Helper()
		err := f.s.DB.WithContext(f.ctx).Exec(query, args...).Error
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.ConstraintName != constraint {
			t.Fatalf("%s: error = %v, want constraint %q", label, err, constraint)
		}
	}
	reject("ceiling below min", "qa_policies_review_context_ceiling_check",
		"UPDATE qa_policies SET review_context_ceiling = ? WHERE team_id = ?", store.ReviewContextCeilingMin-1, team.ID)
	reject("ceiling above max", "qa_policies_review_context_ceiling_check",
		"UPDATE qa_policies SET review_context_ceiling = ? WHERE team_id = ?", store.ReviewContextCeilingMax+1, team.ID)
	reject("unknown effort", "qa_policies_review_effort_check",
		"UPDATE qa_policies SET review_effort = ? WHERE team_id = ?", "extreme", team.ID)
	reject("negative budget", "qa_policies_review_token_budget_check",
		"UPDATE qa_policies SET review_token_budget = ? WHERE team_id = ?", -1, team.ID)
	reject("budget above max", "qa_policies_review_token_budget_check",
		"UPDATE qa_policies SET review_token_budget = ? WHERE team_id = ?", store.ReviewTokenBudgetMax+1, team.ID)
}

// The Down section has to remove the columns, not only the constraints: a
// rollback that leaves them makes the previous revision fail its own inserts.
func TestReviewPolicyControlsMigrationDown(t *testing.T) {
	s := testutil.NewStore(t)
	sqlDB, err := s.DB.DB()
	if err != nil {
		t.Fatalf("sql.DB: %v", err)
	}
	goose.SetBaseFS(os.DirFS("."))
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	if err := goose.DownToContext(context.Background(), sqlDB, "migrations", 9); err != nil {
		t.Fatalf("migrate down to 00009: %v", err)
	}
	for _, column := range []string{"review_context_ceiling", "review_effort", "review_token_budget"} {
		if s.DB.Migrator().HasColumn("qa_policies", column) {
			t.Errorf("column %q still exists after Down", column)
		}
	}
}

func intPtr(v int) *int { return &v }

type configSeed struct {
	f            *fixture
	installation store.Installation
	team         store.Team
	profile      store.LLMProfile
	policy       store.QAPolicy
	repo         store.Repository
	pr           store.PullRequest
}

func newConfigSeed(t *testing.T) *configSeed {
	t.Helper()

	f := newFixture(t)
	id := f.nextID()
	inst := f.installation(id)
	team := f.team("qa")
	profile := f.llmProfile("config-profile", true)
	policy, err := f.s.SaveQAPolicy(f.ctx, store.QAPolicy{
		TeamID: team.ID, LLMProfileID: profile.ID,
	})
	if err != nil {
		t.Fatalf("SaveQAPolicy: %v", err)
	}
	repo := f.repo(id, id, "acme/app", &team.ID, true)
	pr := f.openPR(id, 7, store.StatusPending, false)
	return &configSeed{f: f, installation: inst, team: team, profile: profile, policy: policy, repo: repo, pr: pr}
}

func TestReviewConfigForPR(t *testing.T) {
	t.Run("complete config", func(t *testing.T) {
		seed := newConfigSeed(t)
		got, err := seed.f.s.ReviewConfigForPR(seed.f.ctx, seed.pr.ID)
		if err != nil {
			t.Fatalf("ReviewConfigForPR: %v", err)
		}
		if got.PR.ID != seed.pr.ID || got.Repo.ID != seed.repo.ID ||
			got.Policy.ID != seed.policy.ID || got.Profile.ID != seed.profile.ID ||
			got.Installation.ID != seed.installation.ID {
			t.Fatalf("incomplete config: %+v", got)
		}
		if got.Policy.BlockingThreshold != "major" || got.Profile.APIKeyEnc != "sealed" {
			t.Errorf("config did not load policy/profile contents: %+v", got)
		}
	})

	for _, tc := range []struct {
		name   string
		mutate func(*configSeed) error
	}{
		{"repo disabled", func(c *configSeed) error {
			return c.f.s.DB.WithContext(c.f.ctx).Model(&store.Repository{}).
				Where("id = ?", c.repo.ID).Update("enabled", false).Error
		}},
		{"repo without team", func(c *configSeed) error {
			return c.f.s.DB.WithContext(c.f.ctx).Exec(
				`UPDATE repositories SET team_id = NULL WHERE id = ?`, c.repo.ID,
			).Error
		}},
		{"draft PR", func(c *configSeed) error {
			return c.f.s.DB.WithContext(c.f.ctx).Model(&store.PullRequest{}).
				Where("id = ?", c.pr.ID).Update("draft", true).Error
		}},
		{"fork PR", func(c *configSeed) error {
			return c.f.s.DB.WithContext(c.f.ctx).Model(&store.PullRequest{}).
				Where("id = ?", c.pr.ID).Update("is_fork", true).Error
		}},
		{"missing policy", func(c *configSeed) error {
			return c.f.s.DB.WithContext(c.f.ctx).Delete(&store.QAPolicy{}, c.policy.ID).Error
		}},
		{"disabled profile", func(c *configSeed) error {
			return c.f.s.DB.WithContext(c.f.ctx).Model(&store.LLMProfile{}).
				Where("id = ?", c.profile.ID).Update("enabled", false).Error
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seed := newConfigSeed(t)
			if err := tc.mutate(seed); err != nil {
				t.Fatalf("set up %q: %v", tc.name, err)
			}
			_, err := seed.f.s.ReviewConfigForPR(seed.f.ctx, seed.pr.ID)
			requireNoReviewConfig(t, err)
		})
	}
}

// TestQueuedRunsForTokenRefreshAndSetCloneToken pins the two halves of the
// token renewal: which queued runs need a fresh token, and that a run claimed
// meanwhile is never overwritten.
func TestQueuedRunsForTokenRefreshAndSetCloneToken(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UTC()

	installID := f.nextID()
	f.installation(installID)
	f.repo(installID, installID, "acme/tokens", nil, true)

	nearRun := f.run(f.openPR(installID, 1, store.StatusPending, false), store.RunQueued)
	farRun := f.run(f.openPR(installID, 2, store.StatusPending, false), store.RunQueued)
	unknownRun := f.run(f.openPR(installID, 3, store.StatusPending, false), store.RunQueued)
	runningRun := f.run(f.openPR(installID, 4, store.StatusPending, false), store.RunRunning)

	db := f.s.DB.WithContext(f.ctx)
	updates := []struct {
		id     uint
		expiry time.Time
	}{
		{nearRun.ID, now.Add(10 * time.Minute)},
		{farRun.ID, now.Add(time.Hour)},
		{runningRun.ID, now.Add(time.Minute)},
	}
	for _, u := range updates {
		if err := db.Model(&store.ReviewRun{}).Where("id = ?", u.id).
			Update("clone_token_expires_at", u.expiry).Error; err != nil {
			t.Fatalf("set expiry of run %d: %v", u.id, err)
		}
	}

	runs, err := f.s.QueuedRunsForTokenRefresh(f.ctx, now.Add(30*time.Minute), 10)
	if err != nil {
		t.Fatalf("QueuedRunsForTokenRefresh() error = %v", err)
	}
	want := map[uint]bool{nearRun.ID: true, unknownRun.ID: true}
	if len(runs) != len(want) {
		t.Fatalf("runs = %+v, want the near-expiry and unknown-expiry queued runs", runs)
	}
	for _, run := range runs {
		if !want[run.RunID] {
			t.Errorf("run %d should not need a refresh", run.RunID)
		}
		if run.InstallationID != installID || run.Repository != "acme/tokens" {
			t.Errorf("run %d = installation %d repository %q, want %d/acme/tokens",
				run.RunID, run.InstallationID, run.Repository, installID)
		}
	}

	expires := now.Add(time.Hour).Truncate(time.Microsecond)
	ok, err := f.s.SetCloneToken(f.ctx, nearRun.ID, "sealed-new", expires)
	if err != nil || !ok {
		t.Fatalf("SetCloneToken(queued) = %v/%v, want true", ok, err)
	}
	got, err := f.s.ReviewRun(f.ctx, nearRun.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if got.CloneTokenEnc != "sealed-new" || got.CloneTokenExpiresAt == nil || !got.CloneTokenExpiresAt.Equal(expires) {
		t.Errorf("run token = %q expiry %v, want sealed-new/%v", got.CloneTokenEnc, got.CloneTokenExpiresAt, expires)
	}

	ok, err = f.s.SetCloneToken(f.ctx, runningRun.ID, "sealed-running", expires)
	if err != nil {
		t.Fatalf("SetCloneToken(running) error = %v", err)
	}
	if ok {
		t.Error("SetCloneToken overwrote a run that was no longer queued")
	}
}

func (f *fixture) run(pr store.PullRequest, status string) store.ReviewRun {
	f.t.Helper()

	run := store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, Status: status, Attempt: 1,
	}
	if err := f.s.DB.WithContext(f.ctx).Create(&run).Error; err != nil {
		f.t.Fatalf("create %s review run: %v", status, err)
	}
	return run
}

func prIDs(prs []store.PullRequest) []uint {
	ids := make([]uint, 0, len(prs))
	for _, pr := range prs {
		ids = append(ids, pr.ID)
	}
	return ids
}

func hasID(ids []uint, id uint) bool {
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}

func TestPendingReviewPRs(t *testing.T) {
	f := newFixture(t)
	db := f.s.DB.WithContext(f.ctx)

	installID := f.nextID()
	f.installation(installID)
	team := f.team("qa")
	profile := f.llmProfile("pending-profile", true)
	if _, err := f.s.SaveQAPolicy(f.ctx, store.QAPolicy{TeamID: team.ID, LLMProfileID: profile.ID}); err != nil {
		t.Fatalf("SaveQAPolicy: %v", err)
	}
	goodRepo := f.repo(installID, installID, "acme/good", &team.ID, true)

	// A team without a policy, and a disabled repository, must not enqueue.
	otherTeam := f.team("nop")
	noPolicyRepo := f.repo(f.nextID(), installID, "acme/no-policy", &otherTeam.ID, true)
	disabledRepo := f.repo(f.nextID(), installID, "acme/disabled", &team.ID, false)

	eligible := f.openPR(goodRepo.ID, 1, store.StatusPending, false)
	failed := f.openPR(goodRepo.ID, 2, store.StatusPending, false)
	succeeded := f.openPR(goodRepo.ID, 3, store.StatusPending, false)
	superseded := f.openPR(goodRepo.ID, 4, store.StatusPending, false)
	draft := f.openPR(goodRepo.ID, 5, store.StatusDraft, true)
	fork := f.openPR(goodRepo.ID, 6, store.StatusPending, false)
	closed := f.openPR(goodRepo.ID, 7, store.StatusClosed, false)
	queued := f.openPR(goodRepo.ID, 8, store.StatusPending, false)
	noPolicy := f.openPR(noPolicyRepo.ID, 9, store.StatusPending, false)
	disabled := f.openPR(disabledRepo.ID, 10, store.StatusPending, false)

	f.run(failed, store.RunFailed)
	f.run(succeeded, store.RunSucceeded)
	f.run(superseded, store.RunSuperseded)
	f.run(queued, store.RunQueued)
	for _, tc := range []struct {
		label string
		id    uint
		field string
		value any
	}{
		{"mark PR #6 as fork", fork.ID, "is_fork", true},
		{"close PR #7", closed.ID, "state", "closed"},
	} {
		if err := db.Model(&store.PullRequest{}).Where("id = ?", tc.id).
			Update(tc.field, tc.value).Error; err != nil {
			t.Fatalf("%s: %v", tc.label, err)
		}
	}

	got, err := f.s.PendingReviewPRs(f.ctx, 50)
	if err != nil {
		t.Fatalf("PendingReviewPRs: %v", err)
	}
	ids := prIDs(got)
	for _, pr := range []store.PullRequest{eligible, superseded} {
		if !hasID(ids, pr.ID) {
			t.Errorf("PR #%d should be pending; got IDs %v", pr.Number, ids)
		}
	}
	for _, pr := range []store.PullRequest{failed, succeeded, queued, draft, fork, closed, noPolicy, disabled} {
		if hasID(ids, pr.ID) {
			t.Errorf("PR #%d should not be pending; got IDs %v", pr.Number, ids)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("PendingReviewPRs returned %d PRs, want 2: %v", len(ids), ids)
	}

	// Ordering is by updated_at DESC and LIMIT is applied in SQL.
	old := time.Now().UTC().Add(-time.Hour)
	recent := time.Now().UTC()
	if err := db.Exec(`UPDATE pull_requests SET updated_at = ? WHERE id = ?`, old, eligible.ID).Error; err != nil {
		t.Fatalf("age eligible PR: %v", err)
	}
	if err := db.Exec(`UPDATE pull_requests SET updated_at = ? WHERE id = ?`, recent, superseded.ID).Error; err != nil {
		t.Fatalf("refresh superseded PR: %v", err)
	}
	got, err = f.s.PendingReviewPRs(f.ctx, 50)
	if err != nil {
		t.Fatalf("PendingReviewPRs after ordering: %v", err)
	}
	ids = prIDs(got)
	if len(ids) != 2 || ids[0] != superseded.ID || ids[1] != eligible.ID {
		t.Fatalf("order = %v, want [%d %d]", ids, superseded.ID, eligible.ID)
	}
	got, err = f.s.PendingReviewPRs(f.ctx, 1)
	if err != nil {
		t.Fatalf("PendingReviewPRs(limit 1): %v", err)
	}
	if len(got) != 1 || got[0].ID != superseded.ID {
		t.Fatalf("limit 1 = %v, want only PR #%d", prIDs(got), superseded.Number)
	}
}

func TestRunLifecycle(t *testing.T) {
	f := newFixture(t)
	db := f.s.DB.WithContext(f.ctx)

	installID := f.nextID()
	f.installation(installID)
	team := f.team("qa")
	profile := f.llmProfile("run-profile", true)
	if _, err := f.s.SaveQAPolicy(f.ctx, store.QAPolicy{TeamID: team.ID, LLMProfileID: profile.ID}); err != nil {
		t.Fatalf("SaveQAPolicy: %v", err)
	}
	repo := f.repo(installID, installID, "acme/app", &team.ID, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)

	run, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, ProfileID: profile.ID, HeadSHA: pr.HeadSHA,
	})
	if err != nil {
		t.Fatalf("CreateReviewRun: %v", err)
	}
	if run.ID == 0 || run.Status != store.RunQueued || run.Attempt != 1 {
		t.Fatalf("created run = %+v, want queued attempt 1", run)
	}

	claimed, ok, err := f.s.ClaimReviewRun(f.ctx, run.ID)
	if err != nil || !ok {
		t.Fatalf("ClaimReviewRun = %+v, %v, %v; want true", claimed, ok, err)
	}
	if claimed.Status != store.RunRunning || claimed.StartedAt == nil {
		t.Errorf("claimed run = %+v, want running with StartedAt", claimed)
	}
	_, ok, err = f.s.ClaimReviewRun(f.ctx, run.ID)
	if err != nil || ok {
		t.Fatalf("second ClaimReviewRun ok = %v, err = %v; want false, nil", ok, err)
	}

	finding := store.Finding{
		ID: "finding-a", PullRequestID: pr.ID, Fingerprint: "fp-a",
		Path: "main.go", Severity: "major", Status: store.FindingOpen,
		FirstSeenRunID: run.ID, LastSeenRunID: run.ID,
	}
	resolvedFinding := store.Finding{
		ID: "finding-b", PullRequestID: pr.ID, Fingerprint: "fp-b",
		Path: "old.go", Severity: "minor", Status: store.FindingOpen,
		FirstSeenRunID: run.ID, LastSeenRunID: run.ID,
	}
	coverage := []byte(`{"reviewed_files":2,"skipped":["generated.pb.go"]}`)
	usage := []byte(`{"input_tokens":123,"output_tokens":45}`)
	maxContext := int64(1053)
	err = f.s.CompleteReviewRun(f.ctx, run.ID, store.RunCompletion{
		Status:      store.RunSucceeded,
		Summary:     "looks good",
		Coverage:    coverage,
		Usage:       usage,
		TokensKnown: true,
		Tokens: store.RunUsage{
			Input: 123, Output: 45, Reasoning: 30, CacheRead: 900, CacheWrite: 30,
			Total: 168, MaxContext: &maxContext,
		},
		Findings: []store.Finding{finding, resolvedFinding},
		Resolved: []string{"finding-b"},
	})
	if err != nil {
		t.Fatalf("CompleteReviewRun: %v", err)
	}

	// Re-read everything from the database: complete run, findings and
	// resolution must all have committed together.
	stored, err := f.s.ReviewRun(f.ctx, run.ID)
	if err != nil {
		t.Fatalf("ReviewRun after complete: %v", err)
	}
	if stored.Status != store.RunSucceeded || stored.Summary != "looks good" || stored.FinishedAt == nil {
		t.Errorf("completed run = %+v", stored)
	}
	assertJSONEqual(t, "coverage", datatypes.JSON(stored.Coverage), `{"reviewed_files":2,"skipped":["generated.pb.go"]}`)
	assertJSONEqual(t, "usage", datatypes.JSON(stored.Usage), `{"input_tokens":123,"output_tokens":45}`)
	if stored.InputTokens == nil || *stored.InputTokens != 123 ||
		stored.OutputTokens == nil || *stored.OutputTokens != 45 ||
		stored.ReasoningTokens == nil || *stored.ReasoningTokens != 30 ||
		stored.CacheReadTokens == nil || *stored.CacheReadTokens != 900 ||
		stored.CacheWriteTokens == nil || *stored.CacheWriteTokens != 30 ||
		stored.TotalTokens == nil || *stored.TotalTokens != 168 ||
		stored.MaxContextTokens == nil || *stored.MaxContextTokens != 1053 {
		t.Errorf("token columns = %v/%v/%v/%v/%v/%v/%v, want 123/45/30/900/30/168/1053",
			stored.InputTokens, stored.OutputTokens, stored.ReasoningTokens, stored.CacheReadTokens,
			stored.CacheWriteTokens, stored.TotalTokens, stored.MaxContextTokens)
	}

	findings, err := f.s.FindingsForPR(f.ctx, pr.ID)
	if err != nil || len(findings) != 2 {
		t.Fatalf("FindingsForPR = %+v, %v; want 2", findings, err)
	}
	byID := map[string]store.Finding{}
	for _, item := range findings {
		byID[item.ID] = item
	}
	if byID["finding-a"].Status != store.FindingOpen {
		t.Errorf("finding-a status = %q, want open", byID["finding-a"].Status)
	}
	if byID["finding-b"].Status != store.FindingResolved || byID["finding-b"].LastSeenRunID != run.ID {
		t.Errorf("finding-b = %+v, want resolved by run %d", byID["finding-b"], run.ID)
	}
	open, err := f.s.OpenFindingsForPR(f.ctx, pr.ID)
	if err != nil || len(open) != 1 || open[0].ID != "finding-a" {
		t.Fatalf("OpenFindingsForPR = %+v, %v; want only finding-a", open, err)
	}

	// A failure in the middle of CompleteReviewRun rolls back the findings
	// and leaves the run in its previous state.
	rollbackPR := f.openPR(repo.ID, 2, store.StatusPending, false)
	rollbackRun, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: rollbackPR.ID, HeadSHA: rollbackPR.HeadSHA,
	})
	if err != nil {
		t.Fatalf("CreateReviewRun(rollback): %v", err)
	}
	if _, ok, err := f.s.ClaimReviewRun(f.ctx, rollbackRun.ID); err != nil || !ok {
		t.Fatalf("ClaimReviewRun(rollback): ok = %v, err = %v", ok, err)
	}
	// The deliberately malformed JSON makes PostgreSQL reject the final run
	// UPDATE. Use a silent logger so the expected SQL error is not test noise.
	quiet := &store.Store{
		DB:   f.s.DB.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}),
		Pool: f.s.Pool,
	}
	err = quiet.CompleteReviewRun(f.ctx, rollbackRun.ID, store.RunCompletion{
		Status:   store.RunSucceeded,
		Coverage: []byte("{not-json"),
		Findings: []store.Finding{{
			ID: "should-rollback", PullRequestID: rollbackPR.ID, Fingerprint: "fp-rollback",
			FirstSeenRunID: rollbackRun.ID, LastSeenRunID: rollbackRun.ID,
		}},
	})
	if err == nil {
		t.Fatal("CompleteReviewRun with invalid coverage: got nil error")
	}
	afterRollback, err := f.s.ReviewRun(f.ctx, rollbackRun.ID)
	if err != nil {
		t.Fatalf("ReviewRun after rollback: %v", err)
	}
	if afterRollback.Status != store.RunRunning || afterRollback.FinishedAt != nil {
		t.Errorf("run after rollback = %+v, want still running", afterRollback)
	}
	requireCount(t, db.Table("findings").Where("pull_request_id = ?", rollbackPR.ID), "findings", 0)
}

type reviewRunTokenColumns struct {
	InputTokens      *int64 `gorm:"column:input_tokens"`
	OutputTokens     *int64 `gorm:"column:output_tokens"`
	ReasoningTokens  *int64 `gorm:"column:reasoning_tokens"`
	CacheReadTokens  *int64 `gorm:"column:cache_read_tokens"`
	CacheWriteTokens *int64 `gorm:"column:cache_write_tokens"`
	TotalTokens      *int64 `gorm:"column:total_tokens"`
	MaxContextTokens *int64 `gorm:"column:max_context_tokens"`
}

// TestCompleteReviewRunStoresNullTokensWithoutUsage pins the #129 rule: a run
// with no engine result stores NULL, while a measured zero is a real 0.
func TestCompleteReviewRunStoresNullTokensWithoutUsage(t *testing.T) {
	f := newFixture(t)
	installID := f.nextID()
	f.installation(installID)
	repo := f.repo(installID, installID, "acme/app", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)

	tokenColumns := func(id uint) reviewRunTokenColumns {
		t.Helper()
		var cols reviewRunTokenColumns
		if err := f.s.DB.Raw(`SELECT input_tokens, output_tokens, reasoning_tokens,
			cache_read_tokens, cache_write_tokens, total_tokens, max_context_tokens
			FROM review_runs WHERE id = ?`, id).Scan(&cols).Error; err != nil {
			t.Fatalf("read token columns: %v", err)
		}
		return cols
	}
	runningRun := func() store.ReviewRun {
		t.Helper()
		run, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
			PullRequestID: pr.ID, HeadSHA: pr.HeadSHA,
		})
		if err != nil {
			t.Fatalf("CreateReviewRun: %v", err)
		}
		if _, ok, err := f.s.ClaimReviewRun(f.ctx, run.ID); err != nil || !ok {
			t.Fatalf("ClaimReviewRun: ok = %v, err = %v", ok, err)
		}
		return run
	}

	unknown := runningRun()
	if err := f.s.CompleteReviewRun(f.ctx, unknown.ID, store.RunCompletion{
		Status: store.RunFailed, ErrorCode: "timeout",
	}); err != nil {
		t.Fatalf("CompleteReviewRun(unknown): %v", err)
	}
	cols := tokenColumns(unknown.ID)
	if cols.InputTokens != nil || cols.OutputTokens != nil || cols.ReasoningTokens != nil ||
		cols.CacheReadTokens != nil || cols.CacheWriteTokens != nil ||
		cols.TotalTokens != nil || cols.MaxContextTokens != nil {
		t.Errorf("token columns = %+v, want NULL when no engine reported usage", cols)
	}

	measured := runningRun()
	if err := f.s.CompleteReviewRun(f.ctx, measured.ID, store.RunCompletion{
		Status: store.RunSucceeded, TokensKnown: true,
	}); err != nil {
		t.Fatalf("CompleteReviewRun(measured zero): %v", err)
	}
	cols = tokenColumns(measured.ID)
	if cols.InputTokens == nil || *cols.InputTokens != 0 ||
		cols.TotalTokens == nil || *cols.TotalTokens != 0 {
		t.Errorf("token columns = %+v, want measured zeros stored as 0", cols)
	}
	if cols.MaxContextTokens != nil {
		t.Errorf("max_context_tokens = %v, want NULL when no sample was reported", *cols.MaxContextTokens)
	}
}

func TestCompleteReviewRunRequiresRunningAndClearsToken(t *testing.T) {
	f := newFixture(t)

	installID := f.nextID()
	f.installation(installID)
	repo := f.repo(installID, installID, "acme/app", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)

	run, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, CloneTokenEnc: "sealed-token",
	})
	if err != nil {
		t.Fatalf("CreateReviewRun: %v", err)
	}
	if _, ok, err := f.s.ClaimReviewRun(f.ctx, run.ID); err != nil || !ok {
		t.Fatalf("ClaimReviewRun: ok = %v, err = %v", ok, err)
	}
	finding := store.Finding{
		ID: "finding-clear", PullRequestID: pr.ID, Fingerprint: "fp-clear",
		Path: "main.go", Severity: "major", Status: store.FindingOpen,
		FirstSeenRunID: run.ID, LastSeenRunID: run.ID,
	}
	if err := f.s.CompleteReviewRun(f.ctx, run.ID, store.RunCompletion{
		Status:   store.RunSucceeded,
		Summary:  "ok",
		Findings: []store.Finding{finding},
	}); err != nil {
		t.Fatalf("CompleteReviewRun: %v", err)
	}
	stored, err := f.s.ReviewRun(f.ctx, run.ID)
	if err != nil {
		t.Fatalf("ReviewRun: %v", err)
	}
	if stored.CloneTokenEnc != "" {
		t.Errorf("clone token after completion = %q, want empty", stored.CloneTokenEnc)
	}

	// A run superseded while executing cannot be completed: no findings and no
	// resolutions may be persisted for it.
	supersededPR := f.openPR(repo.ID, 2, store.StatusPending, false)
	supersededRun, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: supersededPR.ID, HeadSHA: supersededPR.HeadSHA, CloneTokenEnc: "sealed-token",
	})
	if err != nil {
		t.Fatalf("CreateReviewRun(superseded): %v", err)
	}
	if _, ok, err := f.s.ClaimReviewRun(f.ctx, supersededRun.ID); err != nil || !ok {
		t.Fatalf("ClaimReviewRun(superseded): ok = %v, err = %v", ok, err)
	}
	if err := f.s.SupersedeActiveRuns(f.ctx, supersededPR.ID); err != nil {
		t.Fatalf("SupersedeActiveRuns: %v", err)
	}
	stale := store.Finding{
		ID: "finding-stale", PullRequestID: supersededPR.ID, Fingerprint: "fp-stale",
		Path: "main.go", Severity: "major", Status: store.FindingOpen,
		FirstSeenRunID: supersededRun.ID, LastSeenRunID: supersededRun.ID,
	}
	err = f.s.CompleteReviewRun(f.ctx, supersededRun.ID, store.RunCompletion{
		Status:   store.RunSucceeded,
		Summary:  "stale",
		Findings: []store.Finding{stale},
		Resolved: []string{"finding-stale"},
	})
	if !errors.Is(err, store.ErrRunNotActive) {
		t.Fatalf("CompleteReviewRun(superseded) error = %v, want ErrRunNotActive", err)
	}
	stored, err = f.s.ReviewRun(f.ctx, supersededRun.ID)
	if err != nil {
		t.Fatalf("ReviewRun(superseded): %v", err)
	}
	if stored.Status != store.RunSuperseded {
		t.Errorf("superseded run status = %q, want superseded", stored.Status)
	}
	if stored.CloneTokenEnc != "" {
		t.Errorf("superseded run kept clone token %q", stored.CloneTokenEnc)
	}
	requireCount(t, f.s.DB.Where("pull_request_id = ?", supersededPR.ID), "findings", 0)
}

func TestRunLifecycleLookups(t *testing.T) {
	f := newFixture(t)

	installID := f.nextID()
	f.installation(installID)
	team := f.team("qa")
	profile := f.llmProfile("lookup-profile", true)
	repo := f.repo(installID, installID, "acme/app", &team.ID, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)

	first, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, Status: store.RunSuperseded,
	})
	if err != nil {
		t.Fatalf("CreateReviewRun(first): %v", err)
	}
	second, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, ProfileID: profile.ID, Status: store.RunSucceeded,
	})
	if err != nil {
		t.Fatalf("CreateReviewRun(second): %v", err)
	}

	loaded, err := f.s.ReviewRun(f.ctx, second.ID)
	if err != nil || loaded.ID != second.ID || loaded.ProfileID != profile.ID {
		t.Fatalf("ReviewRun(%d) = %+v, %v", second.ID, loaded, err)
	}
	requireNotFound(t, func() error {
		_, err := f.s.ReviewRun(f.ctx, 999999)
		return err
	}())

	bySHA, err := f.s.RunForSHA(f.ctx, pr.ID, pr.HeadSHA)
	if err != nil || bySHA.ID != second.ID {
		t.Fatalf("RunForSHA = %+v, %v; want run %d", bySHA, err, second.ID)
	}
	requireNotFound(t, func() error {
		_, err := f.s.RunForSHA(f.ctx, pr.ID, "other-sha")
		return err
	}())

	runs, err := f.s.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 2 || runs[0].ID != first.ID || runs[1].ID != second.ID {
		t.Fatalf("ListReviewRuns = %v, want [%d %d]", prIDsOfRuns(runs), first.ID, second.ID)
	}
}

func TestCompleteReviewRunPersistsBaseSHA(t *testing.T) {
	f := newFixture(t)

	installID := f.nextID()
	f.installation(installID)
	repo := f.repo(installID, installID, "acme/app", nil, true)

	pr := f.openPR(repo.ID, 1, store.StatusPending, false)
	run, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA,
	})
	if err != nil {
		t.Fatalf("CreateReviewRun: %v", err)
	}
	if _, ok, err := f.s.ClaimReviewRun(f.ctx, run.ID); err != nil || !ok {
		t.Fatalf("ClaimReviewRun: ok = %v, err = %v", ok, err)
	}
	if err := f.s.CompleteReviewRun(f.ctx, run.ID, store.RunCompletion{
		Status: store.RunSucceeded, BaseSHA: "base-1",
	}); err != nil {
		t.Fatalf("CompleteReviewRun: %v", err)
	}
	stored, err := f.s.ReviewRun(f.ctx, run.ID)
	if err != nil {
		t.Fatalf("ReviewRun: %v", err)
	}
	if stored.BaseSHA != "base-1" {
		t.Errorf("base_sha = %q, want %q", stored.BaseSHA, "base-1")
	}

	// An empty BaseSHA must never clear a value the run already carried.
	keepPR := f.openPR(repo.ID, 2, store.StatusPending, false)
	keep, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: keepPR.ID, HeadSHA: keepPR.HeadSHA, BaseSHA: "keep-me",
	})
	if err != nil {
		t.Fatalf("CreateReviewRun(keep): %v", err)
	}
	if _, ok, err := f.s.ClaimReviewRun(f.ctx, keep.ID); err != nil || !ok {
		t.Fatalf("ClaimReviewRun(keep): ok = %v, err = %v", ok, err)
	}
	if err := f.s.CompleteReviewRun(f.ctx, keep.ID, store.RunCompletion{
		Status: store.RunSucceeded,
	}); err != nil {
		t.Fatalf("CompleteReviewRun(keep): %v", err)
	}
	stored, err = f.s.ReviewRun(f.ctx, keep.ID)
	if err != nil {
		t.Fatalf("ReviewRun(keep): %v", err)
	}
	if stored.BaseSHA != "keep-me" {
		t.Errorf("base_sha = %q, want the stored value %q", stored.BaseSHA, "keep-me")
	}
}

func TestPreviousSucceededRun(t *testing.T) {
	f := newFixture(t)

	installID := f.nextID()
	f.installation(installID)
	repo := f.repo(installID, installID, "acme/app", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)
	other := f.openPR(repo.ID, 2, store.StatusPending, false)

	requireNotFound(t, func() error {
		_, err := f.s.PreviousSucceededRun(f.ctx, pr.ID, 1_000_000)
		return err
	}())

	first := f.run(pr, store.RunSucceeded)
	f.run(pr, store.RunFailed)
	latest := f.run(pr, store.RunSucceeded)
	// A succeeded run of another PR must never be returned.
	f.run(other, store.RunSucceeded)

	got, err := f.s.PreviousSucceededRun(f.ctx, pr.ID, 1_000_000)
	if err != nil || got.ID != latest.ID {
		t.Fatalf("PreviousSucceededRun = %+v, %v; want run %d", got, err, latest.ID)
	}
	got, err = f.s.PreviousSucceededRun(f.ctx, pr.ID, latest.ID)
	if err != nil || got.ID != first.ID {
		t.Fatalf("PreviousSucceededRun(before latest) = %+v, %v; want run %d", got, err, first.ID)
	}
	requireNotFound(t, func() error {
		_, err := f.s.PreviousSucceededRun(f.ctx, pr.ID, first.ID)
		return err
	}())
}

func prIDsOfRuns(runs []store.ReviewRun) []uint {
	ids := make([]uint, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, run.ID)
	}
	return ids
}

func TestInterruptStaleRuns(t *testing.T) {
	f := newFixture(t)

	installID := f.nextID()
	f.installation(installID)
	repo := f.repo(installID, installID, "acme/app", nil, true)

	oldStarted := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Microsecond)
	recentStarted := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)

	stalePR := f.openPR(repo.ID, 1, store.StatusReviewing, false)
	stale, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: stalePR.ID, HeadSHA: stalePR.HeadSHA, Status: store.RunRunning,
		StartedAt: &oldStarted, CloneTokenEnc: "sealed-token",
	})
	if err != nil {
		t.Fatalf("create stale running run: %v", err)
	}
	recentPR := f.openPR(repo.ID, 2, store.StatusReviewing, false)
	recent := f.run(recentPR, store.RunRunning)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.ReviewRun{}).
		Where("id = ?", recent.ID).Update("started_at", recentStarted).Error; err != nil {
		t.Fatalf("set recent started_at: %v", err)
	}
	queuedPR := f.openPR(repo.ID, 3, store.StatusPending, false)
	queued := f.run(queuedPR, store.RunQueued)

	// A run that overran its budget but still beats: its worker is alive, so
	// the watchdog must report a timeout instead of a vanished worker.
	overrunPR := f.openPR(repo.ID, 4, store.StatusReviewing, false)
	overrun, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: overrunPR.ID, HeadSHA: overrunPR.HeadSHA, Status: store.RunRunning,
		StartedAt: &oldStarted, CloneTokenEnc: "sealed-token",
	})
	if err != nil {
		t.Fatalf("create overrun running run: %v", err)
	}
	if err := f.s.DB.WithContext(f.ctx).Model(&store.ReviewRun{}).
		Where("id = ?", overrun.ID).
		Update("heartbeat_at", time.Now().UTC()).Error; err != nil {
		t.Fatalf("set overrun heartbeat: %v", err)
	}

	now := time.Now().UTC()
	policy := store.StaleRunPolicy{
		StartedBefore:     now.Add(-time.Hour),
		HeartbeatBefore:   now.Add(-3 * time.Minute),
		OverranMessage:    "la revisión superó el tiempo máximo de 60 min sin terminar",
		LostWorkerMessage: "el worker dejó de responder hace más de 3 min (posible reinicio o caída)",
	}
	runs, err := f.s.InterruptStaleRuns(f.ctx, policy)
	if err != nil {
		t.Fatalf("InterruptStaleRuns: %v", err)
	}
	byID := map[uint]store.ReviewRun{}
	for _, run := range runs {
		byID[run.ID] = run
	}
	if len(byID) != 2 {
		t.Fatalf("interrupted runs = %v, want %d and %d", prIDsOfRuns(runs), stale.ID, overrun.ID)
	}

	for _, run := range []store.ReviewRun{stale, overrun} {
		got, err := f.s.ReviewRun(f.ctx, run.ID)
		if err != nil {
			t.Fatalf("ReviewRun(%d): %v", run.ID, err)
		}
		if got.Status != store.RunFailed || got.ErrorCode != store.RunErrorInterrupted {
			t.Errorf("run %d = status %q code %q, want failed/%s",
				run.ID, got.Status, got.ErrorCode, store.RunErrorInterrupted)
		}
		if got.FinishedAt == nil {
			t.Errorf("run %d finished_at is nil", run.ID)
		}
		if got.StartedAt == nil || !got.StartedAt.Equal(oldStarted) {
			t.Errorf("run %d started_at = %v, want the original %v", run.ID, got.StartedAt, oldStarted)
		}
		if got.CloneTokenEnc != "" {
			t.Errorf("run %d clone token = %q, want it cleared", run.ID, got.CloneTokenEnc)
		}
	}

	if msg := byID[stale.ID].ErrorMessage; !strings.Contains(msg, "dejó de responder") {
		t.Errorf("stale run message = %q, want the lost-worker wording", msg)
	}
	if msg := byID[overrun.ID].ErrorMessage; !strings.Contains(msg, "tiempo máximo") {
		t.Errorf("overrun run message = %q, want the overrun wording", msg)
	}

	untouchedRecent, err := f.s.ReviewRun(f.ctx, recent.ID)
	if err != nil {
		t.Fatalf("ReviewRun(recent): %v", err)
	}
	if untouchedRecent.Status != store.RunRunning || untouchedRecent.FinishedAt != nil {
		t.Errorf("recent run = %+v, want still running", untouchedRecent)
	}
	untouchedQueued, err := f.s.ReviewRun(f.ctx, queued.ID)
	if err != nil {
		t.Fatalf("ReviewRun(queued): %v", err)
	}
	if untouchedQueued.Status != store.RunQueued || untouchedQueued.FinishedAt != nil {
		t.Errorf("queued run = %+v, want still queued", untouchedQueued)
	}

	// A second sweep has nothing left to interrupt.
	again, err := f.s.InterruptStaleRuns(f.ctx, policy)
	if err != nil {
		t.Fatalf("second InterruptStaleRuns: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second sweep interrupted %v, want none", prIDsOfRuns(again))
	}
}

func TestInterruptStaleRunsConcurrent(t *testing.T) {
	f := newFixture(t)

	installID := f.nextID()
	f.installation(installID)
	repo := f.repo(installID, installID, "acme/app", nil, true)

	started := time.Now().UTC().Add(-3 * time.Hour)
	pr := f.openPR(repo.ID, 1, store.StatusReviewing, false)
	run, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, Status: store.RunRunning,
		StartedAt: &started,
	})
	if err != nil {
		t.Fatalf("create stale running run: %v", err)
	}

	now := time.Now().UTC()
	policy := store.StaleRunPolicy{
		StartedBefore:     now.Add(-time.Hour),
		HeartbeatBefore:   now.Add(-3 * time.Minute),
		OverranMessage:    "la revisión superó el tiempo máximo de 60 min sin terminar",
		LostWorkerMessage: "el worker dejó de responder hace más de 3 min (posible reinicio o caída)",
	}
	type result struct {
		runs []store.ReviewRun
		err  error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			runs, err := f.s.InterruptStaleRuns(context.Background(), policy)
			results <- result{runs: runs, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	interrupted := 0
	for res := range results {
		if res.err != nil {
			t.Fatalf("InterruptStaleRuns() error = %v", res.err)
		}
		interrupted += len(res.runs)
	}
	if interrupted != 1 {
		t.Errorf("interrupted rows across both callers = %d, want 1", interrupted)
	}

	got, err := f.s.ReviewRun(f.ctx, run.ID)
	if err != nil {
		t.Fatalf("ReviewRun: %v", err)
	}
	if got.Status != store.RunFailed || got.ErrorCode != store.RunErrorInterrupted {
		t.Errorf("run after concurrent sweep = %+v, want failed/%s", got, store.RunErrorInterrupted)
	}
	if got.FinishedAt == nil {
		t.Error("finished_at is nil after concurrent sweep")
	}
	// The run never beat, so exactly one of the sweeps must have left the
	// lost-worker message, not the overrun one.
	if !strings.Contains(got.ErrorMessage, "dejó de responder") {
		t.Errorf("error message = %q, want the lost-worker wording", got.ErrorMessage)
	}
}

func TestReviewRunHeartbeat(t *testing.T) {
	f := newFixture(t)
	installID := f.nextID()
	f.installation(installID)
	repo := f.repo(installID, installID, "acme/app", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)
	run, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA,
	})
	if err != nil {
		t.Fatalf("CreateReviewRun: %v", err)
	}

	claimed, ok, err := f.s.ClaimReviewRun(f.ctx, run.ID)
	if err != nil || !ok {
		t.Fatalf("ClaimReviewRun: ok = %v, err = %v", ok, err)
	}
	if claimed.HeartbeatAt == nil {
		t.Fatal("claimed run has no heartbeat_at")
	}

	old := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.ReviewRun{}).
		Where("id = ?", run.ID).Update("heartbeat_at", old).Error; err != nil {
		t.Fatalf("age heartbeat: %v", err)
	}
	if err := f.s.HeartbeatReviewRun(f.ctx, run.ID); err != nil {
		t.Fatalf("HeartbeatReviewRun: %v", err)
	}
	stored, err := f.s.ReviewRun(f.ctx, run.ID)
	if err != nil {
		t.Fatalf("ReviewRun: %v", err)
	}
	if stored.HeartbeatAt == nil || !stored.HeartbeatAt.After(old) {
		t.Errorf("heartbeat_at = %v, want a time after %v", stored.HeartbeatAt, old)
	}

	if err := f.s.CompleteReviewRun(f.ctx, run.ID, store.RunCompletion{
		Status: store.RunFailed, ErrorCode: "timeout",
	}); err != nil {
		t.Fatalf("CompleteReviewRun: %v", err)
	}
	if err := f.s.HeartbeatReviewRun(f.ctx, run.ID); !errors.Is(err, store.ErrRunNotActive) {
		t.Fatalf("HeartbeatReviewRun on a finished run = %v, want ErrRunNotActive", err)
	}
}

func TestRunLifecycleSupersede(t *testing.T) {
	f := newFixture(t)

	installID := f.nextID()
	f.installation(installID)
	team := f.team("qa")
	repo := f.repo(installID, installID, "acme/app", &team.ID, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)
	otherPR := f.openPR(repo.ID, 2, store.StatusPending, false)

	run := f.run(pr, store.RunQueued)
	other := f.run(otherPR, store.RunRunning)
	if err := f.s.SupersedeActiveRuns(f.ctx, pr.ID); err != nil {
		t.Fatalf("SupersedeActiveRuns: %v", err)
	}
	got, err := f.s.ReviewRun(f.ctx, run.ID)
	if err != nil || got.Status != store.RunSuperseded {
		t.Fatalf("run after supersede = %+v, %v", got, err)
	}
	untouched, err := f.s.ReviewRun(f.ctx, other.ID)
	if err != nil || untouched.Status != store.RunRunning {
		t.Fatalf("other PR's run = %+v, %v; want unchanged", untouched, err)
	}

	// The partial unique index no longer blocks a new queued run.
	if _, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: "new-sha",
	}); err != nil {
		t.Fatalf("CreateReviewRun after supersede: %v", err)
	}
}

func TestRunLifecycleSetPRStatus(t *testing.T) {
	f := newFixture(t)

	installID := f.nextID()
	f.installation(installID)
	repo := f.repo(installID, installID, "acme/app", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)

	if err := f.s.SetPRStatus(f.ctx, pr.ID, store.StatusReadyForHuman); err != nil {
		t.Fatalf("SetPRStatus: %v", err)
	}
	got := f.pullRequest(t, pr.ID)
	if got.QAStatus != store.StatusReadyForHuman {
		t.Errorf("qa_status = %q, want %q", got.QAStatus, store.StatusReadyForHuman)
	}
	if got.State != pr.State || got.HeadSHA != pr.HeadSHA || got.Draft != pr.Draft {
		t.Errorf("SetPRStatus changed unrelated fields: %+v", got)
	}
	requireNotFound(t, func() error { return f.s.SetPRStatus(f.ctx, 999999, store.StatusPending) }())
}

func TestFindingsUpsertAndResolve(t *testing.T) {
	f := newFixture(t)
	db := f.s.DB.WithContext(f.ctx)

	installID := f.nextID()
	f.installation(installID)
	repo := f.repo(installID, installID, "acme/app", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)

	first, err := f.s.UpsertFinding(f.ctx, store.Finding{
		ID: "finding-1", PullRequestID: pr.ID, Fingerprint: "fp",
		Path: "main.go", Severity: "major", Status: store.FindingOpen,
		FirstSeenRunID: 10, LastSeenRunID: 10,
	})
	if err != nil {
		t.Fatalf("UpsertFinding(first): %v", err)
	}
	if first.ID != "finding-1" {
		t.Fatalf("UpsertFinding id = %q", first.ID)
	}

	// Same (PR, fingerprint): one row, original ID and first_seen_run_id kept.
	second, err := f.s.UpsertFinding(f.ctx, store.Finding{
		ID: "finding-2", PullRequestID: pr.ID, Fingerprint: "fp",
		Path: "main.go", Severity: "critical", Body: "updated body",
		Status: store.FindingOpen, FirstSeenRunID: 20, LastSeenRunID: 20,
	})
	if err != nil {
		t.Fatalf("UpsertFinding(conflict): %v", err)
	}
	if second.ID != first.ID || second.FirstSeenRunID != 10 {
		t.Errorf("upsert replaced identity: %+v, want id %q and first_seen 10", second, first.ID)
	}
	if second.Severity != "critical" || second.Body != "updated body" || second.LastSeenRunID != 20 {
		t.Errorf("upsert did not refresh finding: %+v", second)
	}
	requireCount(t, db.Model(&store.Finding{}).Where("pull_request_id = ?", pr.ID), "findings", 1)

	if err := f.s.SetFindingPublished(f.ctx, first.ID, 4242, "node-1", "thread-1"); err != nil {
		t.Fatalf("SetFindingPublished: %v", err)
	}
	published, err := f.s.FindingsForPR(f.ctx, pr.ID)
	if err != nil || len(published) != 1 {
		t.Fatalf("FindingsForPR = %+v, %v", published, err)
	}
	if published[0].GithubCommentID != 4242 || published[0].GithubCommentNodeID != "node-1" ||
		published[0].GithubThreadID != "thread-1" {
		t.Errorf("published coordinates not stored: %+v", published[0])
	}

	// Re-upserting must not wipe the GitHub publication coordinates.
	if _, err := f.s.UpsertFinding(f.ctx, store.Finding{
		ID: "finding-2", PullRequestID: pr.ID, Fingerprint: "fp",
		Path: "main.go", Severity: "major", Status: store.FindingOpen,
		FirstSeenRunID: 30, LastSeenRunID: 30,
	}); err != nil {
		t.Fatalf("UpsertFinding(third): %v", err)
	}
	after, err := f.s.FindingsForPR(f.ctx, pr.ID)
	if err != nil || len(after) != 1 {
		t.Fatalf("FindingsForPR after re-upsert = %+v, %v", after, err)
	}
	if after[0].GithubCommentID != 4242 || after[0].GithubCommentNodeID != "node-1" ||
		after[0].GithubThreadID != "thread-1" || after[0].FirstSeenRunID != 10 {
		t.Errorf("re-upsert lost state: %+v", after[0])
	}

	if err := f.s.UpdateFindingStatus(f.ctx, first.ID, store.FindingResolved, 31); err != nil {
		t.Fatalf("UpdateFindingStatus: %v", err)
	}
	open, err := f.s.OpenFindingsForPR(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("OpenFindingsForPR: %v", err)
	}
	if len(open) != 0 {
		t.Errorf("OpenFindingsForPR = %+v, want none", open)
	}
	resolved, err := f.s.FindingsForPR(f.ctx, pr.ID)
	if err != nil || len(resolved) != 1 {
		t.Fatalf("FindingsForPR after resolve = %+v, %v", resolved, err)
	}
	if resolved[0].Status != store.FindingResolved || resolved[0].LastSeenRunID != 31 {
		t.Errorf("resolved finding = %+v", resolved[0])
	}

	if _, err := f.s.UpsertFinding(f.ctx, store.Finding{
		ID: "finding-3", PullRequestID: pr.ID, Fingerprint: "fp-2", Status: store.FindingOpen,
	}); err != nil {
		t.Fatalf("UpsertFinding(fp-2): %v", err)
	}
	open, err = f.s.OpenFindingsForPR(f.ctx, pr.ID)
	if err != nil || len(open) != 1 || open[0].ID != "finding-3" {
		t.Fatalf("OpenFindingsForPR = %+v, %v; want finding-3", open, err)
	}
	requireNotFound(t, func() error { return f.s.UpdateFindingStatus(f.ctx, "missing", store.FindingResolved, 1) }())
	requireNotFound(t, func() error { return f.s.SetFindingPublished(f.ctx, "missing", 1, "", "") }())
}

func TestArtifactIdempotency(t *testing.T) {
	f := newFixture(t)
	db := f.s.DB.WithContext(f.ctx)

	installID := f.nextID()
	f.installation(installID)
	repo := f.repo(installID, installID, "acme/app", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)

	summary, err := f.s.UpsertArtifact(f.ctx, store.GitHubArtifact{
		PullRequestID: pr.ID, Kind: store.ArtifactSummary, HeadSHA: "sha-a",
		ExternalID: "1", URL: "https://github.com/comment/1",
	})
	if err != nil {
		t.Fatalf("UpsertArtifact(summary): %v", err)
	}
	if summary.ID == 0 {
		t.Fatal("UpsertArtifact(summary) returned ID 0")
	}

	// One summary per PR: a new SHA updates the same row.
	summaryAgain, err := f.s.UpsertArtifact(f.ctx, store.GitHubArtifact{
		PullRequestID: pr.ID, Kind: store.ArtifactSummary, HeadSHA: "sha-b",
		ExternalID: "2", URL: "https://github.com/comment/2",
	})
	if err != nil {
		t.Fatalf("UpsertArtifact(summary again): %v", err)
	}
	if summaryAgain.ID != summary.ID || summaryAgain.ExternalID != "2" {
		t.Errorf("summary upsert = %+v, want ID %d with external_id 2", summaryAgain, summary.ID)
	}
	requireCount(t, db.Model(&store.GitHubArtifact{}).Where("pull_request_id = ?", pr.ID), "github_artifacts", 1)

	// Non-summary kinds are keyed by (PR, kind, head_sha).
	review, err := f.s.UpsertArtifact(f.ctx, store.GitHubArtifact{
		PullRequestID: pr.ID, Kind: store.ArtifactReview, HeadSHA: "sha-a", ExternalID: "3",
	})
	if err != nil {
		t.Fatalf("UpsertArtifact(review): %v", err)
	}
	reviewAgain, err := f.s.UpsertArtifact(f.ctx, store.GitHubArtifact{
		PullRequestID: pr.ID, Kind: store.ArtifactReview, HeadSHA: "sha-a", ExternalID: "4",
	})
	if err != nil {
		t.Fatalf("UpsertArtifact(review again): %v", err)
	}
	if reviewAgain.ID != review.ID || reviewAgain.ExternalID != "4" {
		t.Errorf("review upsert = %+v, want ID %d with external_id 4", reviewAgain, review.ID)
	}
	requireCount(t, db.Model(&store.GitHubArtifact{}).Where("pull_request_id = ?", pr.ID), "github_artifacts", 2)

	// A different kind for the same SHA is a separate artifact.
	if _, err := f.s.UpsertArtifact(f.ctx, store.GitHubArtifact{
		PullRequestID: pr.ID, Kind: store.ArtifactCheck, HeadSHA: "sha-a", ExternalID: "5",
	}); err != nil {
		t.Fatalf("UpsertArtifact(check): %v", err)
	}

	// Summary lookup ignores the SHA: there is only one per pull request.
	gotSummary, err := f.s.ArtifactFor(f.ctx, pr.ID, store.ArtifactSummary, "any-sha")
	if err != nil || gotSummary.ID != summary.ID || gotSummary.ExternalID != "2" {
		t.Fatalf("ArtifactFor(summary) = %+v, %v", gotSummary, err)
	}
	gotReview, err := f.s.ArtifactFor(f.ctx, pr.ID, store.ArtifactReview, "sha-a")
	if err != nil || gotReview.ID != review.ID || gotReview.ExternalID != "4" {
		t.Fatalf("ArtifactFor(review) = %+v, %v", gotReview, err)
	}
	requireNotFound(t, func() error {
		_, err := f.s.ArtifactFor(f.ctx, pr.ID, store.ArtifactReview, "sha-b")
		return err
	}())
}
