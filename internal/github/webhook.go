package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v93/github"
	"gorm.io/gorm"

	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// maxWebhookBody is the largest delivery accepted from GitHub (25 MiB).
const maxWebhookBody = 25 << 20

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

// WebhookHandler receives GitHub App deliveries. Store persists the state they
// describe, Apps supplies the webhook secret, Events notifies browsers and Jobs
// schedules the initial synchronization of new installations.
type WebhookHandler struct {
	Store  *store.Store
	Apps   AppProvider
	Events events.Publisher
	Jobs   SyncEnqueuer
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
	default:
		w.WriteHeader(http.StatusNoContent)
	}
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

	row, changed, err := h.Store.SyncPullRequest(ctx, PRFromGitHub(repo.ID, pr))
	if err != nil {
		h.serverError(w)
		return
	}
	if changed {
		ev := events.Event{Type: "pr.updated", ID: strconv.FormatUint(uint64(row.ID), 10)}
		if err := h.Events.Publish(ctx, ev); err != nil {
			h.serverError(w)
			return
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
