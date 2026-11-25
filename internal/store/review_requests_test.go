package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

// seedReviewRequestFixture creates the installation, repository and open pull
// request every review-request test needs.
func seedReviewRequestFixture(t *testing.T) (*store.Store, store.PullRequest) {
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
		RepositoryID: 1, Number: 7, HeadSHA: "head", State: "open",
		GitHubUpdatedAt: time.Now().UTC(),
	}
	if err := st.DB.WithContext(ctx).Create(&pr).Error; err != nil {
		t.Fatalf("seed pull request: %v", err)
	}
	return st, pr
}

// Two concurrent deliveries must not both call GitHub for the same revision.
func TestClaimReviewRequestIsExclusive(t *testing.T) {
	st, pr := seedReviewRequestFixture(t)
	ctx := context.Background()
	_, req, err := st.CreateReviewRequest(ctx, store.ReviewRequest{
		PullRequestID: pr.ID, HeadSHA: "head", Status: store.ReviewRequestPending,
	})
	if err != nil {
		t.Fatalf("CreateReviewRequest: %v", err)
	}

	var mu sync.Mutex
	claimed := 0
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := st.ClaimReviewRequest(ctx, req.ID, 3)
			if err != nil {
				t.Errorf("ClaimReviewRequest: %v", err)
				return
			}
			if ok {
				mu.Lock()
				claimed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if claimed != 1 {
		t.Fatalf("successful claims = %d, want exactly 1", claimed)
	}
}

// A delivery that crashed while sending must be reclaimable once its lease
// expires, instead of stranding the request.
func TestClaimReviewRequestReclaimsStaleSending(t *testing.T) {
	st, pr := seedReviewRequestFixture(t)
	ctx := context.Background()
	_, req, err := st.CreateReviewRequest(ctx, store.ReviewRequest{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, Status: store.ReviewRequestPending,
	})
	if err != nil {
		t.Fatalf("CreateReviewRequest: %v", err)
	}
	if _, ok, err := st.ClaimReviewRequest(ctx, req.ID, 3); err != nil || !ok {
		t.Fatalf("first Claim = %v, %v; want owned", ok, err)
	}
	if err := st.DB.WithContext(ctx).Model(&store.ReviewRequest{}).Where("id = ?", req.ID).
		Update("updated_at", time.Now().Add(-2*store.ReviewRequestClaimLease)).Error; err != nil {
		t.Fatalf("age the sending row: %v", err)
	}
	claimed, ok, err := st.ClaimReviewRequest(ctx, req.ID, 3)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if !ok || claimed.Attempts != 2 || claimed.Status != store.ReviewRequestSending {
		t.Fatalf("re-claim = %+v, %v; want a second attempt", claimed, ok)
	}
}

// The dashboard reads the latest request through the lateral join.
func TestListVisiblePullRequestsIncludesReviewRequest(t *testing.T) {
	st, pr := seedReviewRequestFixture(t)
	ctx := context.Background()
	delivered := time.Now().UTC().Truncate(time.Second)
	if _, _, err := st.CreateReviewRequest(ctx, store.ReviewRequest{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, Cause: store.ReviewRequestCauseInconclusive,
		ReviewerLogin: "qa-reviewer", Status: store.ReviewRequestDelivered,
		Attempts: 2, LastError: "gone", DeliveredAt: &delivered,
	}); err != nil {
		t.Fatalf("CreateReviewRequest: %v", err)
	}
	views, err := st.ListVisiblePullRequests(ctx, store.User{IsAdmin: true}, store.PRFilter{})
	if err != nil {
		t.Fatalf("ListVisiblePullRequests: %v", err)
	}
	if len(views) != 1 || views[0].ID != pr.ID {
		t.Fatalf("views = %+v, want the seeded pull request", views)
	}
	got := views[0].ReviewRequest
	if got == nil {
		t.Fatal("ReviewRequest is nil, want the latest request")
	}
	if got.Status != store.ReviewRequestDelivered || got.ReviewerLogin != "qa-reviewer" ||
		got.Attempts != 2 || got.LastError != "gone" || got.DeliveredAt == nil {
		t.Errorf("ReviewRequest = %+v, want the delivered request with its last error", got)
	}
}
