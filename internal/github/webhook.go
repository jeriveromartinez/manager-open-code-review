package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v93/github"
	"gorm.io/gorm"

	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// maxWebhookBody is the largest delivery accepted from GitHub (25 MiB).
const maxWebhookBody = 25 << 20

// stateOpenPR is GitHub's state for a pull request that is still open. A CI
// delivery for a closed or merged pull request has nothing to gate.
const stateOpenPR = "open"

// VerifySignature checks the X-Hub-Signature-256 value of a delivery against
// its raw body. Comparison is constant-time; a malformed header is rejected.
func VerifySignature(secret, body []byte, header string) bool {
	if !strings.HasPrefix(header, "sha256=") {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// ReviewStarter is notified when a pull request should be considered for an
// automated review, when it stops being eligible (closed, merged, draft) and
// when its checks change. Implementations decide whether to enqueue work; the
// webhook boundary only applies the exclusion rules.
type ReviewStarter interface {
	OnPullRequest(ctx context.Context, prID uint) error
	OnPullRequestIneligible(ctx context.Context, prID uint) error
	OnChecksUpdated(ctx context.Context, prID uint) error
	Retry(ctx context.Context, prID uint) error
}

// FindingsReconciler applies a finding change that happened outside a review
// run: it closes or reopens the GitHub review thread and republishes the gate so
// the check run and the panel agree.
type FindingsReconciler interface {
	Republish(ctx context.Context, prID uint) error
	SetFindingThread(ctx context.Context, prID uint, finding store.Finding, resolved bool) (bool, error)
}

// PRCommenter posts an issue comment on a pull request. Commands reply through
// it so the person who typed /qa sees the outcome where they wrote it.
type PRCommenter interface {
	CommentOnPullRequest(ctx context.Context, repoFullName string, number int, body string) error
}

// ChecksRefresher re-reads the CI state of a pull request head from GitHub and
// stores it, so the dashboard and the trigger gate agree on the same verdict.
type ChecksRefresher interface {
	RefreshPR(ctx context.Context, prID uint) (store.PRValidations, bool, error)
}

// WebhookHandler receives GitHub App deliveries. Store persists the state they
// describe, Apps supplies the webhook secret, Events notifies browsers, Jobs
// schedules the initial synchronization of new installations, Reviews starts
// automated reviews, Findings/Comments apply thread disputes and /qa commands,
// and Checks refreshes the CI counters of a head (all optional, except Apps,
// Store and Events).
type WebhookHandler struct {
	Store    *store.Store
	Apps     AppProvider
	Events   events.Publisher
	Jobs     SyncEnqueuer
	Reviews  ReviewStarter
	Findings FindingsReconciler
	Comments PRCommenter
	Checks   ChecksRefresher
	// ObjectionJobs queues the worker evaluation and the publisher reply of a
	// reply on a finding's thread. The app has no OCR, so it never evaluates.
	ObjectionJobs ObjectionEnqueuer
	// Box seals the read-only token the worker uses to fetch the cited file.
	Box *crypto.Box
}

// ServeHTTP verifies and dispatches a webhook delivery. Events this phase does
// not handle still get a successful response so GitHub does not retry them.
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > maxWebhookBody {
		http.Error(w, "webhook payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "webhook payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "cannot read webhook payload", http.StatusBadRequest)
		return
	}

	app, err := h.Apps.App(r.Context())
	if errors.Is(err, store.ErrNotConfigured) || (err == nil && app == nil) {
		http.Error(w, "GitHub App is not configured", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		h.serverError(w)
		return
	}
	if !VerifySignature([]byte(app.Creds.WebhookSecret), body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "invalid webhook signature", http.StatusUnauthorized)
		return
	}

	payload, err := gh.ParseWebHook(r.Header.Get("X-GitHub-Event"), body)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch p := payload.(type) {
	case *gh.InstallationEvent:
		h.handleInstallation(r.Context(), w, p)
	case *gh.InstallationRepositoriesEvent:
		h.handleInstallationRepositories(r.Context(), w, p)
	case *gh.PullRequestEvent:
		h.handlePullRequest(r.Context(), w, p)
	case *gh.CheckRunEvent:
		h.handleCheckEvent(r.Context(), w, p.GetRepo(), p.GetCheckRun().GetHeadSHA())
	case *gh.CheckSuiteEvent:
		h.handleCheckEvent(r.Context(), w, p.GetRepo(), p.GetCheckSuite().GetHeadSHA())
	case *gh.StatusEvent:
		h.handleCheckEvent(r.Context(), w, p.GetRepo(), p.GetSHA())
	case *gh.PullRequestReviewThreadEvent:
		h.handleReviewThread(r.Context(), w, p, r.Header.Get("X-GitHub-Delivery"), reviewThreadEventTime(body))
	case *gh.PullRequestReviewCommentEvent:
		h.handleReviewComment(r.Context(), w, p, r.Header.Get("X-GitHub-Delivery"))
	case *gh.PullRequestReviewEvent:
		h.handlePullRequestReview(r.Context(), w, p)
	case *gh.IssueCommentEvent:
		h.handleIssueComment(r.Context(), w, p)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// reviewThreadEventTime reads the top-level updated_at GitHub sends with a
// pull_request_review_thread delivery. It orders resolve/unresolve events, so a
// delayed delivery cannot overwrite a newer one. A payload without the field
// yields the zero time and falls back to state and delivery-id checks.
func reviewThreadEventTime(body []byte) time.Time {
	var payload struct {
		UpdatedAt *time.Time `json:"updated_at"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.UpdatedAt == nil {
		return time.Time{}
	}
	return payload.UpdatedAt.UTC()
}

func (h *WebhookHandler) handleInstallation(ctx context.Context, w http.ResponseWriter, e *gh.InstallationEvent) {
	inst := e.GetInstallation()
	if inst == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	id := inst.GetID()
	switch e.GetAction() {
	case "created":
		if err := h.Store.UpsertInstallation(ctx, InstallationFromGitHub(inst)); err != nil {
			h.serverError(w)
			return
		}
		if err := h.Jobs.EnqueueSync(ctx, id); err != nil {
			h.serverError(w)
			return
		}
	case "deleted":
		if err := h.Store.DeleteInstallation(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
			h.serverError(w)
			return
		}
	case "suspend", "unsuspend":
		row := InstallationFromGitHub(inst)
		if e.GetAction() == "unsuspend" {
			row.SuspendedAt = nil
		}
		if err := h.Store.UpsertInstallation(ctx, row); err != nil {
			h.serverError(w)
			return
		}
		if e.GetAction() == "unsuspend" {
			if err := h.Jobs.EnqueueSync(ctx, id); err != nil {
				h.serverError(w)
				return
			}
		}
	default:
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *WebhookHandler) handleInstallationRepositories(ctx context.Context, w http.ResponseWriter, e *gh.InstallationRepositoriesEvent) {
	installationID := e.GetInstallation().GetID()

	switch e.GetAction() {
	case "added":
		for _, repo := range e.GetRepositoriesAdded() {
			row := store.Repository{
				ID:             repo.GetID(),
				InstallationID: installationID,
				FullName:       repo.GetFullName(),
				Private:        repo.GetPrivate(),
				DefaultBranch:  repo.GetDefaultBranch(),
			}
			if err := h.Store.UpsertRepository(ctx, row); err != nil {
				h.serverError(w)
				return
			}
			if err := h.publishRepoUpdated(ctx, row.ID); err != nil {
				h.serverError(w)
				return
			}
		}
	case "removed":
		for _, repo := range e.GetRepositoriesRemoved() {
			if err := h.Store.RemoveRepository(ctx, repo.GetID()); err != nil && !errors.Is(err, store.ErrNotFound) {
				h.serverError(w)
				return
			}
			if err := h.publishRepoUpdated(ctx, repo.GetID()); err != nil {
				h.serverError(w)
				return
			}
		}
	default:
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *WebhookHandler) handlePullRequest(ctx context.Context, w http.ResponseWriter, e *gh.PullRequestEvent) {
	switch e.GetAction() {
	case "opened", "synchronize", "reopened", "ready_for_review", "converted_to_draft", "edited", "closed":
	default:
		w.WriteHeader(http.StatusNoContent)
		return
	}

	pr := e.GetPullRequest()
	eventRepo := e.GetRepo()
	if pr == nil || eventRepo == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var repo store.Repository
	err := h.Store.DB.WithContext(ctx).Where("id = ?", eventRepo.GetID()).Take(&repo).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		h.serverError(w)
		return
	}
	if !repo.Enabled {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// An edit can change the base branch of the pull request. Loading the row
	// before the sync tells us whether the comparison actually moved, so a
	// metadata-only edit does not schedule a review (#141).
	var prev store.PullRequest
	prevKnown := false
	if e.GetAction() == "edited" {
		err := h.Store.DB.WithContext(ctx).
			Where("repository_id = ? AND number = ?", repo.ID, pr.GetNumber()).
			Take(&prev).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
		case err != nil:
			h.serverError(w)
			return
		default:
			prevKnown = true
		}
	}

	row, changed, err := h.Store.SyncPullRequest(ctx, PRFromGitHub(repo.ID, pr))
	if err != nil {
		h.serverError(w)
		return
	}
	if changed {
		// Fork pull requests are stored so the UI and later syncs know about
		// them, but Fase 2 neither notifies browsers nor starts reviews for
		// them.
		if !row.IsFork {
			ev := events.Event{Type: "pr.updated", ID: strconv.FormatUint(uint64(row.ID), 10)}
			if err := h.Events.Publish(ctx, ev); err != nil {
				h.serverError(w)
				return
			}
		}
	}

	trigger := isReviewTriggerAction(e.GetAction())
	if e.GetAction() == "edited" {
		trigger = !prevKnown || store.ComparisonMoved(prev, row)
	}
	if h.Reviews != nil {
		switch {
		case !row.IsOpen() || row.Draft || row.IsFork:
			// A closed, merged, draft or fork pull request can still carry live
			// work from an earlier delivery. Reconciling on every delivery, not
			// only when the sync changed something, lets an identical
			// redelivery discard a run whose first cancellation failed.
			if err := h.Reviews.OnPullRequestIneligible(ctx, row.ID); err != nil {
				h.serverError(w)
				return
			}
		case trigger:
			// The trigger deduplicates by comparison and repairs a queued run
			// that lost its job or check, so reasserting it is idempotent.
			// Gating this on `changed` let an identical redelivery skip the
			// work the previous delivery failed to finish.
			if err := h.Reviews.OnPullRequest(ctx, row.ID); err != nil {
				h.serverError(w)
				return
			}
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

// isReviewTriggerAction reports which pull request deliveries may start a
// review. Other actions still sync the PR but never trigger one. "edited" is
// gated separately on the comparison identity, because most edits are metadata
// only.
func isReviewTriggerAction(action string) bool {
	switch action {
	case "opened", "synchronize", "reopened", "ready_for_review", "edited":
		return true
	default:
		return false
	}
}

// handleCheckEvent applies a check_run, check_suite or status delivery. The
// payload only tells which commit moved: the verdict is re-read from GitHub so
// the counters always describe the whole head and never a single check. Any
// delivery that does not map to a known, open pull request at that exact SHA is
// acknowledged without work, because GitHub would otherwise retry it forever.
func (h *WebhookHandler) handleCheckEvent(ctx context.Context, w http.ResponseWriter, repo *gh.Repository, headSHA string) {
	if repo == nil || headSHA == "" || h.Checks == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var row store.Repository
	err := h.Store.DB.WithContext(ctx).Where("id = ?", repo.GetID()).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		h.serverError(w)
		return
	}
	if !row.Enabled {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Several pull requests can point at the same commit (for example a branch
	// and a backport PR), so the delivery re-evaluates every one of them instead
	// of only the first row GitHub's query happens to return.
	prs, err := h.Store.OpenPRsForHeadSHA(ctx, row.ID, headSHA)
	if err != nil {
		h.serverError(w)
		return
	}
	if len(prs) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	for _, pr := range prs {
		_, changed, err := h.Checks.RefreshPR(ctx, pr.ID)
		if err != nil {
			h.serverError(w)
			return
		}

		// The gate is re-evaluated even when the counters did not move: a review
		// blocked on a red head must be released by the delivery that completes the
		// checks, and OnChecksUpdated is what deduplicates it against existing runs.
		if changed && !pr.IsFork {
			ev := events.Event{Type: "pr.updated", ID: strconv.FormatUint(uint64(pr.ID), 10)}
			if err := h.Events.Publish(ctx, ev); err != nil {
				h.serverError(w)
				return
			}
		}
		if !pr.Draft && !pr.IsFork && h.Reviews != nil {
			if err := h.Reviews.OnChecksUpdated(ctx, pr.ID); err != nil {
				h.serverError(w)
				return
			}
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *WebhookHandler) publishRepoUpdated(ctx context.Context, repoID int64) error {
	return h.Events.Publish(ctx, events.Event{
		Type: "repo.updated",
		ID:   strconv.FormatInt(repoID, 10),
	})
}

func (h *WebhookHandler) serverError(w http.ResponseWriter) {
	http.Error(w, "webhook processing failed", http.StatusInternalServerError)
}
