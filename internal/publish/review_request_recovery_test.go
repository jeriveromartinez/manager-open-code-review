package publish_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/publish"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

// The publisher heals a review request whose send was interrupted. It must
// reconcile with GitHub before resending, so an accepted request is not
// duplicated.
func TestWorkerReconcileReviewRequestsHealsStrandedSend(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()

	var posts, gets atomic.Int32
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
			gets.Add(1)
			_, _ = w.Write([]byte(`{"users":[{"login":"qa-reviewer"}],"teams":[]}`))
		case http.MethodPost:
			posts.Add(1)
			http.Error(w, "duplicate request", http.StatusUnprocessableEntity)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	team := store.Team{Name: "qa", Slug: "qa"}
	profile := store.LLMProfile{Name: "default", Provider: "openai", Model: "gpt-5", Mode: "api_key", Enabled: true}
	inst := store.Installation{ID: 1, AccountLogin: "acme", AccountType: "Organization"}
	for _, row := range []any{&team, &profile, &inst} {
		if err := st.DB.WithContext(ctx).Create(row).Error; err != nil {
			t.Fatalf("seed %T: %v", row, err)
		}
	}
	policy := store.QAPolicy{TeamID: team.ID, LLMProfileID: profile.ID, ReviewerLogin: "qa-reviewer"}
	if err := st.DB.WithContext(ctx).Create(&policy).Error; err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	repo := store.Repository{
		ID: 1, InstallationID: inst.ID, FullName: "acme/repo", Enabled: true, TeamID: &team.ID,
	}
	if err := st.DB.WithContext(ctx).Create(&repo).Error; err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	pr := store.PullRequest{
		RepositoryID: repo.ID, Number: 7, HeadSHA: "head", BaseRef: "main",
		MergeBaseSHA: "mb", State: "open", GitHubUpdatedAt: time.Now().UTC(),
	}
	if err := st.DB.WithContext(ctx).Create(&pr).Error; err != nil {
		t.Fatalf("seed pull request: %v", err)
	}
	_, request, err := st.CreateReviewRequest(ctx, store.ReviewRequest{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, BaseRef: pr.BaseRef, MergeBaseSHA: pr.MergeBaseSHA,
		Cause: store.ReviewRequestCauseInconclusive, ReviewerLogin: "qa-reviewer",
		Status: store.ReviewRequestSending, Attempts: 1,
	})
	if err != nil {
		t.Fatalf("seed sending request: %v", err)
	}
	if err := st.DB.WithContext(ctx).Model(&store.ReviewRequest{}).Where("id = ?", request.ID).
		Update("updated_at", time.Now().Add(-2*store.ReviewRequestClaimLease)).Error; err != nil {
		t.Fatalf("age the sending row: %v", err)
	}

	app := &ghapp.App{Creds: ghapp.AppCredentials{AppID: 12345, PEM: testPEM(t)}, APIURL: srv.URL}
	worker := &publish.Worker{Store: st, Apps: fakeAppProvider{app: app}}
	resumed, err := worker.ReconcileReviewRequests(ctx, 10)
	if err != nil {
		t.Fatalf("ReconcileReviewRequests() error = %v", err)
	}
	if resumed != 1 {
		t.Errorf("resumed = %d, want 1", resumed)
	}
	saved, err := st.ReviewRequestFor(ctx, pr.ID, pr.HeadSHA)
	if err != nil {
		t.Fatalf("ReviewRequestFor: %v", err)
	}
	if saved.Status != store.ReviewRequestDelivered {
		t.Errorf("status = %q, want delivered", saved.Status)
	}
	if gets.Load() == 0 {
		t.Error("GET requested_reviewers was never called")
	}
	if posts.Load() != 0 {
		t.Errorf("POST calls = %d, want 0: GitHub already had the reviewer", posts.Load())
	}
}
