package store_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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

func TestLockProfileSerializes(t *testing.T) {
	f := newFixture(t)

	profile, err := f.s.CreateLLMProfile(f.ctx, store.LLMProfile{
		Name: "locked", APIKeyEnc: "sealed", Concurrency: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateLLMProfile: %v", err)
	}

	firstHeld := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstUnlocked := make(chan struct{})
	secondAcquired := make(chan struct{})
	releaseSecond := make(chan struct{})
	secondDone := make(chan struct{})
	errCh := make(chan error, 2)

	go func() {
		unlock, err := f.s.LockProfile(f.ctx, profile.ID)
		if err != nil {
			errCh <- fmt.Errorf("first LockProfile: %w", err)
			close(firstHeld)
			return
		}
		close(firstHeld)
		<-releaseFirst
		unlock()
		// unlock must be idempotent: a second call must not release the
		// session lock now held by another caller.
		unlock()
		close(firstUnlocked)
	}()

	select {
	case <-firstHeld:
	case err := <-errCh:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("first LockProfile did not acquire")
	}

	go func() {
		defer close(secondDone)
		unlock, err := f.s.LockProfile(f.ctx, profile.ID)
		if err != nil {
			errCh <- fmt.Errorf("second LockProfile: %w", err)
			return
		}
		close(secondAcquired)
		<-releaseSecond
		unlock()
	}()

	select {
	case <-secondAcquired:
		t.Fatal("second LockProfile acquired while the first caller held the lock")
	case err := <-errCh:
		t.Fatal(err)
	case <-time.After(150 * time.Millisecond):
		// Expected: the second caller is blocked on the advisory lock.
	}

	close(releaseFirst)
	select {
	case <-firstUnlocked:
	case err := <-errCh:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("first unlock did not finish")
	}

	select {
	case <-secondAcquired:
	case err := <-errCh:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("second LockProfile did not acquire after unlock")
	}

	close(releaseSecond)
	select {
	case <-secondDone:
	case err := <-errCh:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("second lock did not finish")
	}
}
