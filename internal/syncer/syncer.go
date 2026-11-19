// Package syncer keeps the local database in sync with GitHub App
// installations: their visible repositories and the open pull requests of the
// repositories an administrator has enabled.
package syncer

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v93/github"

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
}

// SyncInstallation reconciles the repositories visible to an installation and,
// for every enabled repository, its open pull requests.
func (s *Syncer) SyncInstallation(ctx context.Context, installationID int64) error {
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

// ReconcileAll syncs every known installation. A failure in one installation is
// logged and does not prevent the remaining ones from being attempted.
func (s *Syncer) ReconcileAll(ctx context.Context) error {
	installations, err := s.Store.ListInstallations(ctx)
	if err != nil {
		return fmt.Errorf("list installations: %w", err)
	}
	for _, installation := range installations {
		if err := s.SyncInstallation(ctx, installation.ID); err != nil {
			slog.Error("sync installation failed",
				"installation_id", installation.ID, "error", err)
		}
	}
	return nil
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
	for _, repo := range repos {
		owner, name, ok := strings.Cut(repo.FullName, "/")
		if !ok || owner == "" || name == "" {
			slog.Warn("skip repository with invalid full name", "repository_id", repo.ID, "full_name", repo.FullName)
			continue
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
			pr, changed, err := s.Store.SyncPullRequest(ctx, github.PRFromGitHub(repo.ID, pull))
			if err != nil {
				return fmt.Errorf("sync pull request #%d of %s: %w", pull.GetNumber(), repo.FullName, err)
			}
			if changed {
				if err := s.publish(ctx, events.Event{
					Type: "pr.updated",
					ID:   strconv.FormatUint(uint64(pr.ID), 10),
				}); err != nil {
					return err
				}
			}
		}
		if err := s.Store.CloseMissingPullRequests(ctx, repo.ID, openNumbers); err != nil {
			return fmt.Errorf("close missing pull requests of %s: %w", repo.FullName, err)
		}
	}
	return nil
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
