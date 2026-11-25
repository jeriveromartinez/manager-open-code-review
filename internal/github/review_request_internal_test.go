package github

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

func TestReviewRetryDelayCapsRetryAfter(t *testing.T) {
	limited := &ReviewRequestError{StatusCode: 403, RetryAfter: time.Hour}
	if got := reviewRetryDelay(limited, ReviewRequestRetryDelay); got != maxReviewRequestRetryAfter {
		t.Errorf("capped delay = %s, want %s", got, maxReviewRequestRetryAfter)
	}
	plain := errors.New("transient")
	if got := reviewRetryDelay(plain, ReviewRequestRetryDelay); got != ReviewRequestRetryDelay {
		t.Errorf("fallback delay = %s, want %s", got, ReviewRequestRetryDelay)
	}
}

type reviewAppsStub struct{}

func (reviewAppsStub) App(context.Context) (*App, error) {
	return nil, errors.New("the app must not be called")
}

// A stale sending row with no attempts left must end escalated, not stay
// sending forever.
func TestRequestEscalatesStaleSendingAfterBudget(t *testing.T) {
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
	_, req, err := st.CreateReviewRequest(ctx, store.ReviewRequest{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, Status: store.ReviewRequestSending,
		Attempts: 3, ReviewerLogin: "qa-reviewer",
	})
	if err != nil {
		t.Fatalf("CreateReviewRequest: %v", err)
	}
	if err := st.DB.WithContext(ctx).Model(&store.ReviewRequest{}).Where("id = ?", req.ID).
		Update("updated_at", time.Now().Add(-2*store.ReviewRequestClaimLease)).Error; err != nil {
		t.Fatalf("age the sending row: %v", err)
	}

	service := &ReviewRequestService{Store: st, Apps: reviewAppsStub{}}
	cfg := store.ReviewRequestConfig{
		PR:           pr,
		Repo:         store.Repository{FullName: "acme/repo"},
		Policy:       store.QAPolicy{ReviewerLogin: "qa-reviewer"},
		Installation: store.Installation{ID: 1},
	}
	if _, err := service.Request(ctx, cfg, store.ReviewRequestCauseInconclusive); err != nil {
		t.Fatalf("Request() error = %v", err)
	}
	saved, err := st.ReviewRequestFor(ctx, pr.ID, pr.HeadSHA)
	if err != nil {
		t.Fatalf("ReviewRequestFor: %v", err)
	}
	if saved.Status != store.ReviewRequestEscalated || saved.LastError != "retry budget exhausted" {
		t.Errorf("request = %+v, want escalated with the budget cause", saved)
	}
}
