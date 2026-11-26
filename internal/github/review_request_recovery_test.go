package github_test

import (
	"context"
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

func seedRecoveryPullRequest(t *testing.T) (*store.Store, store.PullRequest) {
	t.Helper()
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
		RepositoryID: 1, Number: 7, HeadSHA: "head", BaseRef: "main",
		MergeBaseSHA: "mb", State: "open", GitHubUpdatedAt: time.Now().UTC(),
	}
	if err := st.DB.WithContext(ctx).Create(&pr).Error; err != nil {
		t.Fatalf("seed pull request: %v", err)
	}
	return st, pr
}

// serveReviewRequestToken answers the installation token calls every GitHub
// client makes before reaching the review endpoint.
func serveReviewRequestToken(t *testing.T, w http.ResponseWriter, r *http.Request) bool {
	t.Helper()
	if r.Method != http.MethodPost || r.URL.Path != "/app/installations/1/access_tokens" {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"token":"[SECRET]","expires_at":%q}`,
		time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	return true
}

func recoveryConfig(pr store.PullRequest) store.ReviewRequestConfig {
	return store.ReviewRequestConfig{
		PR:           pr,
		Repo:         store.Repository{ID: 1, FullName: "acme/repo"},
		Policy:       store.QAPolicy{ReviewerLogin: "qa-reviewer"},
		Installation: store.Installation{ID: 1},
	}
}

// A POST whose response was lost may have been accepted by GitHub. The retry
// must reconcile with GET requested_reviewers instead of posting again.
func TestRequestReconcilesAmbiguousSendBeforeResending(t *testing.T) {
	st, pr := seedRecoveryPullRequest(t)
	var posts, gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveReviewRequestToken(t, w, r) {
			return
		}
		if r.URL.Path != "/repos/acme/repo/pulls/7/requested_reviewers" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			gets.Add(1)
			_, _ = w.Write([]byte(`{"users":[{"login":"qa-reviewer"}],"teams":[]}`))
		case http.MethodPost:
			posts.Add(1)
			http.Error(w, "upstream failed after accepting", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	app := &ghapp.App{Creds: ghapp.AppCredentials{AppID: 12345, PEM: testAppPEM(t)}, APIURL: srv.URL}
	service := &ghapp.ReviewRequestService{Store: st, Apps: fakeAppProvider{app: app}, RetryDelay: 0}
	got, err := service.Request(context.Background(), recoveryConfig(pr), store.ReviewRequestCauseInconclusive)
	if err != nil {
		t.Fatalf("Request() error = %v", err)
	}
	if got.Status != store.ReviewRequestDelivered {
		t.Errorf("status = %q, want delivered", got.Status)
	}
	if posts.Load() != 1 {
		t.Errorf("POST calls = %d, want 1: the accepted request must not be sent again", posts.Load())
	}
	if gets.Load() == 0 {
		t.Error("GET requested_reviewers was never called, so the ambiguous POST was not reconciled")
	}
}

// A rejected request whose destination was corrected must be retried instead of
// staying terminal forever.
func TestRequestRecoversRejectedAfterRecipientFixed(t *testing.T) {
	st, pr := seedRecoveryPullRequest(t)
	ctx := context.Background()
	if _, _, err := st.CreateReviewRequest(ctx, store.ReviewRequest{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, BaseRef: pr.BaseRef, MergeBaseSHA: pr.MergeBaseSHA,
		Cause: store.ReviewRequestCauseInconclusive, Status: store.ReviewRequestRejected,
		Attempts: 1, LastError: "no reviewer configured",
	}); err != nil {
		t.Fatalf("seed rejected request: %v", err)
	}
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveReviewRequestToken(t, w, r) {
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

	app := &ghapp.App{Creds: ghapp.AppCredentials{AppID: 12345, PEM: testAppPEM(t)}, APIURL: srv.URL}
	service := &ghapp.ReviewRequestService{Store: st, Apps: fakeAppProvider{app: app}, RetryDelay: 0}
	got, err := service.Request(ctx, recoveryConfig(pr), store.ReviewRequestCauseInconclusive)
	if err != nil {
		t.Fatalf("Request() error = %v", err)
	}
	if got.Status != store.ReviewRequestDelivered {
		t.Errorf("status = %q, want delivered after the destination was fixed", got.Status)
	}
	if posts.Load() != 1 {
		t.Errorf("POST calls = %d, want 1", posts.Load())
	}
	saved, err := st.ReviewRequestFor(ctx, pr.ID, pr.HeadSHA)
	if err != nil {
		t.Fatalf("ReviewRequestFor: %v", err)
	}
	if saved.ReviewerLogin != "qa-reviewer" || saved.LastError != "" {
		t.Errorf("saved request = %+v, want the fixed reviewer and no error", saved)
	}
}

// A sending row whose lease expired has an unknown outcome. Reconciliation must
// find the reviewer GitHub already holds instead of sending a duplicate.
func TestRequestResumesStrandedSendingWithoutDuplicate(t *testing.T) {
	st, pr := seedRecoveryPullRequest(t)
	ctx := context.Background()
	_, seeded, err := st.CreateReviewRequest(ctx, store.ReviewRequest{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, BaseRef: pr.BaseRef, MergeBaseSHA: pr.MergeBaseSHA,
		Cause: store.ReviewRequestCauseInconclusive, ReviewerLogin: "qa-reviewer",
		Status: store.ReviewRequestSending, Attempts: 1,
	})
	if err != nil {
		t.Fatalf("seed sending request: %v", err)
	}
	if err := st.DB.WithContext(ctx).Model(&store.ReviewRequest{}).Where("id = ?", seeded.ID).
		Update("updated_at", time.Now().Add(-2*store.ReviewRequestClaimLease)).Error; err != nil {
		t.Fatalf("age the sending row: %v", err)
	}
	var posts, gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveReviewRequestToken(t, w, r) {
			return
		}
		if r.URL.Path != "/repos/acme/repo/pulls/7/requested_reviewers" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			gets.Add(1)
			_, _ = w.Write([]byte(`{"users":[{"login":"qa-reviewer"}],"teams":[]}`))
		case http.MethodPost:
			posts.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"number":7}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	app := &ghapp.App{Creds: ghapp.AppCredentials{AppID: 12345, PEM: testAppPEM(t)}, APIURL: srv.URL}
	service := &ghapp.ReviewRequestService{Store: st, Apps: fakeAppProvider{app: app}, RetryDelay: 0}
	got, err := service.Request(ctx, recoveryConfig(pr), store.ReviewRequestCauseInconclusive)
	if err != nil {
		t.Fatalf("Request() error = %v", err)
	}
	if got.Status != store.ReviewRequestDelivered {
		t.Errorf("status = %q, want delivered", got.Status)
	}
	if posts.Load() != 0 {
		t.Errorf("POST calls = %d, want 0: GitHub already had the reviewer", posts.Load())
	}
	if gets.Load() == 0 {
		t.Error("GET requested_reviewers was never called")
	}
}

// A delivered request for the same comparison is final: a redelivery, a
// retried publish or a second fork event must not call GitHub again.
func TestRequestDoesNotRepeatDeliveredComparison(t *testing.T) {
	st, pr := seedRecoveryPullRequest(t)
	ctx := context.Background()
	if _, _, err := st.CreateReviewRequest(ctx, store.ReviewRequest{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, BaseRef: pr.BaseRef, MergeBaseSHA: pr.MergeBaseSHA,
		Cause: store.ReviewRequestCauseInconclusive, ReviewerLogin: "qa-reviewer",
		Status: store.ReviewRequestDelivered, Attempts: 1,
	}); err != nil {
		t.Fatalf("seed delivered request: %v", err)
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveReviewRequestToken(t, w, r) {
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":7}`))
	}))
	t.Cleanup(srv.Close)

	app := &ghapp.App{Creds: ghapp.AppCredentials{AppID: 12345, PEM: testAppPEM(t)}, APIURL: srv.URL}
	service := &ghapp.ReviewRequestService{Store: st, Apps: fakeAppProvider{app: app}, RetryDelay: 0}
	got, err := service.Request(ctx, recoveryConfig(pr), store.ReviewRequestCauseInconclusive)
	if err != nil {
		t.Fatalf("Request() error = %v", err)
	}
	if got.Status != store.ReviewRequestDelivered {
		t.Errorf("status = %q, want delivered", got.Status)
	}
	if calls.Load() != 0 {
		t.Errorf("GitHub calls = %d, want 0 for a delivered comparison", calls.Load())
	}
}
