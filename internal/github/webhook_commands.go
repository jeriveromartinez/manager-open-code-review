package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	gh "github.com/google/go-github/v93/github"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// qaCommandPrefix is the slash command the install docs advertise on pull
// requests. Anything else in an issue comment is ignored.
const qaCommandPrefix = "/qa"

// qaCommand is one parsed /qa line.
type qaCommand struct {
	name string
	args string
}

// qaActor is the commenter who issued a command, resolved against local
// installation data rather than trusted from the comment body.
type qaActor struct {
	Login string
	User  *store.User
}

// handleIssueComment applies a /qa command typed on a pull request. Only the
// documented roles may act, and the outcome is replied in the thread where the
// command was typed.
func (h *WebhookHandler) handleIssueComment(ctx context.Context, w http.ResponseWriter, e *gh.IssueCommentEvent) {
	if e.GetAction() != "created" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	issue := e.GetIssue()
	comment := e.GetComment()
	eventRepo := e.GetRepo()
	// Commands on plain issues are out of scope: there is no PR to act on.
	if issue == nil || issue.PullRequestLinks == nil || comment == nil || eventRepo == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	cmd, ok := parseQACommand(comment.GetBody())
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	row, ok := h.enabledPullRequest(ctx, w, eventRepo.GetID(), issue.GetNumber())
	if !ok {
		return
	}

	actor := h.resolveActor(ctx, e.GetSender().GetLogin(), e.GetSender().GetID())
	replyWith := func(body string) error {
		if h.Comments == nil {
			return nil
		}
		return h.Comments.CommentOnPullRequest(ctx, eventRepo.GetFullName(), row.Number, body)
	}

	if !h.canTriage(ctx, actor, row) {
		if err := replyWith(fmt.Sprintf(
			"Solo un admin, un lead o QA del equipo puede ejecutar `%s`. No se aplicó ningún cambio.", qaCommandPrefix)); err != nil {
			h.serverError(w)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Commands are not idempotent by nature, and GitHub redelivers webhooks.
	handled, err := h.Store.CommandHandled(ctx, comment.GetID())
	if err != nil {
		h.serverError(w)
		return
	}
	if handled {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch cmd.name {
	case "retry":
		h.commandRetry(ctx, w, row, comment.GetID(), actor, replyWith)
	case "dismiss":
		h.commandSetFindings(ctx, w, row, cmd, comment.GetID(), actor, store.FindingWithdrawn, replyWith)
	case "confirm":
		h.commandSetFindings(ctx, w, row, cmd, comment.GetID(), actor, store.FindingOpen, replyWith)
	default:
		if err := replyWith(fmt.Sprintf(
			"Comando `%s %s` no reconocido. Disponibles: `retry`, `dismiss <motivo>` y `confirm`.",
			qaCommandPrefix, cmd.name)); err != nil {
			h.serverError(w)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

func (h *WebhookHandler) commandRetry(ctx context.Context, w http.ResponseWriter, pr store.PullRequest,
	commentID int64, actor qaActor, reply func(string) error) {
	if h.Reviews == nil {
		if err := reply("El reintento no está disponible en esta instancia."); err != nil {
			h.serverError(w)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if err := h.Reviews.Retry(ctx, pr.ID); err != nil {
		h.serverError(w)
		return
	}
	if err := h.Store.AuditCommand(ctx, actorID(actor), "review.retried", "pull_request",
		fmt.Sprintf("%d", pr.ID), actor.Login, commentID); err != nil {
		h.serverError(w)
		return
	}
	if err := reply("Reintento encolado para este pull request."); err != nil {
		h.serverError(w)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// commandSelection is the explicit target of a dismiss/confirm command: one
// finding id, or --all, plus an optional reason.
const bulkSelection = "--all"

var (
	// errCommandFindingUnknown rejects a named finding that does not exist or
	// belongs to another pull request instead of silently acting on the rest.
	errCommandFindingUnknown = errors.New("finding id not in this pull request")
	// errCommandNeedsSelection makes a bulk action explicit: a bare reason no
	// longer dismisses or confirms every finding.
	errCommandNeedsSelection = errors.New("command requires a finding id or --all")
)

// commandSetFindings applies dismiss/confirm to the finding the command names,
// or to every matching finding when the command selects --all.
func (h *WebhookHandler) commandSetFindings(ctx context.Context, w http.ResponseWriter, pr store.PullRequest,
	cmd qaCommand, commentID int64, actor qaActor, status string, reply func(string) error) {
	targets, reason, err := h.commandTargets(ctx, pr.ID, cmd.args, status)
	switch {
	case errors.Is(err, errCommandFindingUnknown):
		if err := reply("El hallazgo indicado no existe en este pull request. No se aplicó ningún cambio."); err != nil {
			h.serverError(w)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	case errors.Is(err, errCommandNeedsSelection):
		if err := reply(fmt.Sprintf(
			"Indica el ID del hallazgo o `%s` para todos. No se aplicó ningún cambio.", bulkSelection)); err != nil {
			h.serverError(w)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	case err != nil:
		h.serverError(w)
		return
	}
	if len(targets) == 0 {
		if err := reply("No hay hallazgos que coincidan con ese comando."); err != nil {
			h.serverError(w)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	action := "finding.withdrawn"
	if status == store.FindingOpen {
		action = "finding.confirmed"
	}
	audited := 0
	for _, finding := range targets {
		// /qa is a human triage like the panel: it records who decided and marks
		// the source, so the verifier never overrides it on the next run.
		changed, err := h.Store.TriageFinding(ctx, finding.ID, status, store.FindingAudit{
			Action:    action,
			Actor:     actor.Login,
			ActorID:   actorID(actor),
			CommentID: commentID,
			Reason:    reason,
			Source:    store.TriageSourceHuman,
		})
		if err != nil {
			h.serverError(w)
			return
		}
		if changed {
			audited++
		}
	}

	threads := h.applyThreads(ctx, pr, targets, status == store.FindingWithdrawn)
	if h.Findings != nil && audited > 0 {
		if err := h.Findings.Republish(ctx, pr.ID); err != nil {
			h.serverError(w)
			return
		}
	}

	ids := make([]string, 0, len(targets))
	for _, finding := range targets {
		ids = append(ids, "`"+finding.ID+"`")
	}
	verb := "descartado"
	if status == store.FindingOpen {
		verb = "confirmado"
	}
	body := fmt.Sprintf("Hallazgo(s) %s: %s.", verb, strings.Join(ids, ", "))
	if threads.failed > 0 {
		body += fmt.Sprintf(" Falló la actualización de %d hilo(s) en GitHub; se reintentará en la próxima revisión.", threads.failed)
	}
	if threads.skipped > 0 {
		body += fmt.Sprintf(" No pude actualizar %d hilo(s) en GitHub: el cliente no encontró el hilo del hallazgo o no está configurado.", threads.skipped)
	}
	if err := reply(body); err != nil {
		h.serverError(w)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// commandTargets resolves the findings a dismiss/confirm acts on. The command
// must name one finding of this pull request or select every finding with
// --all; anything else is rejected instead of widening the action. The reason
// is whatever follows the selection and is recorded in the audit row.
func (h *WebhookHandler) commandTargets(ctx context.Context, prID uint, args, status string) ([]store.Finding, string, error) {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return nil, "", errCommandNeedsSelection
	}
	selection := fields[0]
	reason := strings.Join(fields[1:], " ")

	if selection != bulkSelection {
		finding, err := h.Store.Finding(ctx, selection)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return nil, "", errCommandFindingUnknown
		case err != nil:
			return nil, "", err
		}
		if finding.PullRequestID != prID {
			return nil, "", errCommandFindingUnknown
		}
		// A resolved finding is no longer anyone's call: the panel refuses it
		// too, so the command reports nothing to do instead of failing.
		if finding.Status == status || finding.Status == store.FindingResolved {
			return nil, reason, nil
		}
		return []store.Finding{finding}, reason, nil
	}

	findings, err := h.Store.FindingsForPR(ctx, prID)
	if err != nil {
		return nil, "", err
	}
	wanted := store.FindingOpen
	if status == store.FindingOpen {
		wanted = store.FindingWithdrawn
	}
	targets := make([]store.Finding, 0, len(findings))
	for _, finding := range findings {
		if finding.Status == wanted {
			targets = append(targets, finding)
		}
	}
	return targets, reason, nil
}

type threadOutcome struct {
	// skipped counts findings published on GitHub whose thread could not be
	// updated: the client found no matching thread, or no publishing client is
	// configured. The reply names them instead of silently ignoring them.
	skipped int
	failed  int
}

// applyThreads mirrors a dismiss/confirm on the GitHub review threads that were
// published. A finding without thread coordinates has nothing to update there.
func (h *WebhookHandler) applyThreads(ctx context.Context, pr store.PullRequest, findings []store.Finding, resolve bool) threadOutcome {
	var out threadOutcome
	for _, finding := range findings {
		if finding.GithubCommentNodeID == "" && finding.GithubThreadID == "" {
			continue
		}
		if h.Findings == nil {
			out.skipped++
			continue
		}
		updated, err := h.Findings.SetFindingThread(ctx, pr.ID, finding, resolve)
		switch {
		case err != nil:
			out.failed++
		case !updated:
			out.skipped++
		}
	}
	return out
}

// canTriage authorizes a /qa command or a GitHub thread decision. Administrators
// and the lead or QA of the repository's team may act, matching the panel. The
// role comes from the installation data, keyed by the payload sender, never
// from the comment body.
func (h *WebhookHandler) canTriage(ctx context.Context, actor qaActor, pr store.PullRequest) bool {
	if actor.Login == "" || actor.User == nil {
		return false
	}
	if actor.User.IsAdmin {
		return true
	}
	role, err := h.Store.RepoRole(ctx, actor.User.ID, pr.RepositoryID)
	return err == nil && (role == "lead" || role == "qa")
}

// resolveActor maps the payload sender to the local user. An unknown account
// keeps its login so the reply can still name it, but has no permissions.
func (h *WebhookHandler) resolveActor(ctx context.Context, login string, githubID int64) qaActor {
	actor := qaActor{Login: login}
	if githubID == 0 {
		return actor
	}
	user, err := h.Store.UserByGitHubID(ctx, githubID)
	if err != nil {
		return actor
	}
	actor.User = &user
	return actor
}

func actorID(actor qaActor) *uint {
	if actor.User == nil {
		return nil
	}
	return &actor.User.ID
}

// parseQACommand extracts a /qa command from a comment body. Only the first
// line is considered so a quoted diff below it cannot trigger anything.
func parseQACommand(body string) (qaCommand, bool) {
	line := body
	if idx := strings.IndexByte(line, '\n'); idx >= 0 {
		line = line[:idx]
	}
	line = strings.TrimSpace(line)
	if len(line) < len(qaCommandPrefix) || !strings.EqualFold(line[:len(qaCommandPrefix)], qaCommandPrefix) {
		return qaCommand{}, false
	}
	// `/qax` is not a command: the prefix must end the word.
	rest := line[len(qaCommandPrefix):]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return qaCommand{}, false
	}
	rest = strings.TrimSpace(rest)
	name, args, _ := strings.Cut(rest, " ")
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return qaCommand{}, false
	}
	return qaCommand{name: name, args: strings.TrimSpace(args)}, true
}
