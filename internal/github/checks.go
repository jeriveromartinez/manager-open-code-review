package github

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v93/github"
	"gorm.io/gorm"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// Checks refreshes the CI state of a pull request head. The counters are
// persisted on the pull request row so the API can render them without calling
// GitHub on every request.
type Checks struct {
	Store *store.Store
	Apps  AppProvider
}

// RefreshPR reads the check runs and commit statuses of the pull request's
// current head SHA and stores the resulting counters. changed reports whether
// the stored state moved, which is what makes a delivery worth republishing.
func (c *Checks) RefreshPR(ctx context.Context, prID uint) (store.PRValidations, bool, error) {
	if c == nil || c.Store == nil {
		return store.PRValidations{}, false, errors.New("github: checks refresher has no store")
	}
	if c.Apps == nil {
		return store.PRValidations{}, false, errors.New("github: checks refresher has no app provider")
	}

	var pr store.PullRequest
	err := c.Store.DB.WithContext(ctx).Take(&pr, prID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return store.PRValidations{}, false, fmt.Errorf("pull request %d: %w", prID, store.ErrNotFound)
	}
	if err != nil {
		return store.PRValidations{}, false, fmt.Errorf("find pull request %d: %w", prID, err)
	}
	if pr.HeadSHA == "" {
		return store.PRValidations{}, false, nil
	}

	var repo store.Repository
	if err := c.Store.DB.WithContext(ctx).Take(&repo, pr.RepositoryID).Error; err != nil {
		return store.PRValidations{}, false, fmt.Errorf("find repository %d: %w", pr.RepositoryID, err)
	}
	owner, name, ok := strings.Cut(repo.FullName, "/")
	if !ok || owner == "" || name == "" {
		return store.PRValidations{}, false, fmt.Errorf("invalid repository full name %q", repo.FullName)
	}

	app, err := c.Apps.App(ctx)
	if err != nil {
		return store.PRValidations{}, false, fmt.Errorf("load GitHub App: %w", err)
	}
	client, err := app.InstallationClient(repo.InstallationID)
	if err != nil {
		return store.PRValidations{}, false, fmt.Errorf("create installation %d client: %w", repo.InstallationID, err)
	}

	state, err := ReadValidations(ctx, client, owner, name, pr.HeadSHA, app.Creds.AppID)
	if err != nil {
		return store.PRValidations{}, false, err
	}
	changed, err := c.Store.SetPRValidations(ctx, prID, pr.HeadSHA, state)
	if err != nil {
		return store.PRValidations{}, false, err
	}
	return state, changed, nil
}

// ReadValidations reads the check runs and commit statuses GitHub reports for
// one commit and aggregates them. ownAppID is the QA App's id: its own check run
// is a result, not an input, so counting it would let a review block the next
// review of the same commit.
func ReadValidations(ctx context.Context, client *gh.Client, owner, repo, headSHA string, ownAppID int64) (store.PRValidations, error) {
	runs, err := listCheckRuns(ctx, client, owner, repo, headSHA)
	if err != nil {
		return store.PRValidations{}, fmt.Errorf("list check runs of %s/%s at %q: %w", owner, repo, headSHA, err)
	}
	statuses, err := listStatuses(ctx, client, owner, repo, headSHA)
	if err != nil {
		return store.PRValidations{}, fmt.Errorf("list commit statuses of %s/%s at %q: %w", owner, repo, headSHA, err)
	}
	return ValidationsOf(runs, statuses, ownAppID), nil
}

// listCheckRuns follows the pagination of the check runs endpoint. A commit can
// easily exceed one page, and a partial list would make the gate see fewer
// failures than there are.
func listCheckRuns(ctx context.Context, client *gh.Client, owner, repo, ref string) ([]*gh.CheckRun, error) {
	var all []*gh.CheckRun
	opts := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		results, resp, err := client.Checks.ListCheckRunsForRef(ctx, owner, repo, ref, opts)
		if err != nil {
			return nil, err
		}
		if results != nil {
			all = append(all, results.CheckRuns...)
			if results.GetTotal() <= len(all) {
				return all, nil
			}
		}
		if resp == nil || resp.NextPage == 0 || resp.NextPage <= opts.Page {
			return all, nil
		}
		opts.Page = resp.NextPage
	}
}

// listStatuses reads the legacy commit statuses. GitHub already collapses them
// to the most recent state per context in the combined status.
func listStatuses(ctx context.Context, client *gh.Client, owner, repo, ref string) ([]*gh.RepoStatus, error) {
	var all []*gh.RepoStatus
	opts := &gh.ListOptions{PerPage: 100}
	for {
		combined, resp, err := client.Repositories.GetCombinedStatus(ctx, owner, repo, ref, opts)
		if err != nil {
			return nil, err
		}
		if combined != nil {
			all = append(all, combined.Statuses...)
			if combined.GetTotalCount() <= len(all) {
				return all, nil
			}
		}
		if resp == nil || resp.NextPage == 0 || resp.NextPage <= opts.Page {
			return all, nil
		}
		opts.Page = resp.NextPage
	}
}

// ValidationsOf aggregates check runs and commit statuses. A check counts as
// passed when it finished green, as failed when it finished any other way and
// as pending while it is still queued or running. Check runs of ownAppID are
// skipped.
func ValidationsOf(runs []*gh.CheckRun, statuses []*gh.RepoStatus, ownAppID int64) store.PRValidations {
	var v store.PRValidations
	for _, run := range runs {
		if run == nil {
			continue
		}
		if ownAppID != 0 && run.GetApp().GetID() == ownAppID {
			continue
		}
		v.Total++
		if run.GetStatus() != "completed" {
			continue
		}
		switch run.GetConclusion() {
		case "success", "neutral", "skipped":
			v.Passed++
		default:
			v.Failed++
		}
	}
	for _, status := range statuses {
		if status == nil {
			continue
		}
		v.Total++
		switch status.GetState() {
		case "success":
			v.Passed++
		case "pending":
		default:
			v.Failed++
		}
	}
	return v
}
