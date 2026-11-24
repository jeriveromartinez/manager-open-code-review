package github

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	gh "github.com/google/go-github/v93/github"
	"gorm.io/gorm"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// handleReviewThread records a pull_request_review_thread delivery. GitHub's
// thread state is not a QA verdict: it is stored apart from the triage columns.
// Only an authorized sender may turn the event into a finding transition, and a
// person's existing triage always wins. App echoes are recorded as thread state
// only, and repeated, delayed or out-of-order deliveries never flip the state
// back.
func (h *WebhookHandler) handleReviewThread(ctx context.Context, w http.ResponseWriter,
	e *gh.PullRequestReviewThreadEvent, deliveryID string, eventAt time.Time) {
	resolved := false
	switch e.GetAction() {
	case "resolved":
		resolved = true
	case "unresolved":
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
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrFindingThreadAmbiguous) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		h.serverError(w)
		return
	}

	actor := h.resolveActor(ctx, e.GetSender().GetLogin(), e.GetSender().GetID())
	// The App resolves threads itself; its echo reconciles the thread state if
	// the local write after the mutation failed, but it is never a QA decision.
	applyStatus := !isBotSender(e.GetSender()) && h.canTriage(ctx, actor, row)
	status := store.FindingOpen
	action, reason := "finding.reopened", "review thread reopened on GitHub"
	if resolved {
		status = store.FindingResolved
		action, reason = "finding.resolved", "review thread resolved on GitHub"
	}

	changed, statusChanged, err := h.Store.SyncFindingThread(ctx, finding.ID, store.ThreadSync{
		Resolved:    resolved,
		Status:      status,
		ApplyStatus: applyStatus,
		EventAt:     eventAt,
		Audit: store.FindingAudit{
			Action:     action,
			Actor:      actor.Login,
			ActorID:    actorID(actor),
			Reason:     reason,
			ThreadID:   thread.GetNodeID(),
			DeliveryID: deliveryID,
		},
	})
	if err != nil {
		h.serverError(w)
		return
	}
	if !changed {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if statusChanged && h.Findings != nil {
		if err := h.Findings.Republish(ctx, row.ID); err != nil {
			h.serverError(w)
			return
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

// isBotSender reports whether a delivery was produced by an App rather than a
// person. Those are echoes of our own mutations.
func isBotSender(user *gh.User) bool {
	return user.GetType() == "Bot" || strings.HasSuffix(user.GetLogin(), "[bot]")
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
