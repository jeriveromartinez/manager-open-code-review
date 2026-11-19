package store_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

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
