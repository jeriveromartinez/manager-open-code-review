package review

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

type triggerCall struct {
	prID    uint
	headSHA string
	delay   time.Duration
}

type triggerTestJobs struct {
	mu          sync.Mutex
	reviews     []uint
	publishes   []uint
	health      []uint
	triggers    []triggerCall
	failReviews int
}

func (j *triggerTestJobs) EnqueueReview(_ context.Context, runID uint) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.reviews = append(j.reviews, runID)
	if j.failReviews > 0 {
		j.failReviews--
		return errors.New("fake enqueue failure")
	}
	return nil
}

func (j *triggerTestJobs) EnqueuePublish(_ context.Context, runID uint) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.publishes = append(j.publishes, runID)
	return nil
}

func (j *triggerTestJobs) EnqueueLLMHealth(_ context.Context, profileID uint) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.health = append(j.health, profileID)
	return nil
}

func (j *triggerTestJobs) EnqueueReviewTrigger(_ context.Context, prID uint, headSHA string, delay time.Duration) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.triggers = append(j.triggers, triggerCall{prID: prID, headSHA: headSHA, delay: delay})
	return nil
}

type triggerTestChecks struct {
	mu         sync.Mutex
	started    []uint
	cancelled  []uint
	failStarts int
}

func (c *triggerTestChecks) StartCheck(_ context.Context, runID uint) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started = append(c.started, runID)
	if c.failStarts > 0 {
		c.failStarts--
		return errors.New("fake start check failure")
	}
	return nil
}

func (c *triggerTestChecks) CancelCheck(_ context.Context, runID uint) error {
	c.mu.Lock()
	defer c.mu.Unlock()
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
	t            *testing.T
	ctx          context.Context
	st           *store.Store
	jobs         *triggerTestJobs
	checks       *triggerTestChecks
	trigger      *Trigger
	seq          int64
	tokenExpires time.Time

	mu           sync.Mutex
	mergeBase    string
	mergeBases   map[string]string
	compareErr   bool
	compareCalls int
	compareBases []string
	reviewReqs   []triggerReviewRequest
}

// triggerReviewRequest is one call the fake GitHub received on the review
// request endpoint.
type triggerReviewRequest struct {
	Repo          string
	Number        int
	Reviewers     []string
	TeamReviewers []string
}

type seedOpts struct {
	draft          bool
	fork           bool
	origin         string
	authorAssoc    string
	additions      int
	deletions      int
	repoEnabled    bool
	withPolicy     bool
	profileEnabled bool
	headSHA        string
	state          string
	requireGreenCI bool
	reviewerLogin  string
	reviewerTeam   string
	allowExternal  bool
	externalDiff   int
	externalRuns   int
	externalBudget int64
	externalAssocs []string
}

