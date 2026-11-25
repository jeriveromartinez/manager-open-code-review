// Package syncer keeps the local database in sync with GitHub App
// installations: their visible repositories and the open pull requests of the
// repositories an administrator has enabled.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v93/github"
	"gorm.io/gorm"

	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const perPage = 100

// Syncer reconciles one GitHub App installation at a time and publishes the
// resulting live-update events.
type Syncer struct {
	Store  *store.Store
	Apps   github.AppProvider
	Events events.Publisher
	Checks ChecksRefresher
	// ReviewRequests handles external contributions the engine may not review.
	// Polling mode has no webhook delivery, so the syncer is the only path that
	// can hand those pull requests to a person.
	ReviewRequests *github.ReviewRequestService
}

// ChecksRefresher re-reads the CI state of a pull request head and stores it.
// Reconciliation uses it so polling mode gates reviews on the same verdict a
// webhook delivery would have stored.
type ChecksRefresher interface {
	RefreshPRWith(ctx context.Context, client *gh.Client, pr store.PullRequest) (store.PRValidations, bool, error)
}

// SyncInstallation reconciles the repositories visible to an installation and,
// for every enabled repository, its open pull requests. It is a no-op when the
// installation is suspended or no longer exists on GitHub.
//
// A job can run before the installation.created webhook has stored the row
// (setup enqueues it right after the App is installed), so a missing row is
// recovered from GitHub instead of being skipped.
func (s *Syncer) SyncInstallation(ctx context.Context, installationID int64) error {
	inst, err := s.loadInstallation(ctx, installationID)
	if err != nil {
		return err
	}
	if inst == nil || inst.SuspendedAt != nil {
		return nil
	}

	app, err := s.Apps.App(ctx)
	if err != nil {
		return fmt.Errorf("load GitHub App: %w", err)
	}
	client, err := app.InstallationClient(installationID)
	if err != nil {
		return fmt.Errorf("create installation %d client: %w", installationID, err)
	}

	repos, err := listInstallationRepos(ctx, client)
	if err != nil {
		return fmt.Errorf("list repositories of installation %d: %w", installationID, err)
	}
	if err := s.syncRepositories(ctx, installationID, repos); err != nil {
		return err
	}
	return s.syncPullRequests(ctx, client, installationID)
}

// loadInstallation returns the stored row, recovering it from GitHub when the
// installation webhook has not inserted it yet. A nil result means GitHub does
// not know the installation.
func (s *Syncer) loadInstallation(ctx context.Context, installationID int64) (*store.Installation, error) {
	var inst store.Installation
	err := s.Store.DB.WithContext(ctx).Where("id = ?", installationID).Take(&inst).Error
	if err == nil {
		return &inst, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("load installation %d: %w", installationID, err)
	}
	return s.fetchInstallation(ctx, installationID)
}

func (s *Syncer) fetchInstallation(ctx context.Context, installationID int64) (*store.Installation, error) {
	app, err := s.Apps.App(ctx)
	if err != nil {
		return nil, fmt.Errorf("load GitHub App: %w", err)
	}
	client, err := app.AppClient()
	if err != nil {
		return nil, fmt.Errorf("create GitHub App client: %w", err)
	}
	remote, _, err := client.Apps.GetInstallation(ctx, installationID)
	if err != nil {
		// A 404 means the installation was removed (or never existed), so there
		// is nothing to sync; retrying would never succeed.
		var respErr *gh.ErrorResponse
		if errors.As(err, &respErr) && respErr.Response != nil && respErr.Response.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("get installation %d from GitHub: %w", installationID, err)
	}
	inst := github.InstallationFromGitHub(remote)
	if err := s.Store.UpsertInstallation(ctx, inst); err != nil {
		return nil, err
	}
	return &inst, nil
}

// ReconcileAll discovers installations from the GitHub App, prunes rows for
// installations no longer present after a complete discovery, and syncs every
// known installation. On discovery failure it falls back to the DB list without
// deleting anything. A failure in one installation sync is logged and does not
// prevent the remaining ones from being attempted.
func (s *Syncer) ReconcileAll(ctx context.Context) error {
	installations, err := s.reconcileInstallations(ctx)
	if err != nil {
		return err
	}
	for _, installation := range installations {
		if err := s.SyncInstallation(ctx, installation.ID); err != nil {
			slog.Error("sync installation failed",
				"installation_id", installation.ID, "error", err)
		}
	}
	return nil
}

