package store_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

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

// A retarget that keeps the head is a new comparison and must not reuse the
// delivered request of the previous base.
func TestCreateReviewRequestDeduplicatesByComparison(t *testing.T) {
	st, pr := seedReviewRequestFixture(t)
	ctx := context.Background()
	if err := st.DB.WithContext(ctx).Model(&pr).Updates(map[string]any{
		"base_ref": "main", "merge_base_sha": "mb-one",
	}).Error; err != nil {
		t.Fatalf("set the pull request comparison: %v", err)
	}
	pr.BaseRef = "main"
	pr.MergeBaseSHA = "mb-one"

	first := store.ReviewRequest{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, BaseRef: "main", MergeBaseSHA: "mb-one",
		Status: store.ReviewRequestDelivered,
	}
	created, stored, err := st.CreateReviewRequest(ctx, first)
	if err != nil {
		t.Fatalf("CreateReviewRequest(first): %v", err)
	}
	if !created {
		t.Fatalf("first CreateReviewRequest created = false, want true")
	}

	retarget := first
	retarget.MergeBaseSHA = "mb-two"
	created, second, err := st.CreateReviewRequest(ctx, retarget)
	if err != nil {
		t.Fatalf("CreateReviewRequest(retarget): %v", err)
	}
	if !created {
		t.Fatalf("retarget created = false, reused request %d of merge base %q",
			second.ID, second.MergeBaseSHA)
	}
	if second.ID == stored.ID || second.MergeBaseSHA != "mb-two" {
		t.Errorf("retarget request = %+v, want a new row for mb-two", second)
	}

	created, again, err := st.CreateReviewRequest(ctx, retarget)
	if err != nil {
		t.Fatalf("CreateReviewRequest(repeat): %v", err)
	}
	if created || again.ID != second.ID {
		t.Errorf("repeat created = %v id = %d, want the existing retarget request %d", created, again.ID, second.ID)
	}

	var count int64
	if err := st.DB.WithContext(ctx).Model(&store.ReviewRequest{}).
		Where("pull_request_id = ? AND head_sha = ?", pr.ID, pr.HeadSHA).Count(&count).Error; err != nil {
		t.Fatalf("count requests: %v", err)
	}
	if count != 2 {
		t.Errorf("requests for the head = %d, want one per comparison", count)
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

// The panel must show the request of the current comparison. A request for a
// previous base is history even if it is the newest row by id.
func TestListVisiblePullRequestsUsesCurrentComparisonRequest(t *testing.T) {
	st, pr := seedReviewRequestFixture(t)
	ctx := context.Background()
	if err := st.DB.WithContext(ctx).Model(&pr).Updates(map[string]any{
		"base_ref": "main", "merge_base_sha": "mb-new",
	}).Error; err != nil {
		t.Fatalf("set the pull request comparison: %v", err)
	}
	current := store.ReviewRequest{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, BaseRef: "main", MergeBaseSHA: "mb-new",
		Cause: store.ReviewRequestCauseInconclusive, ReviewerLogin: "qa-reviewer",
		Status: store.ReviewRequestPending,
	}
	if _, _, err := st.CreateReviewRequest(ctx, current); err != nil {
		t.Fatalf("CreateReviewRequest(current): %v", err)
	}
	previous := current
	previous.MergeBaseSHA = "mb-old"
	previous.Status = store.ReviewRequestDelivered
	if _, _, err := st.CreateReviewRequest(ctx, previous); err != nil {
		t.Fatalf("CreateReviewRequest(previous): %v", err)
	}

	views, err := st.ListVisiblePullRequests(ctx, store.User{IsAdmin: true}, store.PRFilter{})
	if err != nil {
		t.Fatalf("ListVisiblePullRequests: %v", err)
	}
	if len(views) != 1 || views[0].ReviewRequest == nil {
		t.Fatalf("views = %+v, want a review request summary", views)
	}
	if got := views[0].ReviewRequest; got.Status != store.ReviewRequestPending {
		t.Errorf("review request status = %q, want pending from the current comparison", got.Status)
	}
}

// The recovery sweep only selects rows that can make progress: pending, stale
// sending, and rejected with a changed destination. Terminal rows, fresh sends,
// rows of another comparison and repositories without a usable configuration
// stay out.
func TestRecoverableReviewRequestsSelectsResumableRows(t *testing.T) {
	st, first := seedReviewRequestFixture(t)
	ctx := context.Background()
	team := store.Team{Name: "qa", Slug: "qa"}
	if err := st.DB.WithContext(ctx).Create(&team).Error; err != nil {
		t.Fatalf("seed team: %v", err)
	}
	if err := st.DB.WithContext(ctx).Model(&store.Repository{}).Where("id = ?", 1).
		Update("team_id", team.ID).Error; err != nil {
		t.Fatalf("assign the team: %v", err)
	}
	for _, repo := range []store.Repository{
		{ID: 2, InstallationID: 1, FullName: "acme/disabled", Enabled: false, TeamID: &team.ID},
		{ID: 3, InstallationID: 1, FullName: "acme/no-team", Enabled: true},
	} {
		if err := st.DB.WithContext(ctx).Create(&repo).Error; err != nil {
			t.Fatalf("seed repository %d: %v", repo.ID, err)
		}
	}

	createPR := func(repoID int64, number int, head string) store.PullRequest {
		t.Helper()
		pr := store.PullRequest{
			RepositoryID: repoID, Number: number, HeadSHA: head, BaseRef: "main",
			MergeBaseSHA: "mb", State: "open", GitHubUpdatedAt: time.Now().UTC(),
		}
		if err := st.DB.WithContext(ctx).Create(&pr).Error; err != nil {
			t.Fatalf("seed pull request %d: %v", number, err)
		}
		return pr
	}
	seed := func(pr store.PullRequest, status, reviewer string, attempts int) store.ReviewRequest {
		t.Helper()
		_, req, err := st.CreateReviewRequest(ctx, store.ReviewRequest{
			PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, BaseRef: pr.BaseRef, MergeBaseSHA: pr.MergeBaseSHA,
			Cause: store.ReviewRequestCauseInconclusive, ReviewerLogin: reviewer,
			Status: status, Attempts: attempts,
		})
		if err != nil {
			t.Fatalf("seed %s request: %v", status, err)
		}
		return req
	}

	// first is reused as the pending case.
	pending := seed(first, store.ReviewRequestPending, "", 0)

	staleSending := seed(createPR(1, 11, "head"), store.ReviewRequestSending, "qa-reviewer", 1)
	if err := st.DB.WithContext(ctx).Model(&store.ReviewRequest{}).Where("id = ?", staleSending.ID).
		Update("updated_at", time.Now().Add(-2*store.ReviewRequestClaimLease)).Error; err != nil {
		t.Fatalf("age the sending row: %v", err)
	}
	seed(createPR(1, 12, "head"), store.ReviewRequestSending, "qa-reviewer", 1)
	rejectedChanged := seed(createPR(1, 13, "head"), store.ReviewRequestRejected, "old-reviewer", 1)
	seed(createPR(1, 14, "head"), store.ReviewRequestRejected, "", 1)
	seed(createPR(1, 15, "head"), store.ReviewRequestDelivered, "qa-reviewer", 1)
	seed(createPR(1, 16, "head"), store.ReviewRequestEscalated, "qa-reviewer", 3)

	moved := createPR(1, 17, "head")
	movedRequest := seed(moved, store.ReviewRequestPending, "", 0)
	if err := st.DB.WithContext(ctx).Model(&store.PullRequest{}).Where("id = ?", moved.ID).
		Update("merge_base_sha", "mb-new").Error; err != nil {
		t.Fatalf("move the pull request comparison: %v", err)
	}
	otherHead := createPR(1, 18, "head-new")
	if _, _, err := st.CreateReviewRequest(ctx, store.ReviewRequest{
		PullRequestID: otherHead.ID, HeadSHA: "head-old", BaseRef: "main", MergeBaseSHA: "mb",
		Status: store.ReviewRequestPending,
	}); err != nil {
		t.Fatalf("seed request of a previous head: %v", err)
	}

	// A disabled repository and one without a team cannot produce a request;
	// they must not consume the sweep's batch.
	seed(createPR(2, 1, "head"), store.ReviewRequestPending, "", 0)
	seed(createPR(3, 1, "head"), store.ReviewRequestPending, "", 0)

	rows, err := st.RecoverableReviewRequests(ctx, 20)
	if err != nil {
		t.Fatalf("RecoverableReviewRequests: %v", err)
	}
	want := map[uint]bool{pending.ID: true, staleSending.ID: true, rejectedChanged.ID: true}
	if len(rows) != len(want) {
		t.Fatalf("recoverable requests = %d (%+v), want %d", len(rows), rows, len(want))
	}
	for _, row := range rows {
		if !want[row.ID] {
			t.Errorf("unexpected recoverable request %+v", row)
		}
		delete(want, row.ID)
	}
	if len(want) != 0 {
		t.Errorf("missing recoverable requests: %v (moved request %d)", want, movedRequest.ID)
	}
}

// Rolling back the comparison index must collapse the retarget rows it made
// possible, otherwise recreating (pull_request_id, head_sha) fails.
func TestReviewRequestComparisonMigrationDown(t *testing.T) {
	st, pr := seedReviewRequestFixture(t)
	ctx := context.Background()
	first := store.PullRequest{
		RepositoryID: 1, Number: 8, HeadSHA: "other", BaseRef: "main",
		MergeBaseSHA: "mb", State: "open", GitHubUpdatedAt: time.Now().UTC(),
	}
	if err := st.DB.WithContext(ctx).Create(&first).Error; err != nil {
		t.Fatalf("seed second pull request: %v", err)
	}

	type pair struct {
		prID uint
		head string
		kept uint
	}
	seedPair := func(prID uint, head string, mergeBases ...string) pair {
		t.Helper()
		p := pair{prID: prID, head: head}
		for _, mergeBase := range mergeBases {
			_, row, err := st.CreateReviewRequest(ctx, store.ReviewRequest{
				PullRequestID: prID, HeadSHA: head, BaseRef: "main", MergeBaseSHA: mergeBase,
				Status: store.ReviewRequestDelivered,
			})
			if err != nil {
				t.Fatalf("seed request %s/%s: %v", head, mergeBase, err)
			}
			p.kept = row.ID
		}
		return p
	}
	// Two different bases for the same head leave two rows; the newest per pair
	// must survive the rollback.
	pairs := []pair{
		seedPair(pr.ID, pr.HeadSHA, "mb-old", "mb-new"),
		seedPair(first.ID, first.HeadSHA, "mb-old", "mb-new"),
		seedPair(pr.ID, "single", "mb"),
	}

	sqlDB, err := st.DB.DB()
	if err != nil {
		t.Fatalf("sql.DB: %v", err)
	}
	goose.SetBaseFS(os.DirFS("."))
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	if err := goose.DownToContext(ctx, sqlDB, "migrations", 35); err != nil {
		t.Fatalf("migrate down to 00035: %v", err)
	}

	var oldIndex, newIndex int64
	if err := st.DB.Raw(`SELECT count(*) FROM pg_indexes
		WHERE schemaname = 'public' AND indexname = 'idx_review_requests_pr_sha'`).Scan(&oldIndex).Error; err != nil {
		t.Fatalf("check old index: %v", err)
	}
	if err := st.DB.Raw(`SELECT count(*) FROM pg_indexes
		WHERE schemaname = 'public' AND indexname = 'idx_review_requests_comparison'`).Scan(&newIndex).Error; err != nil {
		t.Fatalf("check new index: %v", err)
	}
	if oldIndex != 1 || newIndex != 0 {
		t.Errorf("indexes after Down: old = %d new = %d, want the head index back", oldIndex, newIndex)
	}
	for _, p := range pairs {
		var count int64
		if err := st.DB.WithContext(ctx).Model(&store.ReviewRequest{}).
			Where("pull_request_id = ? AND head_sha = ?", p.prID, p.head).Count(&count).Error; err != nil {
			t.Fatalf("count requests of %s: %v", p.head, err)
		}
		if count != 1 {
			t.Errorf("requests of %s = %d, want only the newest row", p.head, count)
		}
		saved, err := st.ReviewRequestFor(ctx, p.prID, p.head)
		if err != nil {
			t.Fatalf("ReviewRequestFor(%s): %v", p.head, err)
		}
		if saved.ID != p.kept {
			t.Errorf("kept request of %s = %d, want %d", p.head, saved.ID, p.kept)
		}
	}
}
