package review

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	"github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

type triggerTestJobs struct {
	reviews   []uint
	publishes []uint
	health    []uint
}

func (j *triggerTestJobs) EnqueueReview(_ context.Context, runID uint) error {
	j.reviews = append(j.reviews, runID)
	return nil
}

func (j *triggerTestJobs) EnqueuePublish(_ context.Context, runID uint) error {
	j.publishes = append(j.publishes, runID)
	return nil
}

func (j *triggerTestJobs) EnqueueLLMHealth(_ context.Context, profileID uint) error {
	j.health = append(j.health, profileID)
	return nil
}

type triggerTestChecks struct {
	started   []uint
	cancelled []uint
}

func (c *triggerTestChecks) StartCheck(_ context.Context, runID uint) error {
	c.started = append(c.started, runID)
	return nil
}

func (c *triggerTestChecks) CancelCheck(_ context.Context, runID uint) error {
	c.cancelled = append(c.cancelled, runID)
	return nil
}

type triggerTestApps struct {
	app *github.App
}

func (p triggerTestApps) App(context.Context) (*github.App, error) {
	return p.app, nil
}

func triggerTestPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

type triggerFixture struct {
	t       *testing.T
	ctx     context.Context
	st      *store.Store
	jobs    *triggerTestJobs
	checks  *triggerTestChecks
	trigger *Trigger
	seq     int64
}

type seedOpts struct {
	draft          bool
	fork           bool
	repoEnabled    bool
	withPolicy     bool
	profileEnabled bool
	headSHA        string
}

