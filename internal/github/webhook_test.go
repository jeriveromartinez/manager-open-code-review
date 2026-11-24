package github_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/jeriveromartinez/qa-control-center/internal/events"
	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

const webhookTestSecret = "webhook-secret-value"

type fakeAppProvider struct {
	app *ghapp.App
	err error
}

func (p fakeAppProvider) App(context.Context) (*ghapp.App, error) { return p.app, p.err }

type recordingJobs struct {
	mu  sync.Mutex
	ids []int64
	err error
}

func (j *recordingJobs) EnqueueSync(_ context.Context, id int64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.ids = append(j.ids, id)
	return j.err
}

func (j *recordingJobs) fail(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.err = err
}

func (j *recordingJobs) snapshot() []int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]int64(nil), j.ids...)
}

type recordingPublisher struct {
	mu     sync.Mutex
	events []events.Event
}

func (p *recordingPublisher) Publish(_ context.Context, ev events.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, ev)
	return nil
}

func (p *recordingPublisher) snapshot() []events.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]events.Event(nil), p.events...)
}

type recordingReviews struct {
	mu         sync.Mutex
	ids        []uint
	ineligible []uint
	checks     []uint
	retries    []uint
	err        error
}

func (r *recordingReviews) OnPullRequest(_ context.Context, prID uint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, prID)
	return r.err
}

func (r *recordingReviews) OnPullRequestIneligible(_ context.Context, prID uint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ineligible = append(r.ineligible, prID)
	return r.err
}

func (r *recordingReviews) Retry(_ context.Context, prID uint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retries = append(r.retries, prID)
	return r.err
}

func (r *recordingReviews) OnChecksUpdated(_ context.Context, prID uint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks = append(r.checks, prID)
	return r.err
}

func (r *recordingReviews) snapshot() []uint {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uint(nil), r.ids...)
}

func (r *recordingReviews) ineligibleSnapshot() []uint {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uint(nil), r.ineligible...)
}

func (r *recordingReviews) retrySnapshot() []uint {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uint(nil), r.retries...)
}

func (r *recordingReviews) checksSnapshot() []uint {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uint(nil), r.checks...)
}

type recordingComments struct {
	mu     sync.Mutex
	repos  []string
	nums   []int
	bodies []string
	err    error
}

func (c *recordingComments) CommentOnPullRequest(_ context.Context, repoFullName string, number int, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.repos = append(c.repos, repoFullName)
	c.nums = append(c.nums, number)
	c.bodies = append(c.bodies, body)
	return c.err
}

func (c *recordingComments) snapshot() ([]string, []int, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.repos...),
		append([]int(nil), c.nums...),
		append([]string(nil), c.bodies...)
}

type recordingReconciler struct {
	mu        sync.Mutex
	republish []uint
	threads   []threadSync
	threadHit bool
	err       error
}

type threadSync struct {
	findingID string
	resolved  bool
}

func (r *recordingReconciler) Republish(_ context.Context, prID uint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.republish = append(r.republish, prID)
	return r.err
}

func (r *recordingReconciler) SetFindingThread(_ context.Context, prID uint, finding store.Finding, resolved bool) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.threads = append(r.threads, threadSync{findingID: finding.ID, resolved: resolved})
	return r.threadHit, r.err
}

func (r *recordingReconciler) republishSnapshot() []uint {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uint(nil), r.republish...)
}

func (r *recordingReconciler) threadSnapshot() []threadSync {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]threadSync(nil), r.threads...)
}

type webhookFixture struct {
	t        *testing.T
	ctx      context.Context
	st       *store.Store
	handler  *ghapp.WebhookHandler
	jobs     *recordingJobs
	pub      *recordingPublisher
	reviews  *recordingReviews
	findings *recordingReconciler
	comments *recordingComments
}

func newWebhookFixture(t *testing.T) *webhookFixture {
	t.Helper()

	st := testutil.NewStore(t)
	jobs := &recordingJobs{}
	pub := &recordingPublisher{}
	reviews := &recordingReviews{}
	findings := &recordingReconciler{}
	comments := &recordingComments{}
	h := &ghapp.WebhookHandler{
		Store: st,
		Apps: fakeAppProvider{app: &ghapp.App{
			Creds: ghapp.AppCredentials{WebhookSecret: webhookTestSecret},
		}},
		Events:   pub,
		Jobs:     jobs,
		Reviews:  reviews,
		Findings: findings,
		Comments: comments,
	}
	return &webhookFixture{
		t: t, ctx: context.Background(), st: st,
		handler: h, jobs: jobs, pub: pub, reviews: reviews,
		findings: findings, comments: comments,
	}
}

