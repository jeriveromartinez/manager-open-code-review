package publish_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v93/github"

	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/publish"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

type fakeAppProvider struct{ app *ghapp.App }

func (f fakeAppProvider) App(context.Context) (*ghapp.App, error) { return f.app, nil }

func testPEM(t *testing.T) string {
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

func seedRun(t *testing.T, st *store.Store) (store.ReviewRun, store.PullRequest, store.Repository) {
	t.Helper()

	ctx := context.Background()
	inst := store.Installation{ID: 9, AccountLogin: "acme", AccountType: "Organization"}
	if err := st.DB.WithContext(ctx).Create(&inst).Error; err != nil {
		t.Fatalf("create installation: %v", err)
	}
	repo := store.Repository{ID: 42, InstallationID: inst.ID, FullName: "acme/repo", Enabled: true}
	if err := st.DB.WithContext(ctx).Create(&repo).Error; err != nil {
		t.Fatalf("create repository: %v", err)
	}
	pr := store.PullRequest{
		RepositoryID:    repo.ID,
		Number:          7,
		HeadSHA:         "abc123",
		HeadRef:         "feature/qa",
		GitHubUpdatedAt: time.Now().UTC(),
	}
	if err := st.DB.WithContext(ctx).Create(&pr).Error; err != nil {
		t.Fatalf("create pull request: %v", err)
	}
	run := store.ReviewRun{PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, Status: store.RunQueued}
	if err := st.DB.WithContext(ctx).Create(&run).Error; err != nil {
		t.Fatalf("create review run: %v", err)
	}
	return run, pr, repo
}

// installationAPI answers the token request ghinstallation makes before every
// check-run call, and fails any unexpected path.
func installationAPI(handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/9/access_tokens" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"token":"ghs_inst","expires_at":%q}`,
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			return
		}
		handler(w, r)
	}))
}

func TestStartCheckCreatesInProgress(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr, _ := seedRun(t, st)

	var (
		mu        sync.Mutex
		gotMethod string
		gotPath   string
		gotAuth   string
		gotCheck  gh.CreateCheckRunOptions
	)
	srv := installationAPI(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_ = json.Unmarshal(body, &gotCheck)
		mu.Unlock()

		if r.URL.Path != "/repos/acme/repo/check-runs" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":555,"html_url":"https://github.com/acme/repo/runs/555"}`)
	})
	defer srv.Close()

	w := &publish.Worker{
		Store: st,
		Apps:  fakeAppProvider{app: &ghapp.App{Creds: ghapp.AppCredentials{AppID: 12345, PEM: testPEM(t)}, APIURL: srv.URL}},
		Box:   &crypto.Box{},
	}
	if err := w.StartCheck(ctx, run.ID); err != nil {
		t.Fatalf("StartCheck() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/repos/acme/repo/check-runs" {
		t.Errorf("path = %q, want /repos/acme/repo/check-runs", gotPath)
	}
	if gotAuth == "" {
		t.Error("Authorization header is missing")
	}
	if gotCheck.Name != "QA Control Center" {
		t.Errorf("check name = %q, want QA Control Center", gotCheck.Name)
	}
	if gotCheck.HeadSHA != run.HeadSHA {
		t.Errorf("head_sha = %q, want %q", gotCheck.HeadSHA, run.HeadSHA)
	}
	if gotCheck.Status == nil || *gotCheck.Status != "in_progress" {
		t.Errorf("status = %v, want in_progress", gotCheck.Status)
	}
	if want := strconv.FormatUint(uint64(run.ID), 10); gotCheck.ExternalID == nil || *gotCheck.ExternalID != want {
		t.Errorf("external_id = %v, want %q", gotCheck.ExternalID, want)
	}

	artifact, err := st.ArtifactFor(ctx, pr.ID, store.ArtifactCheck, run.HeadSHA)
	if err != nil {
		t.Fatalf("ArtifactFor() error = %v", err)
	}
	if artifact.ExternalID != "555" {
		t.Errorf("artifact external_id = %q, want 555", artifact.ExternalID)
	}
	if artifact.URL != "https://github.com/acme/repo/runs/555" {
		t.Errorf("artifact url = %q, want the check html_url", artifact.URL)
	}
}

func TestStartCheckReusesExistingCheck(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr, _ := seedRun(t, st)
	if _, err := st.UpsertArtifact(ctx, store.GitHubArtifact{
		PullRequestID: pr.ID,
		Kind:          store.ArtifactCheck,
		HeadSHA:       run.HeadSHA,
		ExternalID:    "555",
		URL:           "https://github.com/acme/repo/runs/555",
	}); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}

	var (
		mu        sync.Mutex
		gotMethod string
		gotPath   string
		gotStatus string
		postCount int
	)
	srv := installationAPI(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotMethod, gotPath = r.Method, r.URL.Path
		if r.Method == http.MethodPost {
			postCount++
		}
		var payload struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(body, &payload)
		gotStatus = payload.Status
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":555,"html_url":"https://github.com/acme/repo/runs/555"}`)
	})
	defer srv.Close()

	w := &publish.Worker{
		Store: st,
		Apps:  fakeAppProvider{app: &ghapp.App{Creds: ghapp.AppCredentials{AppID: 12345, PEM: testPEM(t)}, APIURL: srv.URL}},
	}
	if err := w.StartCheck(ctx, run.ID); err != nil {
		t.Fatalf("StartCheck() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if postCount != 0 {
		t.Errorf("POST count = %d, want 0 when an artifact already exists", postCount)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", gotMethod)
	}
	if gotPath != "/repos/acme/repo/check-runs/555" {
		t.Errorf("path = %q, want /repos/acme/repo/check-runs/555", gotPath)
	}
	if gotStatus != "in_progress" {
		t.Errorf("status = %q, want in_progress", gotStatus)
	}
	artifact, err := st.ArtifactFor(ctx, pr.ID, store.ArtifactCheck, run.HeadSHA)
	if err != nil {
		t.Fatalf("ArtifactFor() error = %v", err)
	}
	if artifact.ExternalID != "555" {
		t.Errorf("artifact external_id = %q, want 555", artifact.ExternalID)
	}
}