func newTriggerFixture(t *testing.T) *triggerFixture {
	t.Helper()

	st := testutil.NewStore(t)
	box, err := crypto.NewBox(bytes.Repeat([]byte{7}, crypto.KeySize))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost ||
			!strings.HasPrefix(r.URL.Path, "/app/installations/") ||
			!strings.HasSuffix(r.URL.Path, "/access_tokens") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"token":"read-token","expires_at":"2026-11-19T12:00:00Z"}`)
	}))
	t.Cleanup(srv.Close)

	app := &github.App{
		Creds:  github.AppCredentials{AppID: 12345, PEM: triggerTestPEM(t)},
		APIURL: srv.URL,
	}
	jobs := &triggerTestJobs{}
	checks := &triggerTestChecks{}
	return &triggerFixture{
		t:      t,
		ctx:    context.Background(),
		st:     st,
		jobs:   jobs,
		checks: checks,
		trigger: &Trigger{
			Store:  st,
			Apps:   triggerTestApps{app: app},
			Jobs:   jobs,
			Checks: checks,
			Box:    box,
		},
	}
}

func (f *triggerFixture) seed(name string, opts seedOpts) store.PullRequest {
	f.t.Helper()
	f.seq++

	installationID := int64(42 + f.seq)
	if err := f.st.UpsertInstallation(f.ctx, store.Installation{
		ID: installationID, AccountLogin: "acme", AccountType: "Organization",
	}); err != nil {
		f.t.Fatalf("UpsertInstallation(%d): %v", installationID, err)
	}

	team := store.Team{Name: "team-" + name, Slug: "team-" + name}
	if err := f.st.DB.WithContext(f.ctx).Create(&team).Error; err != nil {
		f.t.Fatalf("create team %q: %v", name, err)
	}
	profile := store.LLMProfile{
		Name: "profile-" + name, Provider: "anthropic", Model: "claude-test",
		Mode: "api_key", APIKeyEnc: "sealed", Concurrency: 1,
		Enabled: opts.profileEnabled, HealthStatus: store.HealthUnknown,
	}
	if err := f.st.DB.WithContext(f.ctx).Create(&profile).Error; err != nil {
		f.t.Fatalf("create profile %q: %v", name, err)
	}
	if opts.withPolicy {
		if _, err := f.st.SaveQAPolicy(f.ctx, store.QAPolicy{
			TeamID: team.ID, LLMProfileID: profile.ID,
		}); err != nil {
			f.t.Fatalf("SaveQAPolicy(%q): %v", name, err)
		}
	}

	repoID := installationID + 1000
	teamID := team.ID
	repo := store.Repository{
		ID: repoID, InstallationID: installationID,
		FullName: "acme/" + name, DefaultBranch: "main",
		Enabled: opts.repoEnabled, TeamID: &teamID,
	}
	if err := f.st.UpsertRepository(f.ctx, repo); err != nil {
		f.t.Fatalf("UpsertRepository(%q): %v", name, err)
	}

	sha := opts.headSHA
	if sha == "" {
		sha = "sha-" + name
	}
	pr := store.PullRequest{
		RepositoryID: repoID, Number: int(f.seq), Title: "PR " + name,
		AuthorLogin: "octocat", HeadSHA: sha, BaseRef: "main", HeadRef: "branch-" + name,
		HTMLURL: "https://github.com/acme/" + name + "/pull/1",
		Draft:   opts.draft, IsFork: opts.fork, State: "open", QAStatus: store.StatusPending,
		GitHubUpdatedAt: time.Now().UTC(),
	}
	if err := f.st.DB.WithContext(f.ctx).Create(&pr).Error; err != nil {
		f.t.Fatalf("create pull request %q: %v", name, err)
	}
	return pr
}

func eligibleOpts() seedOpts {
	return seedOpts{repoEnabled: true, withPolicy: true, profileEnabled: true}
}

func TestTriggerEnqueuesReview(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("app", eligibleOpts())

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}

	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	run := runs[0]
	if run.Status != store.RunQueued || run.Trigger != "webhook" || run.HeadSHA != pr.HeadSHA {
		t.Errorf("run = status %q trigger %q sha %q, want queued/webhook/%q",
			run.Status, run.Trigger, run.HeadSHA, pr.HeadSHA)
	}
	if run.CloneTokenEnc == "" {
		t.Fatal("clone_token_enc is empty")
	}
	token, err := f.trigger.Box.Open(run.CloneTokenEnc)
	if err != nil {
		t.Fatalf("open clone token: %v", err)
	}
	if string(token) != "read-token" {
		t.Errorf("clone token = %q, want %q", token, "read-token")
	}
	if len(f.jobs.reviews) != 1 || f.jobs.reviews[0] != run.ID {
		t.Errorf("EnqueueReview calls = %v, want [%d]", f.jobs.reviews, run.ID)
	}
	if len(f.jobs.publishes) != 0 || len(f.jobs.health) != 0 {
		t.Errorf("unexpected publish/health enqueues: %v / %v", f.jobs.publishes, f.jobs.health)
	}
	if len(f.checks.started) != 1 || f.checks.started[0] != run.ID {
		t.Errorf("StartCheck calls = %v, want [%d]", f.checks.started, run.ID)
	}
	if len(f.checks.cancelled) != 0 {
		t.Errorf("CancelCheck calls = %v, want none", f.checks.cancelled)
	}
}

func TestTriggerSameSHADeduplicates(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("app", eligibleOpts())

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("first OnPullRequest() error = %v", err)
	}
	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("second OnPullRequest() error = %v", err)
	}

	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if len(f.jobs.reviews) != 1 || len(f.checks.started) != 1 {
		t.Errorf("enqueues = %v, checks = %v; want one each", f.jobs.reviews, f.checks.started)
	}
}

func TestTriggerSupersedesOtherSHA(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("app", eligibleOpts())

	old, err := f.st.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: "old-sha", Trigger: "webhook",
		Status: store.RunRunning, Attempt: 1,
	})
	if err != nil {
		t.Fatalf("seed running run: %v", err)
	}

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}

	updated, err := f.st.ReviewRun(f.ctx, old.ID)
	if err != nil {
		t.Fatalf("ReviewRun(old): %v", err)
	}
	if updated.Status != store.RunSuperseded {
		t.Errorf("old run status = %q, want %q", updated.Status, store.RunSuperseded)
	}
	if len(f.checks.cancelled) != 1 || f.checks.cancelled[0] != old.ID {
		t.Errorf("CancelCheck calls = %v, want [%d]", f.checks.cancelled, old.ID)
	}

	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
	newRun := runs[1]
	if newRun.Status != store.RunQueued || newRun.HeadSHA != pr.HeadSHA {
		t.Errorf("new run = status %q sha %q, want queued/%q", newRun.Status, newRun.HeadSHA, pr.HeadSHA)
	}
	if len(f.jobs.reviews) != 1 || f.jobs.reviews[0] != newRun.ID {
		t.Errorf("EnqueueReview calls = %v, want [%d]", f.jobs.reviews, newRun.ID)
	}
}

func TestTriggerSkipsIneligible(t *testing.T) {
	f := newTriggerFixture(t)

	cases := []struct {
		name string
		opts seedOpts
	}{
		{"fork", seedOpts{fork: true, withPolicy: true, profileEnabled: true}},
		{"draft", seedOpts{draft: true, withPolicy: true, profileEnabled: true}},
		{"disabled", seedOpts{withPolicy: true, profileEnabled: true}},
		{"no-policy", seedOpts{profileEnabled: true}},
		{"disabled-profile", seedOpts{withPolicy: true}},
	}
	for _, tc := range cases {
		pr := f.seed(tc.name, tc.opts)
		if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
			t.Fatalf("OnPullRequest(%s) error = %v", tc.name, err)
		}
	}

	total := 0
	for _, tc := range cases {
		var count int64
		if err := f.st.DB.WithContext(f.ctx).Model(&store.ReviewRun{}).Count(&count).Error; err != nil {
			t.Fatalf("count runs (%s): %v", tc.name, err)
		}
		total += int(count)
	}
	if total != 0 {
		t.Errorf("created runs = %d, want 0", total)
	}
	if len(f.jobs.reviews) != 0 || len(f.checks.started) != 0 {
		t.Errorf("unexpected enqueues/checks: %v / %v", f.jobs.reviews, f.checks.started)
	}
}

func TestEnqueuePendingSkipsFailedSHA(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("app", eligibleOpts())

	if _, err := f.st.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, Trigger: "webhook",
		Status: store.RunFailed, Attempt: 1,
	}); err != nil {
		t.Fatalf("seed failed run: %v", err)
	}

	created, err := f.trigger.EnqueuePending(f.ctx, 50)
	if err != nil {
		t.Fatalf("EnqueuePending() error = %v", err)
	}
	if created != 0 {
		t.Errorf("created = %d, want 0", created)
	}
	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].Status != store.RunFailed {
		t.Errorf("runs = %+v, want only the seeded failed run", runs)
	}
	if len(f.jobs.reviews) != 0 || len(f.checks.started) != 0 {
		t.Errorf("unexpected enqueues/checks: %v / %v", f.jobs.reviews, f.checks.started)
	}
}

func TestTriggerRetryEnqueuesAfterFailedSHA(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("app", eligibleOpts())

	if _, err := f.st.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, Trigger: "webhook",
		Status: store.RunFailed, Attempt: 1,
	}); err != nil {
		t.Fatalf("seed failed run: %v", err)
	}

	if err := f.trigger.Retry(f.ctx, pr.ID); err != nil {
		t.Fatalf("Retry() error = %v", err)
	}

	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
	if runs[1].Trigger != "retry" || runs[1].Status != store.RunQueued {
		t.Errorf("new run = trigger %q status %q, want retry/queued", runs[1].Trigger, runs[1].Status)
	}
	if len(f.jobs.reviews) != 1 || f.jobs.reviews[0] != runs[1].ID {
		t.Errorf("EnqueueReview calls = %v, want [%d]", f.jobs.reviews, runs[1].ID)
	}
}