func signWebhook(body []byte) string {
	mac := hmac.New(sha256.New, []byte(webhookTestSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func (f *webhookFixture) deliver(eventType, fixture string) *httptest.ResponseRecorder {
	f.t.Helper()

	body := f.fixture(fixture)
	return f.deliverBody(eventType, body, signWebhook(body))
}

// fixture reads a webhook body and lets a test alter the delivery before
// signing it.
func (f *webhookFixture) fixture(name string) []byte {
	f.t.Helper()

	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		f.t.Fatalf("read fixture %q: %v", name, err)
	}
	return body
}

func (f *webhookFixture) deliverBody(eventType string, body []byte, signature string) *httptest.ResponseRecorder {
	f.t.Helper()

	return f.deliverBodyWithDelivery(eventType, body, signature, "")
}

// deliverBodyWithDelivery adds the X-GitHub-Delivery header, which the thread
// handler uses to ignore a redelivered event.
func (f *webhookFixture) deliverBodyWithDelivery(eventType string, body []byte, signature, delivery string) *httptest.ResponseRecorder {
	f.t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", eventType)
	req.Header.Set("X-Hub-Signature-256", signature)
	if delivery != "" {
		req.Header.Set("X-GitHub-Delivery", delivery)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func assertStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, want, rec.Body.String())
	}
}

func (f *webhookFixture) seedInstallation() {
	f.t.Helper()
	in := store.Installation{ID: 991, AccountLogin: "acme", AccountType: "Organization"}
	if err := f.st.UpsertInstallation(f.ctx, in); err != nil {
		f.t.Fatalf("seed installation: %v", err)
	}
}

func (f *webhookFixture) seedRepo(enabled bool) {
	f.t.Helper()
	f.seedInstallation()
	r := store.Repository{
		ID:             501,
		InstallationID: 991,
		FullName:       "acme/backend-api",
		DefaultBranch:  "main",
		Enabled:        enabled,
	}
	if err := f.st.UpsertRepository(f.ctx, r); err != nil {
		f.t.Fatalf("seed repository: %v", err)
	}
}

// seedRepoWithTeam mirrors an enabled, team-assigned repository: the normal
// precondition for a review to be started.
func (f *webhookFixture) seedRepoWithTeam() {
	f.t.Helper()
	f.seedRepo(true)
	team := store.Team{Name: "QA", Slug: "qa"}
	if err := f.st.DB.WithContext(f.ctx).Create(&team).Error; err != nil {
		f.t.Fatalf("seed team: %v", err)
	}
	if err := f.st.DB.WithContext(f.ctx).Model(&store.Repository{}).
		Where("id = ?", 501).Update("team_id", team.ID).Error; err != nil {
		f.t.Fatalf("assign repository team: %v", err)
	}
}

func (f *webhookFixture) pullRequest() store.PullRequest {
	f.t.Helper()
	var pr store.PullRequest
	if err := f.st.DB.WithContext(f.ctx).
		Where("repository_id = ? AND number = ?", 501, 142).Take(&pr).Error; err != nil {
		f.t.Fatalf("read pull request: %v", err)
	}
	return pr
}

func (f *webhookFixture) countRows(table string) int64 {
	f.t.Helper()
	var n int64
	if err := f.st.DB.WithContext(f.ctx).Table(table).Count(&n).Error; err != nil {
		f.t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	tampered := []byte(`{"action":"closed"}`)

	wrongSecretMAC := hmac.New(sha256.New, []byte("another-secret"))
	wrongSecretMAC.Write(body)
	wrongSecret := "sha256=" + hex.EncodeToString(wrongSecretMAC.Sum(nil))

	tests := []struct {
		name      string
		header    string
		body      []byte
		wantValid bool
	}{
		{name: "valid", header: signWebhook(body), body: body, wantValid: true},
		{name: "wrong secret", header: wrongSecret, body: body},
		{name: "tampered body", header: signWebhook(body), body: tampered},
		{name: "missing sha256 prefix", header: strings.TrimPrefix(signWebhook(body), "sha256="), body: body},
		{name: "invalid hex", header: "sha256=zzzz", body: body},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ghapp.VerifySignature([]byte(webhookTestSecret), tt.body, tt.header); got != tt.wantValid {
				t.Fatalf("VerifySignature() = %v, want %v", got, tt.wantValid)
			}
		})
	}
}

