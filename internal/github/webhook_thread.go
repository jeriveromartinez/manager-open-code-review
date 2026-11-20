package github

import (
	"context"
	"errors"
	"net/http"

	gh "github.com/google/go-github/v93/github"
	"gorm.io/gorm"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// handleReviewThread applies a pull_request_review_thread delivery: a person
// resolved or reopened a review thread, so the finding published in it must
// follow and the gate must be recomputed. GitHub already holds the new thread
// state, so only our side is written here.
func (h *WebhookHandler) handleReviewThread(ctx context.Context, w http.ResponseWriter, e *gh.PullRequestReviewThreadEvent) {
	desired := ""
	switch e.GetAction() {
	case "resolved":
		desired = store.FindingResolved
	case "unresolved":
		desired = store.FindingOpen
	default:
		w.WriteHeader(http.StatusNoContent)
		return
	}

	thread := e.GetThread()
	pr := e.GetPullRequest()
	eventRepo := e.GetRepo()
	if thread == nil || pr == nil || eventRepo == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	row, ok := h.enabledPullRequest(ctx, w, eventRepo.GetID(), pr.GetNumber())
	if !ok {
		return
	}

	commentIDs := make([]string, 0, len(thread.GetComments()))
	for _, comment := range thread.GetComments() {
		if nodeID := comment.GetNodeID(); nodeID != "" {
			commentIDs = append(commentIDs, nodeID)
		}
	}
	finding, err := h.Store.FindingForThread(ctx, row.ID, thread.GetNodeID(), commentIDs)
	if errors.Is(err, store.ErrNotFound) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		h.serverError(w)
		return
	}

	action := "finding.reopened"
	if desired == store.FindingResolved {
		action = "finding.resolved"
	}
	changed, err := h.Store.SetFindingStatusAudited(ctx, finding.ID, desired, store.FindingAudit{
		Action:   action,
		Actor:    e.GetSender().GetLogin(),
		ActorID:  h.actorID(ctx, e.GetSender().GetID()),
		ThreadID: thread.GetNodeID(),
	})
	if err != nil {
		h.serverError(w)
		return
	}
	if !changed {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if h.Findings != nil {
		if err := h.Findings.Republish(ctx, row.ID); err != nil {
			h.serverError(w)
			return
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

// enabledPullRequest resolves the stored pull request a delivery refers to. It
// writes the response itself and reports whether processing should continue.
func (h *WebhookHandler) enabledPullRequest(ctx context.Context, w http.ResponseWriter, repoID int64, number int) (store.PullRequest, bool) {
	var repo store.Repository
	err := h.Store.DB.WithContext(ctx).Where("id = ?", repoID).Take(&repo).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		w.WriteHeader(http.StatusNoContent)
		return store.PullRequest{}, false
	}
	if err != nil {
		h.serverError(w)
		return store.PullRequest{}, false
	}
	if !repo.Enabled {
		w.WriteHeader(http.StatusNoContent)
		return store.PullRequest{}, false
	}

	var pr store.PullRequest
	err = h.Store.DB.WithContext(ctx).
		Where("repository_id = ? AND number = ?", repoID, number).Take(&pr).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		w.WriteHeader(http.StatusNoContent)
		return store.PullRequest{}, false
	}
	if err != nil {
		h.serverError(w)
		return store.PullRequest{}, false
	}
	return pr, true
}

// actorID maps a GitHub account to the local user, or nil when that person never
// signed in. The audit row keeps the login either way.
func (h *WebhookHandler) actorID(ctx context.Context, githubID int64) *uint {
	if githubID == 0 {
		return nil
	}
	user, err := h.Store.UserByGitHubID(ctx, githubID)
	if err != nil {
		return nil
	}
	return &user.ID
}
