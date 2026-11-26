package publish

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

type reviewRecoveryApps struct{ app *ghapp.App }

func (p reviewRecoveryApps) App(context.Context) (*ghapp.App, error) { return p.app, nil }

func reviewRecoveryPEM(t *testing.T) string {
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

// The publication guard already drops a stale run, but requestHumanReview runs
// after a long publication and reloads the pull request, so it must apply the
// same comparison check on its own.
func TestRequestHumanReviewSkipsStaleComparison(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	if err := st.DB.WithContext(ctx).Create(&store.Installation{
		ID: 1, AccountLogin: "acme", AccountType: "Organization",
	}).Error; err != nil {
		t.Fatalf("seed installation: %v", err)
	}
	if err := st.DB.WithContext(ctx).Create(&store.Repository{
		ID: 1, InstallationID: 1, FullName: "acme/repo", Enabled: true,
	}).Error; err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	pr := store.PullRequest{
		RepositoryID: 1, Number: 7, HeadSHA: "head-sha", BaseRef: "main",
		MergeBaseSHA: "new-merge-base", State: "open", GitHubUpdatedAt: time.Now().UTC(),
	}
	if err := st.DB.WithContext(ctx).Create(&pr).Error; err != nil {
		t.Fatalf("seed pull request: %v", err)
	}
	run := store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: "head-sha", BaseRef: "main",
		BaseSHA: "old-merge-base", Status: store.RunInconclusive,
	}
	p := &publishContext{w: &Worker{Store: st}, pr: pr, run: run}

	if err := p.requestHumanReview(ctx, store.ReviewRequestCauseInconclusive); err != nil {
		t.Fatalf("requestHumanReview() error = %v", err)
	}
	if _, err := st.ReviewRequestFor(ctx, pr.ID, run.HeadSHA); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ReviewRequestFor() error = %v, want ErrNotFound for a stale comparison", err)
	}
}

// A retarget that keeps the head must ask for a review again: the delivered
// request of the previous base is not a confirmation of the new comparison.
func TestRequestHumanReviewRequestsAgainAfterRetarget(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	if err := st.DB.WithContext(ctx).Create(&store.Installation{
		ID: 1, AccountLogin: "acme", AccountType: "Organization",
	}).Error; err != nil {
		t.Fatalf("seed installation: %v", err)
	}
	if err := st.DB.WithContext(ctx).Create(&store.Repository{
		ID: 1, InstallationID: 1, FullName: "acme/repo", Enabled: true,
	}).Error; err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	pr := store.PullRequest{
		RepositoryID: 1, Number: 7, HeadSHA: "head-sha", BaseRef: "main",
		MergeBaseSHA: "mb-new", State: "open", GitHubUpdatedAt: time.Now().UTC(),
	}
	if err := st.DB.WithContext(ctx).Create(&pr).Error; err != nil {
		t.Fatalf("seed pull request: %v", err)
	}
	if _, _, err := st.CreateReviewRequest(ctx, store.ReviewRequest{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, BaseRef: "main", MergeBaseSHA: "mb-old",
		Cause: store.ReviewRequestCauseInconclusive, ReviewerLogin: "qa-reviewer",
		Status: store.ReviewRequestDelivered,
	}); err != nil {
		t.Fatalf("seed previous-base request: %v", err)
	}

	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/app/installations/1/access_tokens" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"token":"[SECRET]","expires_at":%q}`,
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			return
		}
		if r.URL.Path != "/repos/acme/repo/pulls/7/requested_reviewers" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"users":[],"teams":[]}`))
		case http.MethodPost:
			posts.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"number":7}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	app := &ghapp.App{Creds: ghapp.AppCredentials{AppID: 12345, PEM: reviewRecoveryPEM(t)}, APIURL: srv.URL}
	run := store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, BaseRef: "main",
		BaseSHA: "mb-new", Status: store.RunInconclusive,
	}
	p := &publishContext{
		w:      &Worker{Store: st, Apps: reviewRecoveryApps{app: app}},
		pr:     pr,
		run:    run,
		repo:   store.Repository{ID: 1, FullName: "acme/repo"},
		policy: store.QAPolicy{ReviewerLogin: "qa-reviewer"},
		inst:   store.Installation{ID: 1},
	}
	if err := p.requestHumanReview(ctx, store.ReviewRequestCauseInconclusive); err != nil {
		t.Fatalf("requestHumanReview() error = %v", err)
	}

	saved, err := st.ReviewRequestFor(ctx, pr.ID, pr.HeadSHA)
	if err != nil {
		t.Fatalf("ReviewRequestFor: %v", err)
	}
	if saved.MergeBaseSHA != "mb-new" || saved.Status != store.ReviewRequestDelivered {
		t.Errorf("request = %+v, want a delivered request for the new merge base", saved)
	}
	if posts.Load() != 1 {
		t.Errorf("POST calls = %d, want 1 for the new comparison", posts.Load())
	}
}