func (s *Syncer) reconcileInstallations(ctx context.Context) ([]store.Installation, error) {
	app, err := s.Apps.App(ctx)
	if err != nil {
		slog.Error("discover installations failed", "error", err)
		installations, listErr := s.Store.ListInstallations(ctx)
		if listErr != nil {
			return nil, fmt.Errorf("list installations: %w", listErr)
		}
		return installations, nil
	}

	remote, err := s.listAppInstallations(ctx, app)
	if err != nil {
		slog.Error("discover installations failed", "error", err)
		installations, listErr := s.Store.ListInstallations(ctx)
		if listErr != nil {
			return nil, fmt.Errorf("list installations: %w", listErr)
		}
		return installations, nil
	}

	remoteIDs := make(map[int64]struct{}, len(remote))
	for _, inst := range remote {
		remoteIDs[inst.ID] = struct{}{}
		if err := s.Store.UpsertInstallation(ctx, inst); err != nil {
			return nil, fmt.Errorf("upsert installation %d: %w", inst.ID, err)
		}
	}

	current, err := s.Store.ListInstallations(ctx)
	if err != nil {
		return nil, fmt.Errorf("list installations: %w", err)
	}
	for _, inst := range current {
		if _, ok := remoteIDs[inst.ID]; ok {
			continue
		}
		if err := s.Store.DeleteInstallation(ctx, inst.ID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("delete installation %d: %w", inst.ID, err)
		}
	}

	installations, err := s.Store.ListInstallations(ctx)
	if err != nil {
		return nil, fmt.Errorf("list installations: %w", err)
	}
	return installations, nil
}

func (s *Syncer) listAppInstallations(ctx context.Context, app *github.App) ([]store.Installation, error) {
	client, err := app.AppClient()
	if err != nil {
		return nil, err
	}

	var all []store.Installation
	opts := &gh.ListOptions{PerPage: perPage}
	for {
		installs, resp, err := client.Apps.ListInstallations(ctx, opts)
		if err != nil {
			return nil, err
		}
		for _, inst := range installs {
			all = append(all, github.InstallationFromGitHub(inst))
		}
		if resp == nil || resp.NextPage == 0 || resp.NextPage <= opts.Page {
			return all, nil
		}
		opts.Page = resp.NextPage
	}
}

// listInstallationRepos follows the Link header until GitHub stops reporting a
// next page. A server repeating a page cannot trap us in an endless loop.
func listInstallationRepos(ctx context.Context, client *gh.Client) ([]*gh.Repository, error) {
	var all []*gh.Repository
	opts := &gh.ListOptions{PerPage: perPage}
	for {
		list, resp, err := client.Apps.ListRepos(ctx, opts)
		if err != nil {
			return nil, err
		}
		if list != nil {
			all = append(all, list.Repositories...)
		}
		if resp == nil || resp.NextPage == 0 || resp.NextPage <= opts.Page {
			return all, nil
		}
		opts.Page = resp.NextPage
	}
}

// listOpenPullRequests reads every page of open pull requests of one repository.
func listOpenPullRequests(ctx context.Context, client *gh.Client, owner, name string) ([]*gh.PullRequest, error) {
	var all []*gh.PullRequest
	opts := &gh.PullRequestListOptions{
		State:       "open",
		ListOptions: gh.ListOptions{PerPage: perPage},
	}
	for {
		pulls, resp, err := client.PullRequests.List(ctx, owner, name, opts)
		if err != nil {
			return nil, err
		}
		all = append(all, pulls...)
		if resp == nil || resp.NextPage == 0 || resp.NextPage <= opts.Page {
			return all, nil
		}
		opts.Page = resp.NextPage
	}
}

func (s *Syncer) syncRepositories(ctx context.Context, installationID int64, repos []*gh.Repository) error {
	seen := make(map[int64]struct{}, len(repos))
	ids := make([]int64, 0, len(repos))
	for _, r := range repos {
		if r == nil || r.GetID() == 0 {
			continue
		}
		seen[r.GetID()] = struct{}{}
		ids = append(ids, r.GetID())
	}

	known, err := s.existingRepositoryIDs(ctx, ids)
	if err != nil {
		return err
	}

	for _, r := range repos {
		if r == nil || r.GetID() == 0 {
			continue
		}
		id := r.GetID()
		if err := s.Store.UpsertRepository(ctx, store.Repository{
			ID:             id,
			InstallationID: installationID,
			FullName:       r.GetFullName(),
			Private:        r.GetPrivate(),
			DefaultBranch:  r.GetDefaultBranch(),
		}); err != nil {
			return fmt.Errorf("upsert repository %d: %w", id, err)
		}
		if _, ok := known[id]; !ok {
			if err := s.publish(ctx, events.Event{Type: "repo.updated", ID: strconv.FormatInt(id, 10)}); err != nil {
				return err
			}
		}
	}

	current, err := s.installationRepositoryIDs(ctx, installationID)
	if err != nil {
		return err
	}
	for _, id := range current {
		if _, ok := seen[id]; ok {
			continue
		}
		if err := s.Store.RemoveRepository(ctx, id); err != nil {
			return fmt.Errorf("remove repository %d: %w", id, err)
		}
		if err := s.publish(ctx, events.Event{Type: "repo.updated", ID: strconv.FormatInt(id, 10)}); err != nil {
			return err
		}
	}
	return nil
}