func TestWebhookBadSignature401(t *testing.T) {
	f := newWebhookFixture(t)
	body, err := os.ReadFile(filepath.Join("testdata", "installation-created.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	rec := f.deliverBody("installation", body, "sha256=deadbeef")
	assertStatus(t, rec, http.StatusUnauthorized)
	if got := f.countRows("installations"); got != 0 {
		t.Fatalf("installations after bad signature = %d, want 0", got)
	}
	if got := f.jobs.snapshot(); len(got) != 0 {
		t.Fatalf("EnqueueSync calls after bad signature = %v, want none", got)
	}
}

func TestWebhookInstallationCreated(t *testing.T) {
	f := newWebhookFixture(t)

	rec := f.deliver("installation", "installation-created.json")
	assertStatus(t, rec, http.StatusAccepted)

	var in store.Installation
	if err := f.st.DB.WithContext(f.ctx).Where("id = ?", 991).Take(&in).Error; err != nil {
		t.Fatalf("read installation: %v", err)
	}
	if in.AccountLogin != "acme" || in.AccountType != "Organization" {
		t.Errorf("installation = %+v, want acme/Organization", in)
	}
	if got := f.jobs.snapshot(); len(got) != 1 || got[0] != 991 {
		t.Fatalf("EnqueueSync calls = %v, want [991]", got)
	}
}

func TestWebhookPROpenedEnabledRepo(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepo(true)

	rec := f.deliver("pull_request", "pr-opened.json")
	assertStatus(t, rec, http.StatusAccepted)

	pr := f.pullRequest()
	if pr.QAStatus != store.StatusPending || pr.HeadSHA != "aaa111" || pr.Number != 142 {
		t.Fatalf("pull request = %+v, want pending/aaa111/#142", pr)
	}
	want := []events.Event{{Type: "pr.updated", ID: strconv.FormatUint(uint64(pr.ID), 10)}}
	if got := f.pub.snapshot(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("published events = %v, want %v", got, want)
	}
}

func TestWebhookReviewTriggeredForNonForkPR(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()

	assertStatus(t, f.deliver("pull_request", "pr-opened.json"), http.StatusAccepted)

	pr := f.pullRequest()
	if pr.IsFork || pr.HeadRepoFullName != "" {
		t.Fatalf("pull request = %+v, want a non-fork PR", pr)
	}
	if got := f.reviews.snapshot(); len(got) != 1 || got[0] != pr.ID {
		t.Fatalf("OnPullRequest calls = %v, want [%d]", got, pr.ID)
	}
	if got := f.pub.snapshot(); len(got) != 1 {
		t.Fatalf("published events = %v, want the pr.updated event", got)
	}

	// A redelivery reasserts the review intent; the trigger deduplicates it
	// against the run the first delivery queued. It must not be skipped just
	// because the PR sync itself was a no-op.
	assertStatus(t, f.deliver("pull_request", "pr-opened.json"), http.StatusAccepted)
	if got := f.reviews.snapshot(); len(got) != 2 || got[1] != pr.ID {
		t.Fatalf("OnPullRequest calls after redelivery = %v, want [%d %d]", got, pr.ID, pr.ID)
	}
}

func TestWebhookForkPRIsIgnored(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()

	const forkPRPayload = `{
		"action": "opened",
		"number": 142,
		"repository": {"id": 501, "name": "backend-api", "full_name": "acme/backend-api"},
		"pull_request": {
			"number": 142,
			"title": "Add QA endpoint from a fork",
			"user": {"login": "alice"},
			"author_association": "CONTRIBUTOR",
			"head": {
				"sha": "aaa111",
				"ref": "feature/qa",
				"repo": {"id": 777, "full_name": "ext/backend-api", "fork": true}
			},
			"base": {"ref": "main"},
			"html_url": "https://github.com/acme/backend-api/pull/142",
			"draft": false,
			"state": "open",
			"updated_at": "2026-01-01T00:00:00Z"
		}
	}`
	body := []byte(forkPRPayload)
	assertStatus(t, f.deliverBody("pull_request", body, signWebhook(body)), http.StatusAccepted)

	pr := f.pullRequest()
	if !pr.IsFork || pr.HeadRepoFullName != "ext/backend-api" {
		t.Fatalf("pull request = %+v, want IsFork=true, HeadRepoFullName=ext/backend-api", pr)
	}
	if got := f.reviews.snapshot(); len(got) != 0 {
		t.Fatalf("OnPullRequest calls for a fork PR = %v, want none", got)
	}
	if got := f.pub.snapshot(); len(got) != 0 {
		t.Fatalf("published events for a fork PR = %v, want none", got)
	}
	if got := f.jobs.snapshot(); len(got) != 0 {
		t.Fatalf("EnqueueSync calls for a fork PR = %v, want none", got)
	}
}

func TestWebhookReviewTriggerGate(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()

	// Drafts are synced but never trigger a review.
	assertStatus(t, f.deliver("pull_request", "pr-draft.json"), http.StatusAccepted)
	if got := f.reviews.snapshot(); len(got) != 0 {
		t.Fatalf("OnPullRequest calls for a draft = %v, want none", got)
	}

	// ready_for_review is a triggering action.
	assertStatus(t, f.deliver("pull_request", "pr-ready-for-review.json"), http.StatusAccepted)
	pr := f.pullRequest()
	if got := f.reviews.snapshot(); len(got) != 1 || got[0] != pr.ID {
		t.Fatalf("OnPullRequest calls after ready_for_review = %v, want [%d]", got, pr.ID)
	}

	// An edit that only changes metadata keeps the comparison and must not
	// schedule a review.
	metadata := []byte(`{"action":"edited","repository":{"id":501},"pull_request":{"number":142,"title":"Edited title","draft":false,"head":{"sha":"aaa111","repo":{"fork":false}},"base":{"ref":"main"},"updated_at":"2026-01-02T12:00:00Z"}}`)
	assertStatus(t, f.deliverBody("pull_request", metadata, signWebhook(metadata)), http.StatusAccepted)
	if got := f.reviews.snapshot(); len(got) != 1 {
		t.Fatalf("OnPullRequest calls for a metadata edit = %v, want one", got)
	}

	// A retarget changes the comparison even though the head is the same.
	retarget := []byte(`{"action":"edited","repository":{"id":501},"pull_request":{"number":142,"title":"Edited title","draft":false,"head":{"sha":"aaa111","repo":{"fork":false}},"base":{"ref":"release"},"updated_at":"2026-01-02T13:00:00Z"}}`)
	assertStatus(t, f.deliverBody("pull_request", retarget, signWebhook(retarget)), http.StatusAccepted)
	if got := f.reviews.snapshot(); len(got) != 2 {
		t.Fatalf("OnPullRequest calls after a retarget = %v, want two", got)
	}

	// A push keeps triggering as before.
	assertStatus(t, f.deliver("pull_request", "pr-synchronize-later.json"), http.StatusAccepted)
	if got := f.reviews.snapshot(); len(got) != 3 {
		t.Fatalf("OnPullRequest calls after synchronize = %v, want three", got)
	}
}

func TestWebhookReviewStartFailure500(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	f.reviews.err = errors.New("queue unavailable")

	assertStatus(t, f.deliver("pull_request", "pr-opened.json"), http.StatusInternalServerError)
	if got := f.reviews.snapshot(); len(got) != 1 {
		t.Fatalf("OnPullRequest calls = %v, want one attempt", got)
	}
}

func TestWebhookRedeliveryRetriesFailedTrigger(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	f.reviews.err = errors.New("queue unavailable")

	assertStatus(t, f.deliver("pull_request", "pr-opened.json"), http.StatusInternalServerError)
	if got := f.reviews.snapshot(); len(got) != 1 {
		t.Fatalf("OnPullRequest calls after the failure = %v, want one attempt", got)
	}

	// The sync is now a no-op, but the review intent was never completed: the
	// identical delivery must retry it instead of being skipped.
	f.reviews.err = nil
	assertStatus(t, f.deliver("pull_request", "pr-opened.json"), http.StatusAccepted)
	if got := f.reviews.snapshot(); len(got) != 2 {
		t.Fatalf("OnPullRequest calls after the redelivery = %v, want two", got)
	}
}

// A closed, merged or draft pull request may still have a queued run from an
// earlier delivery. The state conversion must reconcile it even when the PR
// sync is a no-op.
func TestWebhookIneligibleStateReconciles(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	assertStatus(t, f.deliver("pull_request", "pr-opened.json"), http.StatusAccepted)
	pr := f.pullRequest()

	closed := []byte(`{"action":"closed","repository":{"id":501},"pull_request":{"number":142,"title":"Add QA endpoint","user":{"login":"alice"},"head":{"sha":"aaa111","ref":"feature/qa","repo":{"id":501,"full_name":"acme/backend-api","fork":false}},"base":{"ref":"main"},"state":"closed","draft":false,"merged":true,"updated_at":"2026-01-02T12:00:00Z"}}`)
	assertStatus(t, f.deliverBody("pull_request", closed, signWebhook(closed)), http.StatusAccepted)
	if got := f.reviews.ineligibleSnapshot(); len(got) != 1 || got[0] != pr.ID {
		t.Fatalf("OnPullRequestIneligible calls after close = %v, want [%d]", got, pr.ID)
	}

	draft := []byte(`{"action":"converted_to_draft","repository":{"id":501},"pull_request":{"number":142,"title":"Add QA endpoint","user":{"login":"alice"},"head":{"sha":"aaa111","ref":"feature/qa","repo":{"id":501,"full_name":"acme/backend-api","fork":false}},"base":{"ref":"main"},"state":"open","draft":true,"updated_at":"2026-01-03T12:00:00Z"}}`)
	assertStatus(t, f.deliverBody("pull_request", draft, signWebhook(draft)), http.StatusAccepted)
	if got := f.reviews.ineligibleSnapshot(); len(got) != 2 {
		t.Fatalf("OnPullRequestIneligible calls after the draft conversion = %v, want two", got)
	}
}

func TestWebhookReviewsNilSafe(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	f.handler.Reviews = nil

	assertStatus(t, f.deliver("pull_request", "pr-opened.json"), http.StatusAccepted)
	if got := f.pub.snapshot(); len(got) != 1 {
		t.Fatalf("published events = %v, want the pr.updated event", got)
	}
}

func TestWebhookPRDisabledRepoIgnored(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepo(false)

	rec := f.deliver("pull_request", "pr-opened.json")
	assertStatus(t, rec, http.StatusNoContent)

	if got := f.countRows("pull_requests"); got != 0 {
		t.Fatalf("pull_requests = %d, want 0", got)
	}
	if got := f.pub.snapshot(); len(got) != 0 {
		t.Fatalf("published events = %v, want none", got)
	}
}

func TestWebhookOutOfOrder(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepo(true)

	assertStatus(t, f.deliver("pull_request", "pr-synchronize-later.json"), http.StatusAccepted)
	assertStatus(t, f.deliver("pull_request", "pr-opened-earlier.json"), http.StatusAccepted)

	pr := f.pullRequest()
	if pr.HeadSHA != "ccc333" {
		t.Errorf("HeadSHA = %q, want ccc333 from the later synchronize", pr.HeadSHA)
	}
	wantTime := time.Date(2026, time.January, 3, 0, 0, 0, 0, time.UTC)
	if !pr.GitHubUpdatedAt.Equal(wantTime) {
		t.Errorf("GitHubUpdatedAt = %v, want %v", pr.GitHubUpdatedAt, wantTime)
	}
	if got := f.pub.snapshot(); len(got) != 1 {
		t.Fatalf("published events = %v, want exactly one", got)
	}
}

func TestWebhookRedelivery(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepo(true)

	assertStatus(t, f.deliver("pull_request", "pr-synchronize-later.json"), http.StatusAccepted)
	assertStatus(t, f.deliver("pull_request", "pr-synchronize-later.json"), http.StatusAccepted)

	pr := f.pullRequest()
	if pr.HeadSHA != "ccc333" {
		t.Errorf("HeadSHA = %q, want ccc333", pr.HeadSHA)
	}
	if got := f.pub.snapshot(); len(got) != 1 {
		t.Fatalf("published events = %v, want exactly one for a redelivery", got)
	}
}

func TestWebhookDraftThenReady(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepo(true)

	assertStatus(t, f.deliver("pull_request", "pr-draft.json"), http.StatusAccepted)
	if got := f.pullRequest().QAStatus; got != store.StatusDraft {
		t.Fatalf("QA status after draft = %q, want %q", got, store.StatusDraft)
	}

	assertStatus(t, f.deliver("pull_request", "pr-ready-for-review.json"), http.StatusAccepted)
	if got := f.pullRequest().QAStatus; got != store.StatusPending {
		t.Fatalf("QA status after ready_for_review = %q, want %q", got, store.StatusPending)
	}
	if got := f.pub.snapshot(); len(got) != 2 {
		t.Fatalf("published events = %v, want two", got)
	}
}

func TestWebhookInstallationDeleted(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedInstallation()

	rec := f.deliver("installation", "installation-deleted.json")
	assertStatus(t, rec, http.StatusAccepted)

	var in store.Installation
	err := f.st.DB.WithContext(f.ctx).Where("id = ?", 991).Take(&in).Error
	if !errorsIsNotFound(err) {
		t.Fatalf("installation lookup error = %v, want record not found", err)
	}
}

func errorsIsNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }

func TestWebhookInstallationSuspendUnsuspend(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedInstallation()

	assertStatus(t, f.deliver("installation", "installation-suspended.json"), http.StatusAccepted)
	var in store.Installation
	if err := f.st.DB.WithContext(f.ctx).Where("id = ?", 991).Take(&in).Error; err != nil {
		t.Fatalf("read suspended installation: %v", err)
	}
	wantSuspended := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	if in.SuspendedAt == nil || !in.SuspendedAt.Equal(wantSuspended) {
		t.Fatalf("SuspendedAt = %v, want %v", in.SuspendedAt, wantSuspended)
	}
	if got := f.jobs.snapshot(); len(got) != 0 {
		t.Fatalf("EnqueueSync calls after suspend = %v, want none", got)
	}

	assertStatus(t, f.deliver("installation", "installation-unsuspended.json"), http.StatusAccepted)
	var unsuspended store.Installation
	if err := f.st.DB.WithContext(f.ctx).Where("id = ?", 991).Take(&unsuspended).Error; err != nil {
		t.Fatalf("read unsuspended installation: %v", err)
	}
	if unsuspended.SuspendedAt != nil {
		t.Fatalf("SuspendedAt = %v, want nil after unsuspend", unsuspended.SuspendedAt)
	}
	if got := f.jobs.snapshot(); len(got) != 1 || got[0] != 991 {
		t.Fatalf("EnqueueSync calls after unsuspend = %v, want [991]", got)
	}
}

func TestWebhookUnsuspendEnqueuesSync(t *testing.T) {
	f := newWebhookFixture(t)

	assertStatus(t, f.deliver("installation", "installation-unsuspended.json"), http.StatusAccepted)

	var in store.Installation
	if err := f.st.DB.WithContext(f.ctx).Where("id = ?", 991).Take(&in).Error; err != nil {
		t.Fatalf("read installation: %v", err)
	}
	if in.SuspendedAt != nil {
		t.Fatalf("SuspendedAt = %v, want nil", in.SuspendedAt)
	}
	if got := f.jobs.snapshot(); len(got) != 1 || got[0] != 991 {
		t.Fatalf("EnqueueSync calls = %v, want [991]", got)
	}
}

func TestWebhookUnsuspendEnqueueFailure500(t *testing.T) {
	f := newWebhookFixture(t)
	f.jobs.fail(errors.New("queue unavailable"))

	rec := f.deliver("installation", "installation-unsuspended.json")
	assertStatus(t, rec, http.StatusInternalServerError)

	var in store.Installation
	if err := f.st.DB.WithContext(f.ctx).Where("id = ?", 991).Take(&in).Error; err != nil {
		t.Fatalf("read installation: %v", err)
	}
	if in.SuspendedAt != nil {
		t.Fatalf("SuspendedAt = %v, want nil", in.SuspendedAt)
	}
	if got := f.jobs.snapshot(); len(got) != 1 || got[0] != 991 {
		t.Fatalf("EnqueueSync calls = %v, want one attempt for 991", got)
	}
}

func TestWebhookInstallationRepositoriesAddedRemoved(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedInstallation()

	assertStatus(t, f.deliver("installation_repositories", "installation-repositories-added.json"), http.StatusAccepted)
	var repo store.Repository
	if err := f.st.DB.WithContext(f.ctx).Where("id = ?", 501).Take(&repo).Error; err != nil {
		t.Fatalf("read added repository: %v", err)
	}
	if repo.FullName != "acme/backend-api" || repo.InstallationID != 991 || repo.Enabled {
		t.Fatalf("added repository = %+v, want acme/backend-api in installation 991, disabled", repo)
	}
	wantAdded := events.Event{Type: "repo.updated", ID: "501"}
	if got := f.pub.snapshot(); len(got) != 1 || got[0] != wantAdded {
		t.Fatalf("published events = %v, want %v", got, wantAdded)
	}

	assertStatus(t, f.deliver("installation_repositories", "installation-repositories-removed.json"), http.StatusAccepted)
	var stillThere store.Repository
	err := f.st.DB.WithContext(f.ctx).Where("id = ?", 501).Take(&stillThere).Error
	if !errorsIsNotFound(err) {
		t.Fatalf("repository lookup after removal = %v, want record not found", err)
	}
	if got := f.pub.snapshot(); len(got) != 2 || got[1] != wantAdded {
		t.Fatalf("published events = %v, want one repo.updated per change", got)
	}
}

func TestWebhookNotConfigured503(t *testing.T) {
	f := newWebhookFixture(t)
	f.handler.Apps = fakeAppProvider{err: store.ErrNotConfigured}

	rec := f.deliver("installation", "installation-created.json")
	assertStatus(t, rec, http.StatusServiceUnavailable)
	if got := f.countRows("installations"); got != 0 {
		t.Fatalf("installations = %d, want 0", got)
	}
}

func TestWebhookUnhandledEvents204(t *testing.T) {
	f := newWebhookFixture(t)
	body := []byte(`{}`)

	for _, eventType := range []string{
		"ping",
		"totally_unknown_event",
		"pull_request_review_comment",
		"issue_comment",
		"check_suite",
		"check_run",
		"status",
	} {
		t.Run(eventType, func(t *testing.T) {
			rec := f.deliverBody(eventType, body, signWebhook(body))
			assertStatus(t, rec, http.StatusNoContent)
		})
	}
	if got := f.pub.snapshot(); len(got) != 0 {
		t.Fatalf("published events = %v, want none", got)
	}
}

func TestWebhookPRUnknownRepoIgnored(t *testing.T) {
	f := newWebhookFixture(t)

	rec := f.deliver("pull_request", "pr-opened.json")
	assertStatus(t, rec, http.StatusNoContent)

	if got := f.countRows("pull_requests"); got != 0 {
		t.Fatalf("pull_requests = %d, want 0", got)
	}
	if got := f.pub.snapshot(); len(got) != 0 {
		t.Fatalf("published events = %v, want none", got)
	}
}

func TestWebhookUnhandledActions204(t *testing.T) {
	f := newWebhookFixture(t)
	tests := []struct {
		name      string
		eventType string
		body      string
	}{
		{
			name:      "installation new_permissions_accepted",
			eventType: "installation",
			body:      `{"action":"new_permissions_accepted","installation":{"id":991}}`,
		},
		{
			name:      "pull_request labeled",
			eventType: "pull_request",
			body:      `{"action":"labeled","repository":{"id":501},"pull_request":{"number":142}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			rec := f.deliverBody(tt.eventType, body, signWebhook(body))
			assertStatus(t, rec, http.StatusNoContent)
		})
	}
	if got := f.countRows("installations"); got != 0 {
		t.Fatalf("installations = %d, want 0", got)
	}
	if got := f.jobs.snapshot(); len(got) != 0 {
		t.Fatalf("EnqueueSync calls = %v, want none", got)
	}
	if got := f.pub.snapshot(); len(got) != 0 {
		t.Fatalf("published events = %v, want none", got)
	}
}

func TestWebhookOversizedBody413(t *testing.T) {
	f := newWebhookFixture(t)
	body := bytes.Repeat([]byte("a"), (25<<20)+1)

	rec := f.deliverBody("pull_request", body, signWebhook(body))
	assertStatus(t, rec, http.StatusRequestEntityTooLarge)
	if got := f.pub.snapshot(); len(got) != 0 {
		t.Fatalf("published events = %v, want none", got)
	}
}

// seedOpenPR stores the pull request the review-thread and command fixtures
// refer to (#142 of repository 501).
func (f *webhookFixture) seedOpenPR() store.PullRequest {
	f.t.Helper()

	pr := store.PullRequest{
		RepositoryID:    501,
		Number:          142,
		Title:           "Add QA endpoint",
		HeadSHA:         "aaa111",
		State:           "open",
		QAStatus:        store.StatusChangesRequested,
		GitHubUpdatedAt: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := f.st.DB.WithContext(f.ctx).Create(&pr).Error; err != nil {
		f.t.Fatalf("seed pull request: %v", err)
	}
	return pr
}

// seedThreadFinding stores a published finding linked to the review comment the
// thread fixtures describe. The first finding keeps the fixtures' coordinates;
// later ones get their own so several findings never share a thread.
func (f *webhookFixture) seedThreadFinding(prID uint, id, status string) store.Finding {
	f.t.Helper()

	var existing int64
	if err := f.st.DB.WithContext(f.ctx).Model(&store.Finding{}).
		Where("pull_request_id = ?", prID).Count(&existing).Error; err != nil {
		f.t.Fatalf("count findings: %v", err)
	}
	commentID, nodeID := 5001, "PRRC_root1"
	if existing > 0 {
		commentID, nodeID = 5100+int(existing), "PRRC_"+id
	}

	saved, err := f.st.UpsertFinding(f.ctx, store.Finding{
		ID:                  id,
		PullRequestID:       prID,
		Fingerprint:         "fp-" + id,
		Path:                "src/a.go",
		StartLine:           2,
		EndLine:             2,
		Severity:            "major",
		Category:            "bug",
		Title:               "Título " + id,
		Status:              status,
		GithubCommentID:     int64(commentID),
		GithubCommentNodeID: nodeID,
	})
	if err != nil {
		f.t.Fatalf("seed finding %s: %v", id, err)
	}
	return saved
}

func (f *webhookFixture) finding(id string) store.Finding {
	f.t.Helper()

	var out store.Finding
	if err := f.st.DB.WithContext(f.ctx).Where("id = ?", id).Take(&out).Error; err != nil {
		f.t.Fatalf("read finding %s: %v", id, err)
	}
	return out
}

func (f *webhookFixture) auditRows() []store.AuditLog {
	f.t.Helper()

	var rows []store.AuditLog
	if err := f.st.DB.WithContext(f.ctx).Order("id").Find(&rows).Error; err != nil {
		f.t.Fatalf("list audit rows: %v", err)
	}
	return rows
}

// seedQALead creates the local user behind the authorized fixtures and makes
// them lead of the repository's team.
func (f *webhookFixture) seedQALead(teamID uint) store.User {
	f.t.Helper()

	user := store.User{GitHubID: 8001, Login: "qa-lead"}
	if err := f.st.DB.WithContext(f.ctx).Create(&user).Error; err != nil {
		f.t.Fatalf("seed lead user: %v", err)
	}
	if err := f.st.SetTeamMember(f.ctx, teamID, user.ID, "lead"); err != nil {
		f.t.Fatalf("assign team lead: %v", err)
	}
	return user
}

func (f *webhookFixture) seedDeveloper() store.User {
	f.t.Helper()

	user := store.User{GitHubID: 8002, Login: "dev"}
	if err := f.st.DB.WithContext(f.ctx).Create(&user).Error; err != nil {
		f.t.Fatalf("seed developer user: %v", err)
	}
	return user
}

func (f *webhookFixture) repoTeam() uint {
	f.t.Helper()

	var repo store.Repository
	if err := f.st.DB.WithContext(f.ctx).Where("id = ?", 501).Take(&repo).Error; err != nil {
		f.t.Fatalf("read repository: %v", err)
	}
	if repo.TeamID == nil {
		f.t.Fatal("repository has no team")
	}
	return *repo.TeamID
}
