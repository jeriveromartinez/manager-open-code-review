package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
func (c *Checks) RefreshPR(ctx context.Context, prID uint, source string) (store.PRValidations, bool, error) {
	if c == nil || c.Store == nil {
		return store.PRValidations{}, false, errors.New("github: checks refresher has no store")
	}
	if c.Apps == nil {
		return store.PRValidations{}, false, errors.New("github: checks refresher has no app provider")
	}

	pr, err := c.pullRequest(ctx, prID)
	if err != nil {
		return store.PRValidations{}, false, err
	}
	if pr.HeadSHA == "" {
		return store.PRValidations{}, false, nil
	}
	repo, err := c.repository(ctx, pr.RepositoryID)
	if err != nil {
		return store.PRValidations{}, false, err
	}

	app, err := c.Apps.App(ctx)
	if err != nil {
		return store.PRValidations{}, false, fmt.Errorf("load GitHub App: %w", err)
	}
	client, err := app.InstallationClient(repo.InstallationID)
	if err != nil {
		return store.PRValidations{}, false, fmt.Errorf("create installation %d client: %w", repo.InstallationID, err)
	}
	return c.storeValidations(ctx, client, app.Creds.AppID, pr, repo, source)
}

// RefreshPRWith refreshes a pull request through an installation client the
// caller already holds, so reconciliation does not mint a second token per PR.
func (c *Checks) RefreshPRWith(ctx context.Context, client *gh.Client, pr store.PullRequest, source string) (store.PRValidations, bool, error) {
	if c == nil || c.Store == nil {
		return store.PRValidations{}, false, errors.New("github: checks refresher has no store")
	}
	if c.Apps == nil {
		return store.PRValidations{}, false, errors.New("github: checks refresher has no app provider")
	}
	if pr.HeadSHA == "" {
		return store.PRValidations{}, false, nil
	}
	repo, err := c.repository(ctx, pr.RepositoryID)
	if err != nil {
		return store.PRValidations{}, false, err
	}
	app, err := c.Apps.App(ctx)
	if err != nil {
		return store.PRValidations{}, false, fmt.Errorf("load GitHub App: %w", err)
	}
	return c.storeValidations(ctx, client, app.Creds.AppID, pr, repo, source)
}

func (c *Checks) pullRequest(ctx context.Context, prID uint) (store.PullRequest, error) {
	var pr store.PullRequest
	err := c.Store.DB.WithContext(ctx).Take(&pr, prID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return store.PullRequest{}, fmt.Errorf("pull request %d: %w", prID, store.ErrNotFound)
	}
	if err != nil {
		return store.PullRequest{}, fmt.Errorf("find pull request %d: %w", prID, err)
	}
	return pr, nil
}

func (c *Checks) repository(ctx context.Context, id int64) (store.Repository, error) {
	var repo store.Repository
	if err := c.Store.DB.WithContext(ctx).Take(&repo, id).Error; err != nil {
		return store.Repository{}, fmt.Errorf("find repository %d: %w", id, err)
	}
	return repo, nil
}

func (c *Checks) storeValidations(ctx context.Context, client *gh.Client, ownAppID int64, pr store.PullRequest, repo store.Repository, source string) (store.PRValidations, bool, error) {
	owner, name, ok := strings.Cut(repo.FullName, "/")
	if !ok || owner == "" || name == "" {
		return store.PRValidations{}, false, fmt.Errorf("invalid repository full name %q", repo.FullName)
	}
	// The stamp is taken before the lookup: a read that started earlier must
	// not overwrite a newer delivery that arrived while it was in flight.
	stamp := store.ObservationStamp{At: time.Now().UTC(), Source: source}
	state, observation, err := ReadObservation(ctx, client, owner, name, pr.HeadSHA, ownAppID)
	if err != nil {
		// The previous counters must not survive a failed lookup: the gate would
		// keep using a verdict GitHub did not confirm.
		if _, markErr := c.Store.SetPRValidationLookupFailure(ctx, pr.ID, pr.HeadSHA, stamp); markErr != nil {
			return store.PRValidations{}, false, errors.Join(err, markErr)
		}
		return store.PRValidations{}, false, err
	}
	changed, err := c.Store.SetPRValidationObservation(ctx, pr.ID, pr.HeadSHA, state, observation, stamp)
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
	state, _, err := ReadObservation(ctx, client, owner, repo, headSHA, ownAppID)
	return state, err
}

// ReadObservation reads the check runs and commit statuses of one commit and
// returns both the counters and the named observation the gate evaluates.
func ReadObservation(ctx context.Context, client *gh.Client, owner, repo, headSHA string, ownAppID int64) (store.PRValidations, store.ValidationObservation, error) {
	runs, err := listCheckRuns(ctx, client, owner, repo, headSHA)
	if err != nil {
		return store.PRValidations{}, store.ValidationObservation{}, fmt.Errorf("list check runs of %s/%s at %q: %w", owner, repo, headSHA, err)
	}
	statuses, err := listStatuses(ctx, client, owner, repo, headSHA)
	if err != nil {
		return store.PRValidations{}, store.ValidationObservation{}, fmt.Errorf("list commit statuses of %s/%s at %q: %w", owner, repo, headSHA, err)
	}
	observation := ObservationOf(runs, statuses, ownAppID)
	return ValidationsFor(observation), observation, nil
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
	return ValidationsFor(ObservationOf(runs, statuses, ownAppID))
}

// ValidationsFor derives the counters from a named observation, so both views
// of the same head always agree.
func ValidationsFor(observation store.ValidationObservation) store.PRValidations {
	var v store.PRValidations
	for _, check := range observation.Checks {
		v.Total++
		switch check.State {
		case store.CheckPassed:
			v.Passed++
		case store.CheckFailed:
			v.Failed++
		}
	}
	return v
}

// ObservationOf keeps the name, origin and state of every check of the head.
// Origin is "app:<id>" for a check run reported by an App and "status" for a
// legacy commit status, which lets the policy require a check by both name and
// origin. Check runs of ownAppID are skipped.
func ObservationOf(runs []*gh.CheckRun, statuses []*gh.RepoStatus, ownAppID int64) store.ValidationObservation {
	observation := store.ValidationObservation{Result: store.ObservationOK}
	for _, run := range runs {
		if run == nil {
			continue
		}
		if ownAppID != 0 && run.GetApp().GetID() == ownAppID {
			continue
		}
		state := store.CheckPending
		if run.GetStatus() == "completed" {
			switch run.GetConclusion() {
			case "success", "neutral", "skipped":
				state = store.CheckPassed
			default:
				state = store.CheckFailed
			}
		}
		observation.Checks = append(observation.Checks, store.CheckObservation{
			Name:   run.GetName(),
			Origin: checkRunOrigin(run.GetApp()),
			State:  state,
		})
	}
	for _, status := range statuses {
		if status == nil {
			continue
		}
		state := store.CheckFailed
		switch status.GetState() {
		case "success":
			state = store.CheckPassed
		case "pending":
			state = store.CheckPending
		}
		observation.Checks = append(observation.Checks, store.CheckObservation{
			Name:   status.GetContext(),
			Origin: "status",
			State:  state,
		})
	}
	return observation
}

func checkRunOrigin(app *gh.App) string {
	if app == nil || app.GetID() == 0 {
		return ""
	}
	return fmt.Sprintf("app:%d", app.GetID())
}
