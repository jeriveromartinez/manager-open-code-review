package publish

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

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
