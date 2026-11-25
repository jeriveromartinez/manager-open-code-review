package github

import (
	"context"
	"net/http"
	"strings"

	gh "github.com/google/go-github/v93/github"
)

// handlePullRequestReview acknowledges a submitted pull request review. A
// GitHub approval or request-changes review is generic: it names neither the
// files and ranges the engine could not cover nor the pending findings the
// person examined, and its commit may belong to another comparison. Closing an
// inconclusive review therefore stays an explicit, scoped decision recorded
// from the panel (#146); an approval alone closes nothing.
func (h *WebhookHandler) handlePullRequestReview(ctx context.Context, w http.ResponseWriter, e *gh.PullRequestReviewEvent) {
	if e.GetAction() != "submitted" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	review := e.GetReview()
	pr := e.GetPullRequest()
	eventRepo := e.GetRepo()
	if review == nil || pr == nil || eventRepo == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	row, ok := h.enabledPullRequest(ctx, w, eventRepo.GetID(), pr.GetNumber())
	if !ok {
		return
	}

	// Only the canonical comparison can ever be relevant; a review of another
	// SHA is stale by definition.
	if !strings.EqualFold(review.GetCommitID(), row.HeadSHA) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	actor := h.resolveActor(ctx, e.GetSender().GetLogin(), e.GetSender().GetID())
	if !h.canTriage(ctx, actor, row) {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