func TestCancelCheckMarksCancelled(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr, _ := seedRun(t, st)
	if _, err := st.UpsertArtifact(ctx, store.GitHubArtifact{
		PullRequestID: pr.ID,
		Kind:          store.ArtifactCheck,
		HeadSHA:       run.HeadSHA,
		ExternalID:    "555",
		URL:           "https://github.com/acme/repo/runs/555",
	}); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}

	var (
		mu        sync.Mutex
		gotBody   string
		gotPath   string
		gotAuth   string
		gotMethod string
	)
	srv := installationAPI(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotMethod, gotPath, gotAuth, gotBody = r.Method, r.URL.Path, r.Header.Get("Authorization"), string(body)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":555,"status":"completed","conclusion":"cancelled"}`)
	})
	defer srv.Close()

	w := &publish.Worker{
		Store: st,
		Apps:  fakeAppProvider{app: &ghapp.App{Creds: ghapp.AppCredentials{AppID: 12345, PEM: testPEM(t)}, APIURL: srv.URL}},
	}
	if err := w.CancelCheck(ctx, run.ID); err != nil {
		t.Fatalf("CancelCheck() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotMethod != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", gotMethod)
	}
	if gotPath != "/repos/acme/repo/check-runs/555" {
		t.Errorf("path = %q, want /repos/acme/repo/check-runs/555", gotPath)
	}
	if gotAuth == "" {
		t.Error("Authorization header is missing")
	}
	if gotBody != `{"status":"completed","conclusion":"cancelled"}` {
		t.Errorf("body = %q, want the completed/cancelled payload", gotBody)
	}
}

// A run queued before StartCheck could create its check has no artifact to
// cancel. That must not fail the trigger that supersedes the orphan run.
func TestCancelCheckWithoutArtifactIsNoop(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, _, _ := seedRun(t, st)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.NotFound(w, r)
	}))
	defer srv.Close()

	w := &publish.Worker{
		Store: st,
		Apps:  fakeAppProvider{app: &ghapp.App{Creds: ghapp.AppCredentials{AppID: 12345, PEM: testPEM(t)}, APIURL: srv.URL}},
	}
	if err := w.CancelCheck(ctx, run.ID); err != nil {
		t.Fatalf("CancelCheck() error = %v, want nil for a missing check", err)
	}
	if calls != 0 {
		t.Errorf("GitHub requests = %d, want none", calls)
	}
}
