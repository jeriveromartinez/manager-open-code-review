package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	gh "github.com/google/go-github/v93/github"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// objectionTokenTimeout bounds token minting and the cited-file fetch. The
// webhook never runs OCR; a failure leaves the intent without content and the
// worker can retry with the sealed token.
const objectionTokenTimeout = 5 * time.Second

// maxObjectionFileBytes bounds the cited file copied into the intent.
const maxObjectionFileBytes = 1 << 20

// handleReviewComment treats a person's reply on a finding's review thread as
// an objection. The app records the intent (finding, head, author, reply and the
// cited file) and enqueues the worker; it never runs OCR inline. Replies on
// other findings, stale revisions, App echoes and human decisions are no-ops.
func (h *WebhookHandler) handleReviewComment(ctx context.Context, w http.ResponseWriter,
	e *gh.PullRequestReviewCommentEvent, deliveryID string) {
	if e.GetAction() != "created" || isBotSender(e.GetSender()) {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	comment := e.GetComment()
	pr := e.GetPullRequest()
	eventRepo := e.GetRepo()
	if comment == nil || pr == nil || eventRepo == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Only a reply disputes an existing finding. A top-level review comment is
	// not attached to a published thread.
	inReplyTo := comment.GetInReplyTo()
	if inReplyTo == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	row, ok := h.enabledPullRequest(ctx, w, eventRepo.GetID(), pr.GetNumber())
	if !ok {
		return
	}

	finding, err := h.Store.FindingForReviewComment(ctx, row.ID, inReplyTo)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrFindingThreadAmbiguous) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		h.serverError(w)
		return
	}

	// The reply must describe the head the finding is still published at. A
	// comment written against an older revision describes code that is gone.
	headSHA := strings.TrimSpace(comment.GetCommitID())
	if headSHA == "" {
		headSHA = strings.TrimSpace(comment.GetOriginalCommitID())
	}
	if headSHA == "" || !strings.EqualFold(headSHA, row.HeadSHA) {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// A resolved finding and a person's triage are final: neither is disputed by
	// a reply, and reevaluating them would override the human decision.
	if finding.Status != store.FindingOpen || finding.TriageSource == store.TriageSourceHuman {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	actor := h.resolveActor(ctx, e.GetSender().GetLogin(), e.GetSender().GetID())
	authorized := h.authorizedObjection(ctx, actor, row)
	content, sealedToken := h.objectionEvidence(ctx, row, finding.Path, headSHA)
	intent, _, err := h.Store.EnqueueFindingObjection(ctx, store.FindingObjection{
		FindingID:     finding.ID,
		PullRequestID: row.ID,
		HeadSHA:       headSHA,
		CommentID:     comment.GetID(),
		DeliveryID:    deliveryID,
		Actor:         actor.Login,
		ActorID:       actorID(actor),
		Authorized:    authorized,
		Body:          comment.GetBody(),
		FilePath:      finding.Path,
		FileContent:   content,
		ReadTokenEnc:  sealedToken,
	})
	if errors.Is(err, store.ErrFindingResolved) || errors.Is(err, store.ErrFindingHumanTriaged) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		h.serverError(w)
		return
	}

	if h.ObjectionJobs == nil {
		// The intent is durable; the reconciler cannot run without an enqueuer,
		// so report the failure instead of pretending the objection was queued.
		h.serverError(w)
		return
	}
	switch intent.State {
	case store.ObjectionPublished, store.ObjectionFailed:
		// Terminal: a redelivery must not spend another OCR pass.
		w.WriteHeader(http.StatusNoContent)
		return
	case store.ObjectionEvaluated:
		// The evaluation landed but the reply may not have: heal it.
		if err := h.ObjectionJobs.EnqueueObjectionPublish(ctx, intent.ID); err != nil {
			h.serverError(w)
			return
		}
	default:
		if err := h.ObjectionJobs.EnqueueObjection(ctx, intent.ID); err != nil {
			h.serverError(w)
			return
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

// authorizedObjection reports whether the sender may change the gate with an
// objection: the pull request author, an administrator, or the team's lead/QA.
func (h *WebhookHandler) authorizedObjection(ctx context.Context, actor qaActor, pr store.PullRequest) bool {
	if actor.Login == "" {
		return false
	}
	if pr.AuthorLogin != "" && strings.EqualFold(actor.Login, pr.AuthorLogin) {
		return true
	}
	return h.canTriage(ctx, actor, pr)
}

// objectionEvidence fetches the cited file at the reviewed head with a
// short-lived read token. The content is what the worker verifies; only when
// that fetch fails does it seal the token as a fallback for the worker.
func (h *WebhookHandler) objectionEvidence(ctx context.Context, pr store.PullRequest, path, ref string) ([]byte, string) {
	if h.Apps == nil || path == "" || ref == "" {
		return nil, ""
	}
	cfg, err := h.Store.ReviewConfigForPR(ctx, pr.ID)
	if err != nil {
		return nil, ""
	}
	app, err := h.Apps.App(ctx)
	if err != nil {
		return nil, ""
	}
	mintCtx, cancel := context.WithTimeout(ctx, objectionTokenTimeout)
	defer cancel()
	token, _, err := app.RepositoryReadToken(mintCtx, cfg.Installation.ID, cfg.Repo.FullName)
	if err != nil {
		return nil, ""
	}
	content, err := fetchCitedFile(ctx, app.APIURL, cfg.Repo.FullName, path, ref, token)
	if err == nil {
		return content, ""
	}
	sealed := ""
	if h.Box != nil {
		if value, err := h.Box.Seal([]byte(token)); err == nil {
			sealed = value
		}
	}
	return nil, sealed
}

// fetchCitedFile downloads one file with a read-only installation token.
func fetchCitedFile(ctx context.Context, apiURL, repoFullName, path, ref, token string) ([]byte, error) {
	owner, name, ok := strings.Cut(repoFullName, "/")
	if !ok || owner == "" || name == "" {
		return nil, errors.New("invalid repository full name")
	}
	fetchCtx, cancel := context.WithTimeout(ctx, objectionTokenTimeout)
	defer cancel()
	endpoint := strings.TrimRight(apiURL, "/") + "/repos/" + url.PathEscape(owner) + "/" +
		url.PathEscape(name) + "/contents/" + escapeRepoPath(path) + "?ref=" + url.QueryEscape(ref)
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github.raw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, errors.New("GitHub API rejected the cited file request")
	}
	content, err := io.ReadAll(io.LimitReader(resp.Body, maxObjectionFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) > maxObjectionFileBytes {
		return nil, errors.New("cited file is too large")
	}
	return content, nil
}

func escapeRepoPath(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}
