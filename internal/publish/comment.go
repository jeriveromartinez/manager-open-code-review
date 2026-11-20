package publish

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"gorm.io/gorm"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// CommentOnPullRequest posts an issue comment on a pull request. The webhook
// handler replies to /qa commands through it; the body is redacted and its
// mentions escaped like every other published text.
func (w *Worker) CommentOnPullRequest(ctx context.Context, repoFullName string, number int, body string) error {
	var repo store.Repository
	err := w.Store.DB.WithContext(ctx).Where("full_name = ?", repoFullName).Take(&repo).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("repository %s: %w", repoFullName, store.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("find repository %s: %w", repoFullName, err)
	}

	app, err := w.Apps.App(ctx)
	if err != nil {
		return fmt.Errorf("load GitHub App: %w", err)
	}
	client, err := app.InstallationClient(repo.InstallationID)
	if err != nil {
		return fmt.Errorf("create installation client: %w", err)
	}
	owner, name, err := splitFullName(repo.FullName)
	if err != nil {
		return err
	}

	p := &publishContext{
		w: w, app: app, client: client, owner: owner, name: name, redactor: NewRedactor(),
	}
	path := p.repoPath(fmt.Sprintf("issues/%d/comments", number))
	return p.request(ctx, http.MethodPost, path, map[string]string{"body": p.sanitize(body)}, nil)
}