func newTriggerFixture(t *testing.T) *triggerFixture {
	t.Helper()

	st := testutil.NewStore(t)
	box, err := crypto.NewBox(bytes.Repeat([]byte{7}, crypto.KeySize))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}

	expiresAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	f := &triggerFixture{
		t:            t,
		ctx:          context.Background(),
		st:           st,
		jobs:         &triggerTestJobs{},
		checks:       &triggerTestChecks{},
		tokenExpires: expiresAt,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/issues/") {
			parts := strings.Split(r.URL.Path, "/")
			if parts[len(parts)-1] != "12" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"number":12,"title":"Retries","body":"## Acceptance criteria\n- [ ] backoff"}`)
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/compare/") {
			_, rest, _ := strings.Cut(r.URL.Path, "/compare/")
			base, _, _ := strings.Cut(rest, "...")
			f.mu.Lock()
			f.compareCalls++
			f.compareBases = append(f.compareBases, base)
			mergeBase, fail := f.mergeBase, f.compareErr
			if pinned, ok := f.mergeBases[base]; ok {
				mergeBase = pinned
			}
			f.mu.Unlock()
			if fail {
				http.Error(w, "compare unavailable", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"merge_base_commit":{"sha":%q}}`, mergeBase)
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/requested_reviewers") {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			var in struct {
				Reviewers     []string `json:"reviewers"`
				TeamReviewers []string `json:"team_reviewers"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &in)
			number := 0
			repo := ""
			if len(parts) == 6 {
				number, _ = strconv.Atoi(parts[4])
				repo = parts[1] + "/" + parts[2]
			}
			f.mu.Lock()
			f.reviewReqs = append(f.reviewReqs, triggerReviewRequest{
				Repo: repo, Number: number,
				Reviewers: in.Reviewers, TeamReviewers: in.TeamReviewers,
			})
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"number":1}`))
			return
		}
		if r.Method != http.MethodPost ||
			!strings.HasPrefix(r.URL.Path, "/app/installations/") ||
			!strings.HasSuffix(r.URL.Path, "/access_tokens") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"token":"read-token","expires_at":%q}`, expiresAt.Format(time.RFC3339))
	}))
	t.Cleanup(srv.Close)

	app := &github.App{
		Creds:  github.AppCredentials{AppID: 12345, PEM: triggerTestPEM(t)},
		APIURL: srv.URL,
	}
	f.trigger = &Trigger{
		Store:   st,
		Apps:    triggerTestApps{app: app},
		Jobs:    f.jobs,
		Checks:  f.checks,
		Box:     box,
		Compare: &github.Compare{Apps: triggerTestApps{app: app}},
	}
	return f
}

// setCompare programs the fake Compare API for the next resolutions.
func (f *triggerFixture) setCompare(mergeBase string, fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mergeBase = mergeBase
	f.compareErr = fail
}

func (f *triggerFixture) compares() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.compareCalls
}

// setCompareByBase programs the fake Compare API per base, so a test can make
// the mutable branch name and the recorded tip resolve different comparisons.
func (f *triggerFixture) setCompareByBase(bases map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mergeBases = bases
}

func (f *triggerFixture) comparedBases() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.compareBases...)
}

// setRunBaseTip seeds the base tip a run snapshot carries, the way a real run
// created by the trigger records it.
func (f *triggerFixture) setRunBaseTip(runID uint, tip string) {
	f.t.Helper()
	if err := f.st.DB.WithContext(f.ctx).Model(&store.ReviewRun{}).
		Where("id = ?", runID).Update("base_tip_sha", tip).Error; err != nil {
		f.t.Fatalf("set base tip of run %d: %v", runID, err)
	}
}

// setPRComparison rewrites the comparison columns the way a webhook sync or the
// poller would, without going through a GitHub payload.
func (f *triggerFixture) setPRComparison(pr store.PullRequest, baseRef, baseSHA, mergeBase string) store.PullRequest {
	f.t.Helper()
	if err := f.st.DB.WithContext(f.ctx).Model(&store.PullRequest{}).Where("id = ?", pr.ID).Updates(map[string]any{
		"base_ref": baseRef, "base_sha": baseSHA, "merge_base_sha": mergeBase,
	}).Error; err != nil {
		f.t.Fatalf("set comparison of PR %d: %v", pr.ID, err)
	}
	var updated store.PullRequest
	if err := f.st.DB.WithContext(f.ctx).Take(&updated, pr.ID).Error; err != nil {
		f.t.Fatalf("reload PR %d: %v", pr.ID, err)
	}
	return updated
}

// seedRun creates a review run with a comparison snapshot.
func (f *triggerFixture) seedRun(pr store.PullRequest, headSHA, baseRef, baseSHA, status string) store.ReviewRun {
	f.t.Helper()
	run, err := f.st.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: headSHA, BaseRef: baseRef, BaseSHA: baseSHA,
		Status: status, Attempt: 1,
	})
	if err != nil {
		f.t.Fatalf("seed run: %v", err)
	}
	return run
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
		policy := store.QAPolicy{
			TeamID: team.ID, LLMProfileID: profile.ID, RequireGreenCI: opts.requireGreenCI,
			ReviewerLogin: opts.reviewerLogin, ReviewerTeam: opts.reviewerTeam,
			AllowExternalReviews:     opts.allowExternal,
			MaxExternalDiffLines:     opts.externalDiff,
			MaxExternalRunsPerDay:    opts.externalRuns,
			ExternalDailyTokenBudget: opts.externalBudget,
		}
		if len(opts.externalAssocs) > 0 {
			raw, err := json.Marshal(opts.externalAssocs)
			if err != nil {
				f.t.Fatalf("encode external associations: %v", err)
			}
			policy.ExternalTrustedAssociations = raw
		}
		if _, err := f.st.SaveQAPolicy(f.ctx, policy); err != nil {
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
	state := opts.state
	if state == "" {
		state = "open"
	}
	origin := opts.origin
	if origin == "" && opts.fork {
		origin = store.OriginCrossRepo
	}
	association := opts.authorAssoc
	if association == "" {
		association = "CONTRIBUTOR"
	}
	pr := store.PullRequest{
		RepositoryID: repoID, Number: int(f.seq), Title: "PR " + name,
		AuthorLogin: "octocat", AuthorAssociation: association,
		HeadSHA: sha, BaseRef: "main", HeadRef: "branch-" + name,
		HTMLURL: "https://github.com/acme/" + name + "/pull/1",
		Draft:   opts.draft, IsFork: opts.fork, Origin: origin,
		Additions: opts.additions, Deletions: opts.deletions,
		State: state, QAStatus: store.StatusPending,
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
	if run.CloneTokenExpiresAt == nil || !run.CloneTokenExpiresAt.Equal(f.tokenExpires) {
		t.Errorf("clone token expiry = %v, want %v", run.CloneTokenExpiresAt, f.tokenExpires)
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

func TestTriggerPersistsLinkedIssueContext(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("app", eligibleOpts())
	if err := f.st.DB.WithContext(f.ctx).Model(&store.PullRequest{}).
		Where("id = ?", pr.ID).Update("body", "Closes #12").Error; err != nil {
		t.Fatalf("set PR body: %v", err)
	}

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}
	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %v, err = %v", runs, err)
	}
	var linked linkedContext
	if err := json.Unmarshal(runs[0].LinkedIssues, &linked); err != nil {
		t.Fatalf("decode linked context %q: %v", runs[0].LinkedIssues, err)
	}
	if len(linked.Issues) != 1 || linked.Issues[0].Title != "Retries" ||
		!strings.Contains(linked.Issues[0].Body, "backoff") {
		t.Errorf("linked context = %+v, want the issue fetched in the control process", linked)
	}
}

// TestRefreshQueuedTokensRenewsExpiringRuns covers the queue backlog case: a
// run whose token is about to expire gets a fresh one before a worker claims
// it, while runs with a valid token are left alone.
func TestRefreshQueuedTokensRenewsExpiringRuns(t *testing.T) {
	f := newTriggerFixture(t)
	stalePR := f.seed("stale", eligibleOpts())
	freshPR := f.seed("fresh", eligibleOpts())
	for _, pr := range []store.PullRequest{stalePR, freshPR} {
		if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
			t.Fatalf("OnPullRequest(%d) error = %v", pr.ID, err)
		}
	}

	staleRuns, err := f.st.ListReviewRuns(f.ctx, stalePR.ID)
	if err != nil || len(staleRuns) != 1 {
		t.Fatalf("stale runs = %v, err = %v", staleRuns, err)
	}
	freshRuns, err := f.st.ListReviewRuns(f.ctx, freshPR.ID)
	if err != nil || len(freshRuns) != 1 {
		t.Fatalf("fresh runs = %v, err = %v", freshRuns, err)
	}
	oldSealed, err := f.trigger.Box.Seal([]byte("old-token"))
	if err != nil {
		t.Fatalf("seal old token: %v", err)
	}
	nearExpiry := time.Now().Add(5 * time.Minute).UTC()
	if err := f.st.DB.WithContext(f.ctx).Model(&store.ReviewRun{}).
		Where("id = ?", staleRuns[0].ID).
		Updates(map[string]any{"clone_token_enc": oldSealed, "clone_token_expires_at": nearExpiry}).Error; err != nil {
		t.Fatalf("simulate expiring run: %v", err)
	}

	refreshed, err := f.trigger.RefreshQueuedTokens(f.ctx, 10)
	if err != nil {
		t.Fatalf("RefreshQueuedTokens() error = %v", err)
	}
	if refreshed != 1 {
		t.Errorf("refreshed = %d, want 1", refreshed)
	}

	renewed, err := f.st.ReviewRun(f.ctx, staleRuns[0].ID)
	if err != nil {
		t.Fatalf("reload stale run: %v", err)
	}
	token, err := f.trigger.Box.Open(renewed.CloneTokenEnc)
	if err != nil {
		t.Fatalf("open renewed token: %v", err)
	}
	if string(token) != "read-token" {
		t.Errorf("renewed token = %q, want %q", token, "read-token")
	}
	if renewed.CloneTokenExpiresAt == nil || !renewed.CloneTokenExpiresAt.Equal(f.tokenExpires) {
		t.Errorf("renewed expiry = %v, want %v", renewed.CloneTokenExpiresAt, f.tokenExpires)
	}

	untouched, err := f.st.ReviewRun(f.ctx, freshRuns[0].ID)
	if err != nil {
		t.Fatalf("reload fresh run: %v", err)
	}
	if untouched.CloneTokenEnc != freshRuns[0].CloneTokenEnc {
		t.Error("a run with a valid token was refreshed")
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
	// The second delivery repairs the queued run: check/enqueue may be called
	// again, but always for the same single run.
	if len(f.jobs.reviews) == 0 || len(f.checks.started) == 0 {
		t.Fatalf("enqueues = %v, checks = %v; want at least one each", f.jobs.reviews, f.checks.started)
	}
	for _, id := range f.jobs.reviews {
		if id != runs[0].ID {
			t.Errorf("EnqueueReview call for %d, want %d", id, runs[0].ID)
		}
	}
	for _, id := range f.checks.started {
		if id != runs[0].ID {
			t.Errorf("StartCheck call for %d, want %d", id, runs[0].ID)
		}
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

// Retry skips the SHA guard, so it hits the partial unique index when an active
// run already exists. That must heal the existing run, not fail the job.
func TestTriggerRetryRepairsActiveSameSHARun(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("app", eligibleOpts())

	existing, err := f.st.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, Trigger: "webhook",
		Status: store.RunQueued, Attempt: 1,
	})
	if err != nil {
		t.Fatalf("seed queued run: %v", err)
	}

	if err := f.trigger.Retry(f.ctx, pr.ID); err != nil {
		t.Fatalf("Retry() error = %v", err)
	}

	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != existing.ID || runs[0].Status != store.RunQueued {
		t.Fatalf("runs = %+v, want the original queued run", runs)
	}
	if len(f.jobs.reviews) != 1 || f.jobs.reviews[0] != existing.ID {
		t.Errorf("EnqueueReview calls = %v, want [%d]", f.jobs.reviews, existing.ID)
	}
	if len(f.checks.started) != 1 || f.checks.started[0] != existing.ID {
		t.Errorf("StartCheck calls = %v, want [%d]", f.checks.started, existing.ID)
	}
	if len(f.checks.cancelled) != 0 {
		t.Errorf("CancelCheck calls = %v, want none for the same SHA", f.checks.cancelled)
	}
}

// A retarget keeps the head but changes the prompt context: the active run of
// the old comparison is superseded and a new run is queued without needing
// Compare.
func TestTriggerRetargetSupersedesSameHeadComparison(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("app", eligibleOpts())
	old := f.seedRun(pr, pr.HeadSHA, "main", "mb-main", store.RunRunning)
	pr = f.setPRComparison(pr, "release", "tip-release", "")

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}

	updated, err := f.st.ReviewRun(f.ctx, old.ID)
	if err != nil {
		t.Fatalf("ReviewRun(old): %v", err)
	}
	if updated.Status != store.RunSuperseded {
		t.Errorf("old run status = %q, want superseded", updated.Status)
	}
	if len(f.checks.cancelled) != 1 || f.checks.cancelled[0] != old.ID {
		t.Errorf("CancelCheck calls = %v, want [%d]", f.checks.cancelled, old.ID)
	}
	if got := f.compares(); got != 0 {
		t.Errorf("Compare calls = %d, want none for a retarget", got)
	}

	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
	created := runs[1]
	if created.Status != store.RunQueued || created.HeadSHA != pr.HeadSHA ||
		created.BaseRef != "release" || created.BaseSHA != "" {
		t.Errorf("new run = %+v, want queued at the same head with the new base", created)
	}
	if len(f.jobs.reviews) != 1 || f.jobs.reviews[0] != created.ID {
		t.Errorf("EnqueueReview calls = %v, want [%d]", f.jobs.reviews, created.ID)
	}
}

// The base advanced without changing the merge base: Compare proves the
// comparison is equivalent and the existing result is reused.
func TestTriggerReusesComparisonWhenBaseAdvances(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("app", eligibleOpts())
	f.seedRun(pr, pr.HeadSHA, "main", "mb-1", store.RunSucceeded)
	pr = f.setPRComparison(pr, "main", "tip-2", "")
	f.setCompare("mb-1", false)

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}

	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want the existing one to be reused", len(runs))
	}
	if got := f.compares(); got != 1 {
		t.Errorf("Compare calls = %d, want 1", got)
	}
	var saved store.PullRequest
	if err := f.st.DB.WithContext(f.ctx).Take(&saved, pr.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.MergeBaseSHA != "mb-1" {
		t.Errorf("merge_base_sha = %q, want mb-1", saved.MergeBaseSHA)
	}
	if len(f.jobs.reviews) != 0 || len(f.checks.started) != 0 {
		t.Errorf("unexpected enqueues/checks: %v / %v", f.jobs.reviews, f.checks.started)
	}
	// The terminal run finished while its comparison was unresolved, so its
	// publication had been skipped; adopting the comparison republishes it.
	if len(f.jobs.publishes) != 1 || f.jobs.publishes[0] != runs[0].ID {
		t.Errorf("EnqueuePublish calls = %v, want [%d]", f.jobs.publishes, runs[0].ID)
	}
}

// Compare must be pinned to the base tip recorded on the pull request, not the
// mutable branch name: a base push after the metadata read must not resolve a
// newer comparison and record its merge base under the old tip.
func TestTriggerPinsCompareToRecordedBaseTip(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("app", eligibleOpts())
	run := f.seedRun(pr, pr.HeadSHA, "main", "mb-1", store.RunSucceeded)
	f.setRunBaseTip(run.ID, "tip-1")
	pr = f.setPRComparison(pr, "main", "tip-2", "")
	// Only the recorded tip still resolves the reviewed comparison; the branch
	// moved again in GitHub after the metadata was read.
	f.setCompareByBase(map[string]string{"main": "mb-elsewhere", "tip-2": "mb-1"})

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}

	if got := f.comparedBases(); len(got) != 1 || got[0] != "tip-2" {
		t.Errorf("Compare bases = %v, want [tip-2]", got)
	}
	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("runs = %+v, want the existing run to be adopted", runs)
	}
	if runs[0].BaseSHA != "mb-1" {
		t.Errorf("adopted run base = %q, want mb-1", runs[0].BaseSHA)
	}
	var saved store.PullRequest
	if err := f.st.DB.WithContext(f.ctx).Take(&saved, pr.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.MergeBaseSHA != "mb-1" {
		t.Errorf("merge_base_sha = %q, want mb-1", saved.MergeBaseSHA)
	}
}

// A run adopted because the base advanced without changing the merge base must
// become the current comparison: a later republish (for example a CI change)
// has to find it instead of reporting no verdict forever.
func TestTriggerAdoptedRunRemainsCurrentForRepublish(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("app", eligibleOpts())
	run := f.seedRun(pr, pr.HeadSHA, "main", "mb-1", store.RunSucceeded)
	f.setRunBaseTip(run.ID, "tip-1")
	pr = f.setPRComparison(pr, "main", "tip-2", "")
	f.setCompare("mb-1", false)

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}
	if got := f.compares(); got != 1 {
		t.Fatalf("Compare calls = %d, want 1", got)
	}
	saved, err := f.st.PullRequest(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("PullRequest: %v", err)
	}

	current, ok, err := f.st.CurrentVerdictRun(f.ctx, saved)
	if err != nil {
		t.Fatalf("CurrentVerdictRun: %v", err)
	}
	if !ok || current.ID != run.ID {
		t.Fatalf("current verdict = %+v/%v, want the adopted run %d", current, ok, run.ID)
	}
	if current.BaseTipSHA != "tip-2" {
		t.Errorf("adopted run base tip = %q, want tip-2", current.BaseTipSHA)
	}
}

// A rewritten base reports a different merge base through Compare: the run is
// a new comparison even with the same head.
func TestTriggerNewRunWhenBaseRewritten(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("app", eligibleOpts())
	f.seedRun(pr, pr.HeadSHA, "main", "mb-old", store.RunSucceeded)
	pr = f.setPRComparison(pr, "main", "tip-rewritten", "")
	f.setCompare("mb-new", false)

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}

	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want a new run", len(runs))
	}
	if got := runs[1]; got.Status != store.RunQueued || got.BaseRef != "main" || got.BaseSHA != "mb-new" {
		t.Errorf("new run = %+v, want queued with the resolved merge base", got)
	}
	if got := f.compares(); got != 1 {
		t.Errorf("Compare calls = %d, want 1", got)
	}
	var saved store.PullRequest
	if err := f.st.DB.WithContext(f.ctx).Take(&saved, pr.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.MergeBaseSHA != "mb-new" {
		t.Errorf("merge_base_sha = %q, want mb-new", saved.MergeBaseSHA)
	}
}

// A failing Compare must never make the trigger reuse a result silently.
func TestTriggerCompareFailureEnqueuesConservativeRun(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("app", eligibleOpts())
	f.seedRun(pr, pr.HeadSHA, "main", "mb-old", store.RunSucceeded)
	pr = f.setPRComparison(pr, "main", "tip-force-push", "")
	f.setCompare("", true)

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}

	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want a conservative new run", len(runs))
	}
	if got := runs[1]; got.Status != store.RunQueued || got.BaseSHA != "" {
		t.Errorf("new run = %+v, want queued with an unknown merge base", got)
	}
	if got := f.compares(); got != 1 {
		t.Errorf("Compare calls = %d, want 1", got)
	}
}

// A metadata edit keeps the same comparison, so the existing result covers it;
// a legacy run without key fields keeps covering it too.
func TestTriggerSameComparisonReusesResult(t *testing.T) {
	t.Run("resolved comparison", func(t *testing.T) {
		f := newTriggerFixture(t)
		pr := f.seed("app", eligibleOpts())
		f.seedRun(pr, pr.HeadSHA, "main", "mb-1", store.RunSucceeded)
		f.setPRComparison(pr, "main", "tip", "mb-1")

		if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
			t.Fatalf("OnPullRequest() error = %v", err)
		}
		runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
		if err != nil {
			t.Fatalf("ListReviewRuns: %v", err)
		}
		if len(runs) != 1 {
			t.Fatalf("runs = %d, want the existing one", len(runs))
		}
		if got := f.compares(); got != 0 {
			t.Errorf("Compare calls = %d, want none for an unchanged comparison", got)
		}
	})

	t.Run("legacy empty key", func(t *testing.T) {
		f := newTriggerFixture(t)
		pr := f.seed("app", eligibleOpts())
		f.seedRun(pr, pr.HeadSHA, "", "", store.RunSucceeded)
		f.setPRComparison(pr, "main", "tip", "mb-1")

		if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
			t.Fatalf("OnPullRequest() error = %v", err)
		}
		runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
		if err != nil {
			t.Fatalf("ListReviewRuns: %v", err)
		}
		if len(runs) != 1 {
			t.Fatalf("runs = %d, want the legacy run to keep covering the PR", len(runs))
		}
	})
}

// The periodic reconciliation must schedule a pull request whose head moved
// while its webhook was lost, and reuse the result when only the base advanced.
func TestEnqueuePendingResolvesMissedWebhooks(t *testing.T) {
	t.Run("new head schedules", func(t *testing.T) {
		f := newTriggerFixture(t)
		pr := f.seed("app", eligibleOpts())
		f.seedRun(pr, pr.HeadSHA, "main", "mb-1", store.RunSucceeded)
		if err := f.st.DB.WithContext(f.ctx).Model(&store.PullRequest{}).Where("id = ?", pr.ID).
			Updates(map[string]any{"head_sha": "sha-new", "base_sha": "tip-2", "merge_base_sha": ""}).Error; err != nil {
			t.Fatalf("move the head: %v", err)
		}

		created, err := f.trigger.EnqueuePending(f.ctx, 50)
		if err != nil {
			t.Fatalf("EnqueuePending() error = %v", err)
		}
		if created != 1 {
			t.Fatalf("created = %d, want 1", created)
		}
		runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
		if err != nil {
			t.Fatalf("ListReviewRuns: %v", err)
		}
		if len(runs) != 2 || runs[1].HeadSHA != "sha-new" {
			t.Fatalf("runs = %+v, want a run for the new head", runs)
		}
	})

	t.Run("base move with the same merge base reuses", func(t *testing.T) {
		f := newTriggerFixture(t)
		pr := f.seed("app", eligibleOpts())
		f.seedRun(pr, pr.HeadSHA, "main", "mb-1", store.RunSucceeded)
		if err := f.st.DB.WithContext(f.ctx).Model(&store.PullRequest{}).Where("id = ?", pr.ID).
			Updates(map[string]any{"base_sha": "tip-2", "merge_base_sha": ""}).Error; err != nil {
			t.Fatalf("move the base: %v", err)
		}
		f.setCompare("mb-1", false)

		created, err := f.trigger.EnqueuePending(f.ctx, 50)
		if err != nil {
			t.Fatalf("EnqueuePending() error = %v", err)
		}
		if created != 0 {
			t.Fatalf("created = %d, want the equivalent comparison to be reused", created)
		}
		runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
		if err != nil {
			t.Fatalf("ListReviewRuns: %v", err)
		}
		if len(runs) != 1 {
			t.Fatalf("runs = %d, want only the existing run", len(runs))
		}
		if got := f.compares(); got != 1 {
			t.Errorf("Compare calls = %d, want 1", got)
		}
	})
}

// A failed StartCheck leaves a queued run behind. Redelivery must heal it
// instead of silently skipping the SHA forever.
func TestTriggerRepairsStrandedQueuedRunAfterStartCheckFailure(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("app", eligibleOpts())
	f.checks.failStarts = 1

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err == nil {
		t.Fatal("first OnPullRequest() error = nil, want the StartCheck failure")
	}
	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].Status != store.RunQueued {
		t.Fatalf("runs = %+v, want one queued run", runs)
	}
	if len(f.jobs.reviews) != 0 {
		t.Fatalf("EnqueueReview calls = %v, want none after StartCheck failure", f.jobs.reviews)
	}

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("second OnPullRequest() error = %v", err)
	}
	runs, err = f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].Status != store.RunQueued {
		t.Fatalf("runs = %+v, want the same single queued run", runs)
	}
	if len(f.checks.started) != 2 || f.checks.started[1] != runs[0].ID {
		t.Errorf("StartCheck calls = %v, want two calls for run %d", f.checks.started, runs[0].ID)
	}
	if len(f.jobs.reviews) != 1 || f.jobs.reviews[0] != runs[0].ID {
		t.Errorf("EnqueueReview calls = %v, want [%d]", f.jobs.reviews, runs[0].ID)
	}
}

// A failed EnqueueReview also leaves a queued run behind; the next delivery
// must re-enqueue that run without creating a second one.
func TestTriggerRepairsStrandedQueuedRunAfterEnqueueFailure(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("app", eligibleOpts())
	f.jobs.failReviews = 1

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err == nil {
		t.Fatal("first OnPullRequest() error = nil, want the enqueue failure")
	}
	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("second OnPullRequest() error = %v", err)
	}

	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].Status != store.RunQueued {
		t.Fatalf("runs = %+v, want one queued run", runs)
	}
	if len(f.jobs.reviews) != 2 {
		t.Fatalf("EnqueueReview calls = %v, want two attempts", f.jobs.reviews)
	}
	for _, id := range f.jobs.reviews {
		if id != runs[0].ID {
			t.Errorf("EnqueueReview call for %d, want %d", id, runs[0].ID)
		}
	}
	if len(f.checks.started) != 2 {
		t.Errorf("StartCheck calls = %v, want two (restart is idempotent)", f.checks.started)
	}
}

// Concurrent deliveries for one PR must be serialized by the advisory lock:
// one active run, no superseded run of the same SHA and no orphan check.
func TestTriggerConcurrentSamePRSha(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("app", eligibleOpts())

	const n = 4
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- f.trigger.OnPullRequest(f.ctx, pr.ID)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent OnPullRequest() error = %v", err)
		}
	}

	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	active := 0
	for _, run := range runs {
		if run.HeadSHA != pr.HeadSHA {
			t.Errorf("run %d has SHA %q, want %q", run.ID, run.HeadSHA, pr.HeadSHA)
		}
		if run.Status == store.RunQueued || run.Status == store.RunRunning {
			active++
		}
	}
	if active != 1 || len(runs) != 1 {
		t.Fatalf("runs = %+v, want exactly one active run", runs)
	}

	if len(f.jobs.reviews) == 0 || len(f.checks.started) == 0 {
		t.Fatalf("no enqueues/checks: jobs = %v, checks = %v", f.jobs.reviews, f.checks.started)
	}
	for _, id := range f.jobs.reviews {
		if id != runs[0].ID {
			t.Errorf("EnqueueReview call for %d, want %d", id, runs[0].ID)
		}
	}
	for _, id := range f.checks.started {
		if id != runs[0].ID {
			t.Errorf("StartCheck call for %d, want %d", id, runs[0].ID)
		}
	}
}

// Only a newly queued run changes what the panel shows; a redelivery that just
// repairs the existing run stays silent.
func TestEnqueueNotifiesThePR(t *testing.T) {
	f := newTriggerFixture(t)
	pub := &runTestEvents{}
	f.trigger.Events = pub
	pr := f.seed("app", eligibleOpts())
	want := []events.Event{{Type: "pr.updated", ID: strconv.FormatUint(uint64(pr.ID), 10)}}

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}
	if got := pub.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events after a new run = %+v, want %+v", got, want)
	}

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("redelivered OnPullRequest() error = %v", err)
	}
	if err := f.trigger.Retry(f.ctx, pr.ID); err != nil {
		t.Fatalf("Retry() over the active run error = %v", err)
	}
	if got := pub.snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("events after repairs = %+v, want unchanged %+v", got, want)
	}
	if len(f.jobs.reviews) != 3 {
		t.Errorf("EnqueueReview calls = %v, want the run repaired twice", f.jobs.reviews)
	}
}

func TestEnqueueWithFailedNotificationStillQueues(t *testing.T) {
	f := newTriggerFixture(t)
	f.trigger.Events = failingRunEvents{}
	pr := f.seed("app", eligibleOpts())

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}
	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 1 || len(f.jobs.reviews) != 1 {
		t.Errorf("runs = %d, EnqueueReview calls = %v; want one queued run", len(runs), f.jobs.reviews)
	}
}

// A configured debounce turns the push webhook into a scheduled job. The run
// must not be created yet: a burst of pushes would otherwise review every
// intermediate head.
func TestTriggerDebouncesPullRequest(t *testing.T) {
	f := newTriggerFixture(t)
	f.trigger.Debounce = 2 * time.Minute
	pr := f.seed("app", eligibleOpts())

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}

	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("runs = %d, want none before the debounce fires", len(runs))
	}
	if len(f.jobs.triggers) != 1 {
		t.Fatalf("EnqueueReviewTrigger calls = %v, want one", f.jobs.triggers)
	}
	got := f.jobs.triggers[0]
	if got.prID != pr.ID || got.headSHA != pr.HeadSHA || got.delay != 2*time.Minute {
		t.Errorf("scheduled trigger = %+v, want pull request %d head %q delay 2m", got, pr.ID, pr.HeadSHA)
	}
	if len(f.jobs.reviews) != 0 || len(f.checks.started) != 0 {
		t.Errorf("unexpected enqueues/checks before the debounce: %v / %v", f.jobs.reviews, f.checks.started)
	}
}

// The zero value must keep the immediate path used by every existing caller.
func TestTriggerWithoutDebounceStaysImmediate(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("app", eligibleOpts())

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}

	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 1 || len(f.jobs.reviews) != 1 {
		t.Errorf("runs = %d, EnqueueReview calls = %v; want one immediate run", len(runs), f.jobs.reviews)
	}
	if len(f.jobs.triggers) != 0 {
		t.Errorf("EnqueueReviewTrigger calls = %v, want none without a debounce", f.jobs.triggers)
	}
}

func TestTriggerDebounceSkipsDraftsForksAndDisabled(t *testing.T) {
	f := newTriggerFixture(t)
	f.trigger.Debounce = 2 * time.Minute
	for name, opts := range map[string]seedOpts{
		"draft":    {draft: true, repoEnabled: true, withPolicy: true, profileEnabled: true},
		"fork":     {fork: true, repoEnabled: true, withPolicy: true, profileEnabled: true},
		"disabled": {withPolicy: true, profileEnabled: true},
	} {
		pr := f.seed(name, opts)
		if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
			t.Fatalf("OnPullRequest(%s) error = %v", name, err)
		}
	}
	if len(f.jobs.triggers) != 0 {
		t.Errorf("EnqueueReviewTrigger calls = %v, want none for ineligible pull requests", f.jobs.triggers)
	}
}

func TestFireDebouncedCreatesRunForCurrentHead(t *testing.T) {
	f := newTriggerFixture(t)
	f.trigger.Debounce = 2 * time.Minute
	pr := f.seed("app", eligibleOpts())

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}
	if err := f.trigger.FireDebounced(f.ctx, pr.ID, pr.HeadSHA); err != nil {
		t.Fatalf("FireDebounced() error = %v", err)
	}

	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].Status != store.RunQueued || runs[0].Trigger != "webhook" {
		t.Fatalf("runs = %+v, want one queued webhook run", runs)
	}
	if len(f.jobs.reviews) != 1 || f.jobs.reviews[0] != runs[0].ID {
		t.Errorf("EnqueueReview calls = %v, want [%d]", f.jobs.reviews, runs[0].ID)
	}
}

// A push that arrived after the job was scheduled has its own job: the older
// one must no-op even though its args still match the PR row in every other way.
func TestFireDebouncedIgnoresStaleHead(t *testing.T) {
	f := newTriggerFixture(t)
	f.trigger.Debounce = 2 * time.Minute
	pr := f.seed("app", eligibleOpts())
	oldHead := pr.HeadSHA

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}
	if err := f.st.DB.WithContext(f.ctx).Model(&store.PullRequest{}).
		Where("id = ?", pr.ID).Update("head_sha", "sha-newer").Error; err != nil {
		t.Fatalf("move the head: %v", err)
	}

	if err := f.trigger.FireDebounced(f.ctx, pr.ID, oldHead); err != nil {
		t.Fatalf("FireDebounced(old head) error = %v", err)
	}
	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("runs = %+v, want none for the superseded head", runs)
	}

	if err := f.trigger.FireDebounced(f.ctx, pr.ID, "sha-newer"); err != nil {
		t.Fatalf("FireDebounced(current head) error = %v", err)
	}
	runs, err = f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].HeadSHA != "sha-newer" {
		t.Fatalf("runs = %+v, want one run for the current head", runs)
	}
}

// The pre-check is only a fast path: the head must still match when the config
// is reloaded under the PR lock, or a push landing in between would start the
// wrong review.
func TestEnqueueForHeadRejectsStaleHeadUnderLock(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("app", eligibleOpts())
	oldHead := pr.HeadSHA
	if err := f.st.DB.WithContext(f.ctx).Model(&store.PullRequest{}).
		Where("id = ?", pr.ID).Update("head_sha", "sha-newer").Error; err != nil {
		t.Fatalf("move the head: %v", err)
	}

	created, err := f.trigger.enqueueForHead(f.ctx, pr.ID, oldHead, "webhook", false, comparisonHint{})
	if err != nil {
		t.Fatalf("enqueueForHead(old head) error = %v", err)
	}
	if created {
		t.Fatal("enqueueForHead(old head) created a run, want none")
	}
	if runs, err := f.st.ListReviewRuns(f.ctx, pr.ID); err != nil || len(runs) != 0 {
		t.Fatalf("runs = %+v, err = %v; want none", runs, err)
	}
	if len(f.jobs.reviews) != 0 || len(f.checks.started) != 0 {
		t.Errorf("unexpected enqueues/checks: %v / %v", f.jobs.reviews, f.checks.started)
	}

	created, err = f.trigger.enqueueForHead(f.ctx, pr.ID, "sha-newer", "webhook", false, comparisonHint{})
	if err != nil || !created {
		t.Fatalf("enqueueForHead(current head) = %v, %v; want a created run", created, err)
	}
	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil || len(runs) != 1 || runs[0].HeadSHA != "sha-newer" {
		t.Fatalf("runs = %+v, err = %v; want one run for the current head", runs, err)
	}
}

// The PR can become a draft or a fork while the job waits, and the
// configuration can disappear (disabled repo, removed policy).
func TestFireDebouncedSkipsIneligiblePullRequests(t *testing.T) {
	cases := []struct {
		name string
		opts seedOpts
		head string
	}{
		{"draft", seedOpts{draft: true, repoEnabled: true, withPolicy: true, profileEnabled: true}, ""},
		{"fork", seedOpts{fork: true, repoEnabled: true, withPolicy: true, profileEnabled: true}, ""},
		{"closed", seedOpts{repoEnabled: true, withPolicy: true, profileEnabled: true, state: "closed"}, ""},
		{"merged", seedOpts{repoEnabled: true, withPolicy: true, profileEnabled: true, state: "merged"}, ""},
		{"disabled", seedOpts{withPolicy: true, profileEnabled: true}, ""},
		{"stale-head", eligibleOpts(), "sha-anterior"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTriggerFixture(t)
			f.trigger.Debounce = 2 * time.Minute
			pr := f.seed("app", tc.opts)
			head := tc.head
			if head == "" {
				head = pr.HeadSHA
			}

			if err := f.trigger.FireDebounced(f.ctx, pr.ID, head); err != nil {
				t.Fatalf("FireDebounced() error = %v", err)
			}
			runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
			if err != nil {
				t.Fatalf("ListReviewRuns: %v", err)
			}
			if len(runs) != 0 {
				t.Errorf("runs = %+v, want none", runs)
			}
			if len(f.jobs.reviews) != 0 || len(f.checks.started) != 0 {
				t.Errorf("unexpected enqueues/checks: %v / %v", f.jobs.reviews, f.checks.started)
			}
		})
	}
}

func (f *triggerFixture) reviewRequests() []triggerReviewRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]triggerReviewRequest(nil), f.reviewReqs...)
}

// External reviews are disabled by default, so the trigger hands the fork to a
// person through the official review-request API. Repeating the delivery stays
// deduplicated by pull request and head.
func TestTriggerRequestsHumanReviewForFork(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("fork", seedOpts{
		fork: true, repoEnabled: true, withPolicy: true, profileEnabled: true,
		reviewerLogin: "human-qa", reviewerTeam: "qa-team",
	})

	for i := 0; i < 2; i++ {
		if err := f.trigger.OnPullRequestIneligible(f.ctx, pr.ID); err != nil {
			t.Fatalf("OnPullRequestIneligible() #%d error = %v", i+1, err)
		}
	}

	calls := f.reviewRequests()
	if len(calls) != 1 {
		t.Fatalf("review requests = %+v, want one deduplicated call", calls)
	}
	got := calls[0]
	if got.Repo != "acme/fork" || got.Number != pr.Number {
		t.Errorf("review request target = %s#%d, want acme/fork#%d", got.Repo, got.Number, pr.Number)
	}
	if len(got.Reviewers) != 1 || got.Reviewers[0] != "human-qa" ||
		len(got.TeamReviewers) != 1 || got.TeamReviewers[0] != "qa-team" {
		t.Errorf("review request = %+v, want human-qa/qa-team", got)
	}
	saved, err := f.st.ReviewRequestFor(f.ctx, pr.ID, pr.HeadSHA)
	if err != nil {
		t.Fatalf("ReviewRequestFor() error = %v", err)
	}
	if saved.Status != store.ReviewRequestDelivered || saved.Cause != store.ReviewRequestCauseFork {
		t.Errorf("request = %+v, want a delivered fork request", saved)
	}
}

// A fork whose team has no policy still produces a queue item: the missing
// target is recorded as rejected instead of silently dropping the PR.
func TestTriggerRecordsForkWithoutPolicy(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("fork-no-policy", seedOpts{
		fork: true, repoEnabled: true, withPolicy: false, profileEnabled: true,
	})

	if err := f.trigger.OnPullRequestIneligible(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequestIneligible() error = %v", err)
	}
	if calls := f.reviewRequests(); len(calls) != 0 {
		t.Fatalf("review requests = %+v, want none without a target", calls)
	}
	saved, err := f.st.ReviewRequestFor(f.ctx, pr.ID, pr.HeadSHA)
	if err != nil {
		t.Fatalf("ReviewRequestFor() error = %v", err)
	}
	if saved.Status != store.ReviewRequestRejected || saved.ReviewerLogin != "" ||
		!strings.Contains(saved.LastError, "no reviewer") {
		t.Errorf("request = %+v, want a rejected row naming the missing target", saved)
	}
}

// An external contribution the policy authorizes is reviewed from the base
// repository's pull ref. No human request is created for it.
func TestTriggerReviewsAuthorizedExternalPR(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("authorized-fork", seedOpts{
		fork: true, origin: store.OriginCrossRepo, authorAssoc: "OWNER",
		repoEnabled: true, withPolicy: true, profileEnabled: true,
		allowExternal: true, additions: 10, deletions: 2,
	})

	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}

	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].Status != store.RunQueued || runs[0].HeadSHA != pr.HeadSHA {
		t.Fatalf("runs = %+v, want one queued run for the current head", runs)
	}
	if got := f.reviewRequests(); len(got) != 0 {
		t.Fatalf("review requests = %+v, want none for an authorized contribution", got)
	}
	if len(f.jobs.reviews) != 1 || len(f.checks.started) != 1 {
		t.Errorf("jobs/checks = %v/%v, want the run queued and its check started",
			f.jobs.reviews, f.checks.started)
	}
}

// A contribution outside the policy is never analysed: the reason is recorded
// as a human request instead.
func TestTriggerEscalatesExternalPROutsidePolicy(t *testing.T) {
	cases := []struct {
		name  string
		opts  seedOpts
		cause string
		prep  func(*triggerFixture, store.PullRequest)
	}{
		{
			name: "reviews disabled",
			opts: seedOpts{fork: true, repoEnabled: true, withPolicy: true, profileEnabled: true,
				reviewerLogin: "human-qa", authorAssoc: "OWNER"},
			cause: store.ReviewRequestCauseFork,
		},
		{
			name: "untrusted association",
			opts: seedOpts{fork: true, repoEnabled: true, withPolicy: true, profileEnabled: true,
				reviewerLogin: "human-qa", allowExternal: true, externalAssocs: []string{"MEMBER"}},
			cause: store.ReviewRequestCauseForkUnauthorized,
		},
		{
			name: "over the size limit",
			opts: seedOpts{fork: true, repoEnabled: true, withPolicy: true, profileEnabled: true,
				reviewerLogin: "human-qa", authorAssoc: "OWNER", allowExternal: true,
				externalDiff: 100, additions: 90, deletions: 20},
			cause: store.ReviewRequestCauseForkTooLarge,
		},
		{
			name: "over the token budget",
			opts: seedOpts{fork: true, repoEnabled: true, withPolicy: true, profileEnabled: true,
				reviewerLogin: "human-qa", authorAssoc: "OWNER", allowExternal: true,
				externalBudget: 100},
			cause: store.ReviewRequestCauseForkBudget,
			prep: func(f *triggerFixture, pr store.PullRequest) {
				run := f.seedRun(pr, "old-head", "main", "", store.RunSuperseded)
				tokens := int64(150)
				if err := f.st.DB.WithContext(f.ctx).Model(&store.ReviewRun{}).
					Where("id = ?", run.ID).Update("total_tokens", tokens).Error; err != nil {
					f.t.Fatalf("record token usage: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTriggerFixture(t)
			pr := f.seed("external", tc.opts)
			if tc.prep != nil {
				tc.prep(f, pr)
			}

			if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
				t.Fatalf("OnPullRequest() error = %v", err)
			}
			runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
			if err != nil {
				t.Fatalf("ListReviewRuns: %v", err)
			}
			for _, run := range runs {
				if run.Status == store.RunQueued || run.Status == store.RunRunning {
					t.Fatalf("external contribution was queued: %+v", run)
				}
			}
			if len(f.jobs.reviews) != 0 || len(f.checks.started) != 0 {
				t.Fatalf("external contribution was queued: jobs=%v checks=%v",
					f.jobs.reviews, f.checks.started)
			}
			saved, err := f.st.ReviewRequestFor(f.ctx, pr.ID, pr.HeadSHA)
			if err != nil {
				t.Fatalf("ReviewRequestFor: %v", err)
			}
			if saved.Cause != tc.cause || saved.Status != store.ReviewRequestDelivered {
				t.Fatalf("request = %+v, want cause %q delivered", saved, tc.cause)
			}
		})
	}
}

// The daily run limit counts runs of the same external author across the
// instance, so a burst of contributions is stopped even in another repository.
func TestTriggerEscalatesExternalPRRateLimited(t *testing.T) {
	f := newTriggerFixture(t)
	first := f.seed("external-one", seedOpts{
		fork: true, repoEnabled: true, withPolicy: true, profileEnabled: true,
		authorAssoc: "OWNER", allowExternal: true, externalRuns: 1,
	})
	second := f.seed("external-two", seedOpts{
		fork: true, repoEnabled: true, withPolicy: true, profileEnabled: true,
		authorAssoc: "OWNER", allowExternal: true, externalRuns: 1,
		reviewerLogin: "human-qa",
	})
	if err := f.trigger.OnPullRequest(f.ctx, first.ID); err != nil {
		t.Fatalf("OnPullRequest(first) error = %v", err)
	}
	if err := f.trigger.OnPullRequest(f.ctx, second.ID); err != nil {
		t.Fatalf("OnPullRequest(second) error = %v", err)
	}
	runs, err := f.st.ListReviewRuns(f.ctx, second.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("rate-limited contribution was queued: %+v", runs)
	}
	saved, err := f.st.ReviewRequestFor(f.ctx, second.ID, second.HeadSHA)
	if err != nil {
		t.Fatalf("ReviewRequestFor: %v", err)
	}
	if saved.Cause != store.ReviewRequestCauseForkRateLimited {
		t.Fatalf("request = %+v, want cause %q", saved, store.ReviewRequestCauseForkRateLimited)
	}
}

// A branch of the base repository is internal even when the repository itself
// is a fork of another project.
func TestTriggerReviewsInternalBranchOfForkOwnedRepo(t *testing.T) {
	f := newTriggerFixture(t)
	pr := f.seed("fork-owned-internal", seedOpts{
		origin: store.OriginInternal, authorAssoc: "NONE",
		repoEnabled: true, withPolicy: true, profileEnabled: true,
	})
	if err := f.trigger.OnPullRequest(f.ctx, pr.ID); err != nil {
		t.Fatalf("OnPullRequest() error = %v", err)
	}
	runs, err := f.st.ListReviewRuns(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ListReviewRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %+v, want the internal branch reviewed", runs)
	}
	if got := f.reviewRequests(); len(got) != 0 {
		t.Fatalf("review requests = %+v, want none", got)
	}
}

// A draft fork is not ready for a person yet, and an ordinary pull request is
// owned by the engine.
func TestTriggerDoesNotRequestReviewForOtherIneligiblePRs(t *testing.T) {
	f := newTriggerFixture(t)
	draftFork := f.seed("draft-fork", seedOpts{
		fork: true, draft: true, repoEnabled: true, withPolicy: true, profileEnabled: true,
		reviewerLogin: "human-qa",
	})
	draft := f.seed("draft", seedOpts{
		draft: true, repoEnabled: true, withPolicy: true, profileEnabled: true,
		reviewerLogin: "human-qa",
	})
	for _, pr := range []store.PullRequest{draftFork, draft} {
		if err := f.trigger.OnPullRequestIneligible(f.ctx, pr.ID); err != nil {
			t.Fatalf("OnPullRequestIneligible(%d) error = %v", pr.ID, err)
		}
	}
	if calls := f.reviewRequests(); len(calls) != 0 {
		t.Fatalf("review requests = %+v, want none", calls)
	}
}
