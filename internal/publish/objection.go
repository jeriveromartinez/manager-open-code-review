package publish

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// PublishObjection posts the decision of an evaluated objection in its review
// thread and republishes the gate when it withdrew the finding. It is
// idempotent: the reply carries a marker that a retry finds before posting, and
// the intent is marked published only after GitHub accepted the reply.
func (w *Worker) PublishObjection(ctx context.Context, objectionID uint) error {
	intent, err := w.Store.FindingObjection(ctx, objectionID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if intent.State != store.ObjectionEvaluated {
		return nil
	}
	finding, err := w.Store.Finding(ctx, intent.FindingID)
	if errors.Is(err, store.ErrNotFound) {
		return w.Store.MarkFindingObjectionPublished(ctx, intent.ID)
	}
	if err != nil {
		return err
	}
	pr, err := loadPullRequest(ctx, w.Store, intent.PullRequestID)
	if err != nil {
		return err
	}
	repo, err := loadRepository(ctx, w.Store, pr.RepositoryID)
	if err != nil {
		return err
	}
	inst, err := loadInstallation(ctx, w.Store, repo.InstallationID)
	if err != nil {
		return err
	}
	app, err := w.Apps.App(ctx)
	if err != nil {
		return fmt.Errorf("load GitHub App: %w", err)
	}
	client, err := app.InstallationClient(inst.ID)
	if err != nil {
		return fmt.Errorf("create installation client: %w", err)
	}
	owner, name, err := splitFullName(repo.FullName)
	if err != nil {
		return err
	}
	p := &publishContext{
		w: w, pr: pr, app: app, client: client, inst: inst, owner: owner, name: name,
		redactor: NewRedactor(),
	}

	// A human decision between evaluation and publication wins: only apply the
	// withdrawal the worker proposed when the finding is still withdrawn, and
	// never publish a stale "retiré" reply otherwise.
	withdrawn := intent.Action == store.ObjectionWithdraw && intent.Apply && finding.Status == store.FindingWithdrawn
	superseded := intent.Action == store.ObjectionWithdraw && intent.Apply && !withdrawn
	if withdrawn {
		if err := w.Republish(ctx, pr.ID); err != nil {
			return err
		}
		if _, err := w.SetFindingThread(ctx, pr.ID, finding, true); err != nil {
			return err
		}
	}

	if finding.GithubCommentID != 0 {
		marker := objectionMarker(intent.ID)
		body := marker + "\n" + objectionReplyText(finding, intent, superseded)
		present, err := p.markerPresent(ctx, marker)
		if err != nil {
			return err
		}
		if !present {
			path := p.repoPath(fmt.Sprintf("pulls/%d/comments/%d/replies",
				pr.Number, finding.GithubCommentID))
			if err := p.request(ctx, http.MethodPost, path, map[string]string{"body": p.sanitize(body)}, nil); err != nil {
				return fmt.Errorf("reply to objection %d: %w", intent.ID, err)
			}
		}
	}
	return w.Store.MarkFindingObjectionPublished(ctx, intent.ID)
}

func objectionMarker(id uint) string {
	return fmt.Sprintf("<!-- qa-objection:%d -->", id)
}

// FailObjectionPublication makes an intent terminal after the retry budget, so
// a deleted thread or a permanent GitHub error is not re-enqueued forever.
func (w *Worker) FailObjectionPublication(ctx context.Context, objectionID uint, failure string) error {
	return w.Store.FailFindingObjectionPublication(ctx, objectionID, failure)
}

// objectionReplyText explains the decision where the objection was written.
func objectionReplyText(f store.Finding, intent store.FindingObjection, superseded bool) string {
	id := "`" + f.ID + "`"
	if superseded {
		return fmt.Sprintf("La decisión sobre el hallazgo %s fue reemplazada por una decisión posterior; no se aplicó el retiro automático.", id)
	}
	switch intent.Action {
	case store.ObjectionWithdraw:
		if intent.Apply {
			return fmt.Sprintf("Reevalué la objeción y retiré el hallazgo %s. Evidencia: `%s`.", id, intent.Evidence)
		}
		return fmt.Sprintf("La evidencia respalda retirar el hallazgo %s, pero la política no permite aplicarlo automáticamente. Evidencia: `%s`.", id, intent.Evidence)
	case store.ObjectionDowngrade:
		return fmt.Sprintf("La evidencia propone rebajar el hallazgo %s. El cambio de severidad requiere confirmación de un QA/lead.", id)
	case store.ObjectionUphold:
		return fmt.Sprintf("Reevalué la objeción y el hallazgo %s sigue en pie. Evidencia: `%s`.", id, intent.Evidence)
	case store.ObjectionIgnored:
		return fmt.Sprintf("Solo el autor del pull request, un admin, un lead o QA del equipo puede cambiar el gate con una objeción. El hallazgo %s no se modificó.", id)
	case store.ObjectionEscalate:
		return fmt.Sprintf("La objeción sobre el hallazgo %s requiere revisión de un QA/lead; el gate no se libera. %s", id, intent.Reason)
	default:
		return fmt.Sprintf("La objeción sobre el hallazgo %s quedó registrada. %s", id, intent.Reason)
	}
}
