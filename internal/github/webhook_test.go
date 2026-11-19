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
}

func (j *recordingJobs) EnqueueSync(_ context.Context, id int64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.ids = append(j.ids, id)
	return nil
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

type webhookFixture struct {
	t       *testing.T
	ctx     context.Context
	st      *store.Store
	handler *ghapp.WebhookHandler
	jobs    *recordingJobs
	pub     *recordingPublisher
}

func newWebhookFixture(t *testing.T) *webhookFixture {
	t.Helper()

	st := testutil.NewStore(t)
	jobs := &recordingJobs{}
	pub := &recordingPublisher{}
	h := &ghapp.WebhookHandler{
		Store: st,
		Apps: fakeAppProvider{app: &ghapp.App{
			Creds: ghapp.AppCredentials{WebhookSecret: webhookTestSecret},
		}},
		Events: pub,
		Jobs:   jobs,
	}
	return &webhookFixture{t: t, ctx: context.Background(), st: st, handler: h, jobs: jobs, pub: pub}
}

func signWebhook(body []byte) string {
	mac := hmac.New(sha256.New, []byte(webhookTestSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func (f *webhookFixture) deliver(eventType, fixture string) *httptest.ResponseRecorder {
	f.t.Helper()

	body, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		f.t.Fatalf("read fixture %q: %v", fixture, err)
	}
	return f.deliverBody(eventType, body, signWebhook(body))
}

func (f *webhookFixture) deliverBody(eventType string, body []byte, signature string) *httptest.ResponseRecorder {
	f.t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", eventType)
	req.Header.Set("X-Hub-Signature-256", signature)
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

	assertStatus(t, f.deliver("installation", "installation-unsuspended.json"), http.StatusAccepted)
	var unsuspended store.Installation
	if err := f.st.DB.WithContext(f.ctx).Where("id = ?", 991).Take(&unsuspended).Error; err != nil {
		t.Fatalf("read unsuspended installation: %v", err)
	}
	if unsuspended.SuspendedAt != nil {
		t.Fatalf("SuspendedAt = %v, want nil after unsuspend", unsuspended.SuspendedAt)
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
