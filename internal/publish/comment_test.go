package publish_test

import (
	"context"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

func TestCommentOnPullRequestPostsIssueComment(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seedPublishContext(t, st, store.RunSucceeded, "major")
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.CommentOnPullRequest(ctx, "acme/repo", 7, "Reintento encolado"); err != nil {
		t.Fatalf("CommentOnPullRequest() error = %v", err)
	}

	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(gh.comments))
	}
	if gh.comments[0].Body != "Reintento encolado" {
		t.Errorf("comment body = %q", gh.comments[0].Body)
	}
}
