// Package publish writes QA results back to GitHub: checks, comments and
// reviews. Worker owns the GitHub App and the store needed to do so.
package publish

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v93/github"
	"gorm.io/gorm"

	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
	appgithub "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const (
	checkRunName         = "QA Control Center"
	cancelErrorBodyLimit = 300
)

// Worker publishes review artefacts for a run.
type Worker struct {
	Store     *store.Store
	Apps      appgithub.AppProvider
	Box       *crypto.Box
	Events    events.Publisher
	PublicURL string
}

// StartCheck opens (or reopens) the QA check run for a review run's head SHA and
// records the resulting check in the artifact table. An existing check is
// PATCHed back to in_progress so a retried run does not leave duplicate checks.
func (w *Worker) StartCheck(ctx context.Context, runID uint) error {
	run, err := w.Store.ReviewRun(ctx, runID)
	if err != nil {
		return err
	}
	pr, err := loadPullRequest(ctx, w.Store, run.PullRequestID)
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
	externalID := strconv.FormatUint(uint64(run.ID), 10)

	artifact, err := w.Store.ArtifactFor(ctx, pr.ID, store.ArtifactCheck, run.HeadSHA)
	switch {
	case err == nil:
		return w.restartCheck(ctx, client, owner, name, externalID, artifact)
	case errors.Is(err, store.ErrNotFound):
		return w.createCheck(ctx, client, owner, name, pr.ID, run.HeadSHA, externalID)
	default:
		return err
	}
}

func (w *Worker) createCheck(ctx context.Context, client *gh.Client, owner, name string, prID uint, headSHA, externalID string) error {
	check, _, err := client.Checks.CreateCheckRun(ctx, owner, name, gh.CreateCheckRunOptions{
		Name:       checkRunName,
		HeadSHA:    headSHA,
		ExternalID: gh.Ptr(externalID),
		Status:     gh.Ptr("in_progress"),
	})
	if err != nil {
		return fmt.Errorf("create check run: %w", err)
	}
	if check == nil || check.GetID() == 0 {
		return fmt.Errorf("create check run: GitHub returned no check run id")
	}
	if _, err := w.Store.UpsertArtifact(ctx, store.GitHubArtifact{
		PullRequestID: prID,
		Kind:          store.ArtifactCheck,
		HeadSHA:       headSHA,
		ExternalID:    strconv.FormatInt(check.GetID(), 10),
		URL:           check.GetHTMLURL(),
	}); err != nil {
		return fmt.Errorf("save check run artifact: %w", err)
	}
	return nil
}

func (w *Worker) restartCheck(ctx context.Context, client *gh.Client, owner, name, externalID string, artifact store.GitHubArtifact) error {
	checkID, err := strconv.ParseInt(artifact.ExternalID, 10, 64)
	if err != nil {
		return fmt.Errorf("check artifact external id %q: %w", artifact.ExternalID, err)
	}
	check, _, err := client.Checks.UpdateCheckRun(ctx, owner, name, checkID, gh.UpdateCheckRunOptions{
		Name:       checkRunName,
		Status:     gh.Ptr("in_progress"),
		ExternalID: gh.Ptr(externalID),
	})
	if err != nil {
		return fmt.Errorf("reopen check run %d: %w", checkID, err)
	}
	artifactURL := artifact.URL
	if check != nil && check.GetHTMLURL() != "" {
		artifactURL = check.GetHTMLURL()
	}
	if _, err := w.Store.UpsertArtifact(ctx, store.GitHubArtifact{
		PullRequestID: artifact.PullRequestID,
		Kind:          artifact.Kind,
		HeadSHA:       artifact.HeadSHA,
		ExternalID:    artifact.ExternalID,
		URL:           artifactURL,
	}); err != nil {
		return fmt.Errorf("refresh check run artifact: %w", err)
	}
	return nil
}

// CancelCheck marks the run's check run as cancelled. It uses the artifact
// recorded by StartCheck so no GitHub lookup is needed.
func (w *Worker) CancelCheck(ctx context.Context, runID uint) error {
	run, err := w.Store.ReviewRun(ctx, runID)
	if err != nil {
		return err
	}
	pr, err := loadPullRequest(ctx, w.Store, run.PullRequestID)
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
	artifact, err := w.Store.ArtifactFor(ctx, pr.ID, store.ArtifactCheck, run.HeadSHA)
	if err != nil {
		return err
	}
	checkID, err := strconv.ParseInt(artifact.ExternalID, 10, 64)
	if err != nil {
		return fmt.Errorf("check artifact external id %q: %w", artifact.ExternalID, err)
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

	const body = `{"status":"completed","conclusion":"cancelled"}`
	endpoint := fmt.Sprintf("%s/repos/%s/%s/check-runs/%d",
		strings.TrimRight(app.APIURL, "/"), url.PathEscape(owner), url.PathEscape(name), checkID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("build cancel check request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Client().Do(req)
	if err != nil {
		return fmt.Errorf("cancel check run %d: %w", checkID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		payload, readErr := io.ReadAll(io.LimitReader(resp.Body, cancelErrorBodyLimit))
		if readErr != nil {
			return fmt.Errorf("cancel check run %d: GitHub API returned status %d", checkID, resp.StatusCode)
		}
		return fmt.Errorf("cancel check run %d: GitHub API returned status %d: %s", checkID, resp.StatusCode, payload)
	}
	return nil
}

func splitFullName(fullName string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok || owner == "" || name == "" {
		return "", "", fmt.Errorf("invalid repository full name %q", fullName)
	}
	return owner, name, nil
}

func loadPullRequest(ctx context.Context, st *store.Store, id uint) (store.PullRequest, error) {
	var pr store.PullRequest
	if err := st.DB.WithContext(ctx).Take(&pr, id).Error; err != nil {
		return store.PullRequest{}, rowError("pull request", id, err)
	}
	return pr, nil
}

func loadRepository(ctx context.Context, st *store.Store, id int64) (store.Repository, error) {
	var repo store.Repository
	if err := st.DB.WithContext(ctx).Take(&repo, id).Error; err != nil {
		return store.Repository{}, rowError("repository", id, err)
	}
	return repo, nil
}

func loadInstallation(ctx context.Context, st *store.Store, id int64) (store.Installation, error) {
	var inst store.Installation
	if err := st.DB.WithContext(ctx).Take(&inst, id).Error; err != nil {
		return store.Installation{}, rowError("installation", id, err)
	}
	return inst, nil
}

func rowError(kind string, id any, err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("%s %v: %w", kind, id, store.ErrNotFound)
	}
	return fmt.Errorf("find %s %v: %w", kind, id, err)
}