// existingRepositoryIDs reports which of the given GitHub repository IDs are
// already known to the instance.
func (s *Syncer) existingRepositoryIDs(ctx context.Context, ids []int64) (map[int64]struct{}, error) {
	known := make(map[int64]struct{}, len(ids))
	if len(ids) == 0 {
		return known, nil
	}
	var found []int64
	if err := s.Store.DB.WithContext(ctx).Model(&store.Repository{}).
		Where("id IN ?", ids).Pluck("id", &found).Error; err != nil {
		return nil, fmt.Errorf("list known repositories: %w", err)
	}
	for _, id := range found {
		known[id] = struct{}{}
	}
	return known, nil
}

// installationRepositoryIDs reads the current local repository list for an
// installation straight from the store's DB handle.
func (s *Syncer) installationRepositoryIDs(ctx context.Context, installationID int64) ([]int64, error) {
	var ids []int64
	if err := s.Store.DB.WithContext(ctx).Model(&store.Repository{}).
		Where("installation_id = ?", installationID).Pluck("id", &ids).Error; err != nil {
		return nil, fmt.Errorf("list repositories of installation %d: %w", installationID, err)
	}
	return ids, nil
}

func (s *Syncer) syncPullRequests(ctx context.Context, client *gh.Client, installationID int64) error {
	repos, err := s.Store.EnabledRepositories(ctx, installationID)
	if err != nil {
		return fmt.Errorf("list enabled repositories of installation %d: %w", installationID, err)
	}
	var errs []error
	for _, repo := range repos {
		if err := s.syncRepositoryPullRequests(ctx, client, repo); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// syncRepositoryPullRequests reconciles one repository. The caller keeps
// walking the rest of the installation after an error: a repository GitHub
// cannot be read from must not starve its siblings. Closing is skipped when the
// listing or a pull request sync failed, because the snapshot would be
// incomplete and valid rows could be closed.
func (s *Syncer) syncRepositoryPullRequests(ctx context.Context, client *gh.Client, repo store.Repository) error {
	owner, name, ok := strings.Cut(repo.FullName, "/")
	if !ok || owner == "" || name == "" {
		slog.Warn("skip repository with invalid full name", "repository_id", repo.ID, "full_name", repo.FullName)
		return nil
	}
	pulls, err := listOpenPullRequests(ctx, client, owner, name)
	if err != nil {
		return fmt.Errorf("list open pull requests of %s: %w", repo.FullName, err)
	}

	openNumbers := make([]int, 0, len(pulls))
	for _, pull := range pulls {
		if pull == nil || pull.GetNumber() == 0 {
			continue
		}
		openNumbers = append(openNumbers, pull.GetNumber())
		pr, changed, err := s.Store.SyncPullRequest(ctx, github.PRFromGitHub(repo.ID, repo.FullName, pull))
		if err != nil {
			return fmt.Errorf("sync pull request #%d of %s: %w", pull.GetNumber(), repo.FullName, err)
		}
		if pr.Origin == store.OriginUnknown {
			slog.Warn("pull request origin could not be determined",
				"repository", repo.FullName, "pull_request", pr.Number,
				"head_repository", pr.HeadRepoFullName)
		}
		if changed {
			if err := s.publish(ctx, events.Event{
				Type: "pr.updated",
				ID:   strconv.FormatUint(uint64(pr.ID), 10),
			}); err != nil {
				return err
			}
		}
		if err := s.refreshChecks(ctx, client, pr); err != nil {
			slog.Error("refresh pull request checks failed",
				"pull_request_id", pr.ID, "head_sha", pr.HeadSHA, "error", err)
		}
		s.requestForkReview(ctx, pr)
	}

	openNumbers, revalidateErr := s.revalidateAbsentPullRequests(ctx, client, owner, name, repo, openNumbers)
	closeErr := s.Store.CloseMissingPullRequests(ctx, repo.ID, openNumbers)
	if closeErr != nil {
		closeErr = fmt.Errorf("close missing pull requests of %s: %w", repo.FullName, closeErr)
	}
	return errors.Join(revalidateErr, closeErr)
}

// revalidateAbsentPullRequests re-checks every locally open pull request that
// was absent from a successful paginated listing before the sweep closes it. A
// pull request opened while the pages were being read is missing from the
// snapshot but still open, so closing it would cancel live work. A 404 confirms
// it is gone; any other read failure keeps the row open and retries on the next
// sweep.
func (s *Syncer) revalidateAbsentPullRequests(ctx context.Context, client *gh.Client, owner, name string, repo store.Repository, openNumbers []int) ([]int, error) {
	missing, err := s.Store.MissingOpenPullRequests(ctx, repo.ID, openNumbers)
	if err != nil {
		return openNumbers, fmt.Errorf("list missing pull requests of %s: %w", repo.FullName, err)
	}
	var errs []error
	for _, pr := range missing {
		pull, resp, getErr := client.PullRequests.Get(ctx, owner, name, pr.Number)
		if getErr != nil {
			if resp != nil && resp.StatusCode == http.StatusNotFound {
				continue
			}
			openNumbers = append(openNumbers, pr.Number)
			errs = append(errs, fmt.Errorf("re-check pull request #%d of %s: %w", pr.Number, repo.FullName, getErr))
			continue
		}
		if pull == nil || strings.EqualFold(pull.GetState(), "open") {
			openNumbers = append(openNumbers, pr.Number)
		}
	}
	return openNumbers, errors.Join(errs...)
}

// refreshChecks keeps the stored counters current in polling mode. A failure is
// logged and never aborts the reconciliation: a GitHub hiccup on one PR must not
// stop the rest of the installation from syncing, and the next pass retries.
// Drafts are measured too, so a PR that becomes ready for review is gated on a
// verdict that already exists instead of waiting for the next pass.
func (s *Syncer) refreshChecks(ctx context.Context, client *gh.Client, pr store.PullRequest) error {
	if s.Checks == nil || pr.HeadSHA == "" {
		return nil
	}
	_, changed, err := s.Checks.RefreshPRWith(ctx, client, pr)
	if err != nil {
		return err
	}
	if changed {
		return s.publish(ctx, events.Event{
			Type: "pr.updated",
			ID:   strconv.FormatUint(uint64(pr.ID), 10),
		})
	}
	return nil
}

// requestForkReview hands an external contribution the engine may not review
// to a person. Authorized contributions are left to the pending-review poller,
// which applies size, rate and spend limits again before queueing. The request
// is deduplicated by pull request and head, so walking the open pull requests
// on every poll is cheap and never asks GitHub twice. A failure is logged and
// does not abort the rest of the installation.
func (s *Syncer) requestForkReview(ctx context.Context, pr store.PullRequest) {
	if s.ReviewRequests == nil || !pr.External() || !pr.IsOpen() || pr.Draft || pr.HeadSHA == "" {
		return
	}
	cfg, err := s.Store.ReviewRequestConfigForPR(ctx, pr.ID)
	if errors.Is(err, store.ErrNoReviewConfig) {
		return
	}
	if err != nil {
		slog.Error("load external review request config failed",
			"pull_request_id", pr.ID, "error", err)
		return
	}
	usage, err := s.Store.ExternalReviewUsage(ctx, pr, *cfg.Repo.TeamID)
	if err != nil {
		slog.Error("measure external contribution failed",
			"pull_request_id", pr.ID, "error", err)
		return
	}
	decision := store.DecideExternalReview(pr, cfg.Policy, usage)
	if decision.Allow {
		return
	}
	if _, err := s.ReviewRequests.Request(ctx, cfg, decision.Cause); err != nil {
		slog.Error("request human review of external contribution failed",
			"pull_request_id", pr.ID, "head_sha", pr.HeadSHA, "cause", decision.Cause, "error", err)
	}
}

func (s *Syncer) publish(ctx context.Context, ev events.Event) error {
	if s.Events == nil {
		return nil
	}
	if err := s.Events.Publish(ctx, ev); err != nil {
		return fmt.Errorf("publish %s %s: %w", ev.Type, ev.ID, err)
	}
	return nil
}
