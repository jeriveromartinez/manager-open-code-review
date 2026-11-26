package store_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

// fixture is a migrated database plus helpers to seed the rows the store
// methods under test depend on.
type fixture struct {
	t   *testing.T
	ctx context.Context
	s   *store.Store
	seq int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{t: t, ctx: context.Background(), s: testutil.NewStore(t)}
}

func (f *fixture) nextID() int64 {
	f.seq++
	return f.seq
}

func (f *fixture) installation(id int64) store.Installation {
	f.t.Helper()

	in := store.Installation{ID: id, AccountLogin: "acme", AccountType: "Organization"}
	if err := f.s.UpsertInstallation(f.ctx, in); err != nil {
		f.t.Fatalf("UpsertInstallation(%d): %v", id, err)
	}
	return in
}

func (f *fixture) repo(id, installationID int64, fullName string, teamID *uint, enabled bool) store.Repository {
	f.t.Helper()

	r := store.Repository{
		ID:             id,
		InstallationID: installationID,
		FullName:       fullName,
		DefaultBranch:  "main",
		Enabled:        enabled,
		TeamID:         teamID,
	}
	if err := f.s.UpsertRepository(f.ctx, r); err != nil {
		f.t.Fatalf("UpsertRepository(%q): %v", fullName, err)
	}
	return r
}

func (f *fixture) team(name string) store.Team {
	f.t.Helper()

	tm := store.Team{Name: name, Slug: strings.ToLower(name)}
	if err := f.s.DB.WithContext(f.ctx).Create(&tm).Error; err != nil {
		f.t.Fatalf("create team %q: %v", name, err)
	}
	return tm
}

func (f *fixture) user(login string, admin bool) store.User {
	f.t.Helper()

	u := store.User{GitHubID: f.nextID(), Login: login, IsAdmin: admin}
	if err := f.s.DB.WithContext(f.ctx).Create(&u).Error; err != nil {
		f.t.Fatalf("create user %q: %v", login, err)
	}
	return u
}

func (f *fixture) member(team store.Team, user store.User, role string) {
	f.t.Helper()

	tm := store.TeamMember{TeamID: team.ID, UserID: user.ID, Role: role}
	if err := f.s.DB.WithContext(f.ctx).Create(&tm).Error; err != nil {
		f.t.Fatalf("add user %q to team %q: %v", user.Login, team.Name, err)
	}
}

// openPR inserts an open pull request directly through GORM so tests do not
// depend on the sync path to build their fixtures.
func (f *fixture) openPR(repoID int64, number int, qaStatus string, draft bool) store.PullRequest {
	f.t.Helper()

	pr := store.PullRequest{
		RepositoryID:    repoID,
		Number:          number,
		Title:           fmt.Sprintf("PR %d", number),
		AuthorLogin:     "octocat",
		HeadSHA:         fmt.Sprintf("sha-%d", number),
		BaseRef:         "main",
		HeadRef:         fmt.Sprintf("branch-%d", number),
		HTMLURL:         fmt.Sprintf("https://github.com/acme/app/pull/%d", number),
		Draft:           draft,
		State:           "open",
		QAStatus:        qaStatus,
		GitHubUpdatedAt: time.Now().UTC().Add(time.Duration(number) * time.Second),
	}
	if err := f.s.DB.WithContext(f.ctx).Create(&pr).Error; err != nil {
		f.t.Fatalf("create pull request #%d: %v", number, err)
	}
	return pr
}

func (f *fixture) pullRequest(t *testing.T, id uint) store.PullRequest {
	t.Helper()

	var pr store.PullRequest
	if err := f.s.DB.WithContext(f.ctx).Take(&pr, id).Error; err != nil {
		t.Fatalf("load pull request %d: %v", id, err)
	}
	return pr
}

func (f *fixture) repository(t *testing.T, id int64) store.Repository {
	t.Helper()

	var r store.Repository
	if err := f.s.DB.WithContext(f.ctx).Take(&r, id).Error; err != nil {
		t.Fatalf("load repository %d: %v", id, err)
	}
	return r
}

// reposOf returns the sorted repository names of the given PR views.
func reposOf(views []store.PRView) []string {
	repos := make([]string, len(views))
	for i, v := range views {
		repos[i] = v.Repo
	}
	sort.Strings(repos)
	return repos
}

func TestUpsertRepositoryKeepsSettings(t *testing.T) {
	f := newFixture(t)
	inst := f.installation(1001)
	team := f.team("team-a")

	first := store.Repository{
		ID:             5001,
		InstallationID: inst.ID,
		FullName:       "acme/old-name",
		Private:        false,
		DefaultBranch:  "main",
		Enabled:        true,
		TeamID:         &team.ID,
	}
	if err := f.s.UpsertRepository(f.ctx, first); err != nil {
		t.Fatalf("first UpsertRepository() returned error: %v", err)
	}

	// A later GitHub sync must refresh the metadata but never the local
	// settings: enabled and team assignment are owned by the app.
	second := store.Repository{
		ID:             first.ID,
		InstallationID: inst.ID,
		FullName:       "acme/new-name",
		Private:        true,
		DefaultBranch:  "trunk",
		Enabled:        false,
	}
	if err := f.s.UpsertRepository(f.ctx, second); err != nil {
		t.Fatalf("second UpsertRepository() returned error: %v", err)
	}

	got := f.repository(t, first.ID)
	if got.FullName != "acme/new-name" {
		t.Errorf("FullName = %q, want %q", got.FullName, "acme/new-name")
	}
	if !got.Private {
		t.Error("Private = false, want true")
	}
	if got.DefaultBranch != "trunk" {
		t.Errorf("DefaultBranch = %q, want %q", got.DefaultBranch, "trunk")
	}
	if !got.Enabled {
		t.Error("Enabled = false, want true (upsert must not reset it)")
	}
	if got.TeamID == nil || *got.TeamID != team.ID {
		t.Errorf("TeamID = %v, want %d (upsert must not reset it)", got.TeamID, team.ID)
	}
}

func TestSyncPullRequestIgnoresStaleEvent(t *testing.T) {
	f := newFixture(t)
	inst := f.installation(1002)
	repo := f.repo(5101, inst.ID, "acme/app", nil, true)

	t2 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	t1 := t2.Add(-time.Hour)

	created, changed, err := f.s.SyncPullRequest(f.ctx, store.PullRequest{
		RepositoryID:    repo.ID,
		Number:          7,
		Title:           "fresh",
		HeadSHA:         "b",
		State:           "open",
		GitHubUpdatedAt: t2,
	})
	if err != nil {
		t.Fatalf("first SyncPullRequest() returned error: %v", err)
	}
	if !changed {
		t.Fatal("first SyncPullRequest() changed = false, want true")
	}

	stale, changed, err := f.s.SyncPullRequest(f.ctx, store.PullRequest{
		RepositoryID:    repo.ID,
		Number:          7,
		Title:           "stale",
		HeadSHA:         "a",
		State:           "open",
		GitHubUpdatedAt: t1,
	})
	if err != nil {
		t.Fatalf("stale SyncPullRequest() returned error: %v", err)
	}
	if changed {
		t.Error("stale SyncPullRequest() changed = true, want false")
	}
	if stale.HeadSHA != "b" {
		t.Errorf("returned HeadSHA = %q, want %q", stale.HeadSHA, "b")
	}
	if got := f.pullRequest(t, created.ID); got.HeadSHA != "b" || got.Title != "fresh" {
		t.Errorf("stored PR = {HeadSHA: %q, Title: %q}, want %q/%q", got.HeadSHA, got.Title, "b", "fresh")
	}
}

func TestSyncPullRequestDuplicateIsNoop(t *testing.T) {
	f := newFixture(t)
	inst := f.installation(1003)
	repo := f.repo(5201, inst.ID, "acme/app", nil, true)

	event := store.PullRequest{
		RepositoryID:    repo.ID,
		Number:          9,
		Title:           "same",
		AuthorLogin:     "octocat",
		HeadSHA:         "sha-9",
		BaseRef:         "main",
		HeadRef:         "feature",
		HTMLURL:         "https://github.com/acme/app/pull/9",
		State:           "open",
		GitHubUpdatedAt: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC),
	}

	created, changed, err := f.s.SyncPullRequest(f.ctx, event)
	if err != nil {
		t.Fatalf("first SyncPullRequest() returned error: %v", err)
	}
	if !changed {
		t.Fatal("first SyncPullRequest() changed = false, want true")
	}

	again, changed, err := f.s.SyncPullRequest(f.ctx, event)
	if err != nil {
		t.Fatalf("duplicate SyncPullRequest() returned error: %v", err)
	}
	if changed {
		t.Error("duplicate SyncPullRequest() changed = true, want false")
	}
	if again.ID != created.ID {
		t.Errorf("duplicate SyncPullRequest() ID = %d, want %d", again.ID, created.ID)
	}

	var count int64
	if err := f.s.DB.WithContext(f.ctx).Model(&store.PullRequest{}).Count(&count).Error; err != nil {
		t.Fatalf("count pull requests: %v", err)
	}
	if count != 1 {
		t.Errorf("pull request rows = %d, want 1", count)
	}
}

func TestSyncPullRequestEqualTimestampChangedPayload(t *testing.T) {
	f := newFixture(t)
	inst := f.installation(1013)
	repo := f.repo(5302, inst.ID, "acme/app", nil, true)

	stamp := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	first, changed, err := f.s.SyncPullRequest(f.ctx, store.PullRequest{
		RepositoryID:    repo.ID,
		Number:          11,
		Title:           "old",
		HeadSHA:         "same-sha",
		State:           "open",
		GitHubUpdatedAt: stamp,
	})
	if err != nil {
		t.Fatalf("first SyncPullRequest() returned error: %v", err)
	}
	if !changed {
		t.Fatal("first SyncPullRequest() changed = false, want true")
	}

	updated, changed, err := f.s.SyncPullRequest(f.ctx, store.PullRequest{
		RepositoryID:    repo.ID,
		Number:          11,
		Title:           "new title",
		HeadSHA:         "same-sha",
		State:           "open",
		GitHubUpdatedAt: stamp,
	})
	if err != nil {
		t.Fatalf("updated SyncPullRequest() returned error: %v", err)
	}
	if !changed {
		t.Error("equal timestamp with changed payload: changed = false, want true")
	}
	if got := f.pullRequest(t, first.ID); got.Title != "new title" || updated.Title != "new title" {
		t.Errorf("Title = %q (returned %q), want %q", got.Title, updated.Title, "new title")
	}
}

func TestSyncPullRequestPersistsDescription(t *testing.T) {
	f := newFixture(t)
	inst := f.installation(1015)
	repo := f.repo(5303, inst.ID, "acme/app", nil, true)

	stamp := time.Date(2026, 11, 11, 13, 0, 0, 0, time.UTC)
	first, changed, err := f.s.SyncPullRequest(f.ctx, store.PullRequest{
		RepositoryID:    repo.ID,
		Number:          12,
		Title:           "Descripción",
		Body:            "primera versión",
		HeadSHA:         "same-sha",
		State:           "open",
		GitHubUpdatedAt: stamp,
	})
	if err != nil {
		t.Fatalf("first SyncPullRequest() returned error: %v", err)
	}
	if !changed {
		t.Fatal("first SyncPullRequest() changed = false, want true")
	}
	if got := f.pullRequest(t, first.ID); got.Body != "primera versión" {
		t.Errorf("Body = %q, want %q", got.Body, "primera versión")
	}

	updated, changed, err := f.s.SyncPullRequest(f.ctx, store.PullRequest{
		RepositoryID:    repo.ID,
		Number:          12,
		Title:           "Descripción",
		Body:            "segunda versión",
		HeadSHA:         "same-sha",
		State:           "open",
		GitHubUpdatedAt: stamp,
	})
	if err != nil {
		t.Fatalf("updated SyncPullRequest() returned error: %v", err)
	}
	if !changed {
		t.Error("edited body: changed = false, want true")
	}
	if got := f.pullRequest(t, first.ID); got.Body != "segunda versión" || updated.Body != "segunda versión" {
		t.Errorf("Body = %q (returned %q), want %q", got.Body, updated.Body, "segunda versión")
	}
}

func TestSyncPullRequestPersistsForkFields(t *testing.T) {
	f := newFixture(t)
	inst := f.installation(1014)
	repo := f.repo(6101, inst.ID, "acme/app", nil, true)

	stamp := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	base := store.PullRequest{
		RepositoryID:    repo.ID,
		Number:          13,
		Title:           "same title",
		HeadSHA:         "same-sha",
		State:           "open",
		GitHubUpdatedAt: stamp,
	}

	first := base
	first.IsFork = true
	first.HeadRepoFullName = "ext/repo"
	created, changed, err := f.s.SyncPullRequest(f.ctx, first)
	if err != nil {
		t.Fatalf("first SyncPullRequest() returned error: %v", err)
	}
	if !changed {
		t.Fatal("first SyncPullRequest() changed = false, want true")
	}
	if !created.IsFork || created.HeadRepoFullName != "ext/repo" {
		t.Fatalf("created PR = %+v, want IsFork=true, HeadRepoFullName=ext/repo", created)
	}

	// Only the fork fields change. The timestamp and every other payload field
	// stay put, so changed=true here also proves the equality check sees them.
	second := base
	_, changed, err = f.s.SyncPullRequest(f.ctx, second)
	if err != nil {
		t.Fatalf("second SyncPullRequest() returned error: %v", err)
	}
	if !changed {
		t.Fatal("fork-only change: changed = false, want true")
	}
	stored := f.pullRequest(t, created.ID)
	if stored.IsFork || stored.HeadRepoFullName != "" {
		t.Fatalf("stored PR = %+v, want IsFork=false, HeadRepoFullName empty", stored)
	}
}

func TestDeleteInstallationRemovesPRs(t *testing.T) {
	f := newFixture(t)
	inst := f.installation(1004)
	repo := f.repo(5301, inst.ID, "acme/app", nil, true)
	f.openPR(repo.ID, 1, store.StatusPending, false)

	admin := f.user("admin", true)

	installations, err := f.s.ListInstallations(f.ctx)
	if err != nil {
		t.Fatalf("ListInstallations() returned error: %v", err)
	}
	if len(installations) != 1 || installations[0].ID != inst.ID {
		t.Fatalf("ListInstallations() = %+v, want only installation %d", installations, inst.ID)
	}

	views, err := f.s.ListVisiblePullRequests(f.ctx, admin, store.PRFilter{})
	if err != nil {
		t.Fatalf("ListVisiblePullRequests() returned error: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("visible PRs before delete = %d, want 1", len(views))
	}

	if err := f.s.DeleteInstallation(f.ctx, inst.ID); err != nil {
		t.Fatalf("DeleteInstallation() returned error: %v", err)
	}

	views, err = f.s.ListVisiblePullRequests(f.ctx, admin, store.PRFilter{})
	if err != nil {
		t.Fatalf("ListVisiblePullRequests() after delete returned error: %v", err)
	}
	if len(views) != 0 {
		t.Errorf("visible PRs after delete = %d, want 0", len(views))
	}

	var repos, prs int64
	if err := f.s.DB.WithContext(f.ctx).Model(&store.Repository{}).Count(&repos).Error; err != nil {
		t.Fatalf("count repositories: %v", err)
	}
	if err := f.s.DB.WithContext(f.ctx).Model(&store.PullRequest{}).Count(&prs).Error; err != nil {
		t.Fatalf("count pull requests: %v", err)
	}
	if repos != 0 || prs != 0 {
		t.Errorf("after delete repositories = %d, pull requests = %d, want 0/0", repos, prs)
	}

	if err := f.s.DeleteInstallation(f.ctx, inst.ID); err == nil {
		t.Error("second DeleteInstallation() returned nil error, want ErrNotFound")
	}
}

func TestEnabledRepositories(t *testing.T) {
	f := newFixture(t)
	inst := f.installation(1005)
	enabled := f.repo(5401, inst.ID, "acme/enabled", nil, true)
	f.repo(5402, inst.ID, "acme/disabled", nil, false)
	other := f.installation(1006)
	f.repo(5403, other.ID, "other/enabled", nil, true)
	f.openPR(enabled.ID, 1, store.StatusPending, false)

	repos, err := f.s.EnabledRepositories(f.ctx, inst.ID)
	if err != nil {
		t.Fatalf("EnabledRepositories() returned error: %v", err)
	}
	if len(repos) != 1 || repos[0].FullName != "acme/enabled" {
		t.Fatalf("EnabledRepositories() = %+v, want only acme/enabled", repos)
	}

	if err := f.s.RemoveRepository(f.ctx, 5401); err != nil {
		t.Fatalf("RemoveRepository() returned error: %v", err)
	}
	repos, err = f.s.EnabledRepositories(f.ctx, inst.ID)
	if err != nil {
		t.Fatalf("EnabledRepositories() after remove returned error: %v", err)
	}
	if len(repos) != 0 {
		t.Errorf("EnabledRepositories() after remove = %+v, want empty", repos)
	}

	var prs int64
	if err := f.s.DB.WithContext(f.ctx).Model(&store.PullRequest{}).
		Where("repository_id = ?", enabled.ID).Count(&prs).Error; err != nil {
		t.Fatalf("count pull requests after remove: %v", err)
	}
	if prs != 0 {
		t.Errorf("pull requests after remove = %d, want 0 (cascade)", prs)
	}
}

func TestCloseMissingPullRequests(t *testing.T) {
	f := newFixture(t)
	inst := f.installation(1007)
	repo := f.repo(5501, inst.ID, "acme/app", nil, true)
	kept := f.openPR(repo.ID, 1, store.StatusPending, false)
	gone := f.openPR(repo.ID, 2, store.StatusReviewing, false)
	merged := f.openPR(repo.ID, 3, store.StatusPending, false)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.PullRequest{}).
		Where("id = ?", merged.ID).
		Updates(map[string]any{"state": store.StatusMerged, "qa_status": store.StatusMerged}).Error; err != nil {
		t.Fatalf("mark PR #3 as merged: %v", err)
	}

	if err := f.s.CloseMissingPullRequests(f.ctx, repo.ID, []int{1}, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CloseMissingPullRequests() returned error: %v", err)
	}

	if got := f.pullRequest(t, kept.ID); got.State != "open" || got.QAStatus != store.StatusPending {
		t.Errorf("PR #1 = {State: %q, QAStatus: %q}, want open/pending", got.State, got.QAStatus)
	}
	if got := f.pullRequest(t, gone.ID); got.State != store.StatusClosed || got.QAStatus != store.StatusClosed {
		t.Errorf("PR #2 = {State: %q, QAStatus: %q}, want closed/closed", got.State, got.QAStatus)
	}
	if got := f.pullRequest(t, merged.ID); got.State != store.StatusMerged || got.QAStatus != store.StatusMerged {
		t.Errorf("PR #3 = {State: %q, QAStatus: %q}, want merged/merged", got.State, got.QAStatus)
	}
}

func TestCloseMissingPullRequestsEmptyList(t *testing.T) {
	f := newFixture(t)
	inst := f.installation(1008)
	repo := f.repo(5601, inst.ID, "acme/app", nil, true)
	first := f.openPR(repo.ID, 1, store.StatusPending, false)
	second := f.openPR(repo.ID, 2, store.StatusPending, false)

	if err := f.s.CloseMissingPullRequests(f.ctx, repo.ID, nil, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CloseMissingPullRequests(nil) returned error: %v", err)
	}

	for _, pr := range []store.PullRequest{first, second} {
		got := f.pullRequest(t, pr.ID)
		if got.State != store.StatusClosed || got.QAStatus != store.StatusClosed {
			t.Errorf("PR #%d = {State: %q, QAStatus: %q}, want closed/closed", pr.Number, got.State, got.QAStatus)
		}
	}
}

func TestVisibility(t *testing.T) {
	f := newFixture(t)
	inst := f.installation(1009)

	teamOne := f.team("team-one")
	teamTwo := f.team("team-two")

	repoA := f.repo(5701, inst.ID, "acme/a", &teamOne.ID, true)
	repoB := f.repo(5702, inst.ID, "acme/b", &teamTwo.ID, true)
	repoC := f.repo(5703, inst.ID, "acme/c", &teamOne.ID, false)

	f.openPR(repoA.ID, 1, store.StatusPending, false)
	f.openPR(repoB.ID, 1, store.StatusPending, false)
	f.openPR(repoC.ID, 1, store.StatusPending, false)

	developer := f.user("dev-one", false)
	f.member(teamOne, developer, "developer")
	outsider := f.user("outsider", false)
	admin := f.user("admin", true)

	views, err := f.s.ListVisiblePullRequests(f.ctx, developer, store.PRFilter{})
	if err != nil {
		t.Fatalf("developer ListVisiblePullRequests() returned error: %v", err)
	}
	if got, want := reposOf(views), []string{"acme/a"}; !equalStrings(got, want) {
		t.Errorf("developer sees %v, want %v", got, want)
	}

	views, err = f.s.ListVisiblePullRequests(f.ctx, admin, store.PRFilter{})
	if err != nil {
		t.Fatalf("admin ListVisiblePullRequests() returned error: %v", err)
	}
	if got, want := reposOf(views), []string{"acme/a", "acme/b"}; !equalStrings(got, want) {
		t.Errorf("admin sees %v, want %v (disabled repo must never be visible)", got, want)
	}

	views, err = f.s.ListVisiblePullRequests(f.ctx, outsider, store.PRFilter{})
	if err != nil {
		t.Fatalf("outsider ListVisiblePullRequests() returned error: %v", err)
	}
	if len(views) != 0 {
		t.Errorf("outsider sees %v, want nothing", reposOf(views))
	}

	stats, err := f.s.Stats(f.ctx, developer)
	if err != nil {
		t.Fatalf("developer Stats() returned error: %v", err)
	}
	if stats.Open != 1 {
		t.Errorf("developer Stats().Open = %d, want 1", stats.Open)
	}
}

func TestFilterAndStats(t *testing.T) {
	f := newFixture(t)
	inst := f.installation(1010)
	repo := f.repo(5801, inst.ID, "acme/app", nil, true)

	pending := f.openPR(repo.ID, 1, store.StatusPending, false)
	changes := f.openPR(repo.ID, 2, store.StatusChangesRequested, false)
	ready := f.openPR(repo.ID, 3, store.StatusReadyForHuman, false)
	draft := f.openPR(repo.ID, 4, store.StatusDraft, true)

	admin := f.user("admin", true)

	stats, err := f.s.Stats(f.ctx, admin)
	if err != nil {
		t.Fatalf("Stats() returned error: %v", err)
	}
	want := store.Stats{Open: 4, AwaitingHuman: 1, InEvaluation: 1, WithProblems: 1}
	if stats != want {
		t.Errorf("Stats() = %+v, want %+v", stats, want)
	}

	cases := []struct {
		group string
		want  uint
	}{
		{"in_review", pending.ID},
		{"blocked", changes.ID},
		{"ready", ready.ID},
	}
	for _, tc := range cases {
		views, err := f.s.ListVisiblePullRequests(f.ctx, admin, store.PRFilter{Group: tc.group})
		if err != nil {
			t.Fatalf("ListVisiblePullRequests(group=%q) returned error: %v", tc.group, err)
		}
		if len(views) != 1 {
			t.Errorf("group %q returned %d rows, want 1", tc.group, len(views))
			continue
		}
		if views[0].ID != tc.want {
			t.Errorf("group %q returned PR %d, want %d", tc.group, views[0].ID, tc.want)
		}
		if views[0].Group != tc.group {
			t.Errorf("group %q view has Group %q", tc.group, views[0].Group)
		}
	}

	views, err := f.s.ListVisiblePullRequests(f.ctx, admin, store.PRFilter{})
	if err != nil {
		t.Fatalf("unfiltered ListVisiblePullRequests() returned error: %v", err)
	}
	if len(views) != 4 {
		t.Fatalf("unfiltered views = %d, want 4", len(views))
	}
	found := false
	for _, v := range views {
		if v.Validations.Passed != 0 || v.Validations.Total != 0 {
			t.Errorf("PR %d Validations = %+v, want 0/0", v.ID, v.Validations)
		}
		if v.ID == draft.ID {
			found = true
			if v.Group != "" {
				t.Errorf("draft PR group = %q, want empty", v.Group)
			}
		}
	}
	if !found {
		t.Error("draft pull request is not in the visible list")
	}
}

func TestFilterGroupsMatchGroupOf(t *testing.T) {
	f := newFixture(t)
	inst := f.installation(1011)
	repo := f.repo(5901, inst.ID, "acme/app", nil, true)
	admin := f.user("admin", true)

	statuses := []string{
		store.StatusDraft,
		store.StatusAwaitingApproval,
		store.StatusPending,
		store.StatusReviewing,
		store.StatusChangesRequested,
		store.StatusWaitingCI,
		store.StatusReadyForHuman,
		store.StatusInconclusive,
		store.StatusPaused,
		store.StatusMerged,
		store.StatusClosed,
	}

	// Go through the real SQL filter path: duplicating the predicates in a
	// helper would not catch a mismatch between GroupOf and the query.
	byID := make(map[uint]string, len(statuses))
	for i, status := range statuses {
		pr := f.openPR(repo.ID, i+1, status, false)
		byID[pr.ID] = status
	}

	for _, group := range []string{"in_review", "blocked", "ready"} {
		views, err := f.s.ListVisiblePullRequests(f.ctx, admin, store.PRFilter{Group: group})
		if err != nil {
			t.Fatalf("ListVisiblePullRequests(group=%q) returned error: %v", group, err)
		}
		got := make(map[uint]bool, len(views))
		for _, v := range views {
			got[v.ID] = true
			if v.Group != group {
				t.Errorf("group %q returned PR %d with Group %q", group, v.ID, v.Group)
			}
		}
		for id, status := range byID {
			if want := group == store.GroupOf(status); got[id] != want {
				t.Errorf("group %q: PR %d (%s) present = %v, want %v", group, id, status, got[id], want)
			}
		}
	}
}

func TestListVisiblePullRequestsOrderedByUpdatedAt(t *testing.T) {
	f := newFixture(t)
	inst := f.installation(1012)
	repo := f.repo(6001, inst.ID, "acme/app", nil, true)
	admin := f.user("admin", true)

	base := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	oldest := f.openPR(repo.ID, 1, store.StatusPending, false)
	middle := f.openPR(repo.ID, 2, store.StatusPending, false)
	newest := f.openPR(repo.ID, 3, store.StatusPending, false)
	for i, pr := range []store.PullRequest{oldest, middle, newest} {
		stamp := base.Add(time.Duration(i) * time.Hour)
		if err := f.s.DB.WithContext(f.ctx).Model(&store.PullRequest{}).
			Where("id = ?", pr.ID).
			Updates(map[string]any{"updated_at": stamp}).Error; err != nil {
			t.Fatalf("set updated_at for PR %d: %v", pr.ID, err)
		}
	}

	views, err := f.s.ListVisiblePullRequests(f.ctx, admin, store.PRFilter{})
	if err != nil {
		t.Fatalf("ListVisiblePullRequests() returned error: %v", err)
	}
	wantIDs := []uint{newest.ID, middle.ID, oldest.ID}
	if len(views) != len(wantIDs) {
		t.Fatalf("visible PRs = %d, want %d", len(views), len(wantIDs))
	}
	for i, want := range wantIDs {
		if views[i].ID != want {
			t.Errorf("views[%d].ID = %d, want %d (updated_at DESC)", i, views[i].ID, want)
		}
	}
}

func TestPRValidationsArePersisted(t *testing.T) {
	f := newFixture(t)
	inst := f.installation(1014)
	repo := f.repo(6101, inst.ID, "acme/app", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)

	want := store.PRValidations{Passed: 9, Total: 9}
	changed, err := f.s.SetPRValidations(f.ctx, pr.ID, pr.HeadSHA, want)
	if err != nil {
		t.Fatalf("SetPRValidations: %v", err)
	}
	if !changed {
		t.Fatal("SetPRValidations changed = false, want true")
	}

	stored, err := f.s.PRValidationsFor(f.ctx, pr.ID, pr.HeadSHA)
	if err != nil {
		t.Fatalf("PRValidationsFor: %v", err)
	}
	if stored != want {
		t.Fatalf("stored validations = %+v, want %+v", stored, want)
	}

	// Replaying the same verdict is a no-op so deliveries do not republish.
	changed, err = f.s.SetPRValidations(f.ctx, pr.ID, pr.HeadSHA, want)
	if err != nil {
		t.Fatalf("SetPRValidations(replay): %v", err)
	}
	if changed {
		t.Error("SetPRValidations(replay) changed = true, want false")
	}

	admin := f.user("admin", true)
	views, err := f.s.ListVisiblePullRequests(f.ctx, admin, store.PRFilter{})
	if err != nil {
		t.Fatalf("ListVisiblePullRequests: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("views = %d, want 1", len(views))
	}
	if views[0].Validations != want {
		t.Errorf("view validations = %+v, want %+v", views[0].Validations, want)
	}

	// A refresh of a head the pull request has already left must not overwrite
	// the counters of the current head.
	changed, err = f.s.SetPRValidations(f.ctx, pr.ID, "another-sha", store.PRValidations{Passed: 1, Total: 5})
	if err != nil {
		t.Fatalf("SetPRValidations(stale head): %v", err)
	}
	if changed {
		t.Error("SetPRValidations(stale head) changed = true, want false")
	}
	if stored, err := f.s.PRValidationsFor(f.ctx, pr.ID, pr.HeadSHA); err != nil || stored != want {
		t.Errorf("validations after stale write = %+v, err = %v; want %+v", stored, err, want)
	}
}

func TestPRValidationsResetOnNewHead(t *testing.T) {
	f := newFixture(t)
	inst := f.installation(1015)
	repo := f.repo(6102, inst.ID, "acme/app", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)

	if _, err := f.s.SetPRValidations(f.ctx, pr.ID, pr.HeadSHA, store.PRValidations{Passed: 9, Total: 9}); err != nil {
		t.Fatalf("SetPRValidations: %v", err)
	}
	updated, _, err := f.s.SyncPullRequest(f.ctx, store.PullRequest{
		RepositoryID:    repo.ID,
		Number:          pr.Number,
		Title:           pr.Title,
		HeadSHA:         "new-head-sha",
		State:           "open",
		GitHubUpdatedAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("SyncPullRequest(new head): %v", err)
	}
	if updated.HeadSHA != "new-head-sha" {
		t.Fatalf("HeadSHA = %q, want new-head-sha", updated.HeadSHA)
	}

	stored, err := f.s.PRValidationsFor(f.ctx, pr.ID, "new-head-sha")
	if err != nil {
		t.Fatalf("PRValidationsFor: %v", err)
	}
	if stored != (store.PRValidations{}) {
		t.Errorf("validations of the new head = %+v, want 0/0 for a different SHA", stored)
	}

	admin := f.user("admin", true)
	views, err := f.s.ListVisiblePullRequests(f.ctx, admin, store.PRFilter{})
	if err != nil {
		t.Fatalf("ListVisiblePullRequests: %v", err)
	}
	if len(views) != 1 || views[0].Validations != (store.PRValidations{}) {
		t.Errorf("view validations = %+v, want 0/0 after a new head", views)
	}
}

// markFork flips the fork flag through the database so tests can cover the
// eligibility rule without going through a GitHub payload.
func (f *fixture) markFork(pr store.PullRequest) store.PullRequest {
	f.t.Helper()

	if err := f.s.DB.WithContext(f.ctx).Model(&store.PullRequest{}).
		Where("id = ?", pr.ID).Update("is_fork", true).Error; err != nil {
		f.t.Fatalf("mark pull request #%d as fork: %v", pr.Number, err)
	}
	return pr
}

func TestReviewStateScenarios(t *testing.T) {
	f := newFixture(t)
	inst := f.installation(1016)

	enabledProfile := f.llmProfile("enabled", true)
	disabledProfile := f.llmProfile("disabled", false)

	configuredTeam := f.team("configured")
	if _, err := f.s.SaveQAPolicy(f.ctx, store.QAPolicy{
		TeamID: configuredTeam.ID, LLMProfileID: enabledProfile.ID,
	}); err != nil {
		t.Fatalf("SaveQAPolicy(configured): %v", err)
	}
	brokenTeam := f.team("broken")
	if _, err := f.s.SaveQAPolicy(f.ctx, store.QAPolicy{
		TeamID: brokenTeam.ID, LLMProfileID: disabledProfile.ID,
	}); err != nil {
		t.Fatalf("SaveQAPolicy(broken): %v", err)
	}
	noPolicyTeam := f.team("no-policy")

	forkRepo := f.repo(6201, inst.ID, "acme/fork", &configuredTeam.ID, true)
	draftRepo := f.repo(6202, inst.ID, "acme/draft", &configuredTeam.ID, true)
	noTeamRepo := f.repo(6203, inst.ID, "acme/no-team", nil, true)
	noPolicyRepo := f.repo(6204, inst.ID, "acme/no-policy", &noPolicyTeam.ID, true)
	disabledRepo := f.repo(6205, inst.ID, "acme/disabled-profile", &brokenTeam.ID, true)
	readyRepo := f.repo(6206, inst.ID, "acme/ready", &configuredTeam.ID, true)
	runningRepo := f.repo(6207, inst.ID, "acme/running", nil, true)

	prFork := f.markFork(f.openPR(forkRepo.ID, 1, store.StatusPending, false))
	prDraft := f.openPR(draftRepo.ID, 1, store.StatusPending, true)
	prNoTeam := f.openPR(noTeamRepo.ID, 1, store.StatusPending, false)
	prNoPolicy := f.openPR(noPolicyRepo.ID, 1, store.StatusPending, false)
	prDisabled := f.openPR(disabledRepo.ID, 1, store.StatusPending, false)
	prScheduled := f.openPR(readyRepo.ID, 1, store.StatusPending, false)
	prRunning := f.openPR(runningRepo.ID, 1, store.StatusPending, false)

	// The active run must win even though this repository has no configuration
	// at all, so a queued review is never hidden as blocked.
	if _, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: prRunning.ID, ProfileID: enabledProfile.ID,
		HeadSHA: prRunning.HeadSHA, Status: store.RunQueued,
	}); err != nil {
		t.Fatalf("CreateReviewRun: %v", err)
	}

	admin := f.user("admin", true)
	views, err := f.s.ListVisiblePullRequests(f.ctx, admin, store.PRFilter{})
	if err != nil {
		t.Fatalf("ListVisiblePullRequests() returned error: %v", err)
	}
	byID := make(map[uint]store.PRView, len(views))
	for _, v := range views {
		byID[v.ID] = v
	}

	cases := []struct {
		name   string
		id     uint
		state  string
		reason string
	}{
		{"fork", prFork.ID, "blocked", "PR externo: el equipo no permite aportes externos"},
		{"draft", prDraft.ID, "blocked", "Borrador: se revisará cuando pase a listo"},
		{"no team", prNoTeam.ID, "blocked", "Repositorio sin equipo asignado"},
		{"no policy", prNoPolicy.ID, "blocked", "El equipo no tiene política QA"},
		{"disabled profile", prDisabled.ID, "blocked", "La política apunta a un perfil LLM deshabilitado"},
		{"scheduled", prScheduled.ID, "scheduled", ""},
		{"active run", prRunning.ID, "running", ""},
	}
	for _, tc := range cases {
		view, ok := byID[tc.id]
		if !ok {
			t.Errorf("%s: PR %d is missing from the visible list", tc.name, tc.id)
			continue
		}
		if view.ReviewState != tc.state || view.ReviewReason != tc.reason {
			t.Errorf("%s: ReviewState/ReviewReason = %q/%q, want %q/%q",
				tc.name, view.ReviewState, view.ReviewReason, tc.state, tc.reason)
		}
	}
}

func TestReviewStateFor(t *testing.T) {
	full := store.ReviewSignals{TeamAssigned: true, HasPolicy: true, ProfileEnabled: true}
	with := func(base store.ReviewSignals, change func(*store.ReviewSignals)) store.ReviewSignals {
		change(&base)
		return base
	}

	cases := []struct {
		name   string
		in     store.ReviewSignals
		state  string
		reason string
	}{
		{"active run ignores config", store.ReviewSignals{ActiveRun: true}, "running", ""},
		{"draft", with(full, func(s *store.ReviewSignals) { s.Draft = true }), "blocked", "Borrador: se revisará cuando pase a listo"},
		{"fork", with(full, func(s *store.ReviewSignals) { s.Fork = true }), "blocked", "PR externo: el equipo no permite aportes externos"},
		{"fork allowed", with(full, func(s *store.ReviewSignals) {
			s.Origin = store.OriginCrossRepo
			s.ExternalAllowed = true
		}), "scheduled", ""},
		{"unknown origin", with(full, func(s *store.ReviewSignals) {
			s.Origin = store.OriginUnknown
			s.ExternalAllowed = true
		}), "blocked", "Origen del PR indeterminado: requiere revisión humana"},
		{"draft beats fork", store.ReviewSignals{Draft: true, Fork: true}, "blocked", "Borrador: se revisará cuando pase a listo"},
		{"no team", store.ReviewSignals{HasPolicy: true, ProfileEnabled: true}, "blocked", "Repositorio sin equipo asignado"},
		{"no policy", store.ReviewSignals{TeamAssigned: true, ProfileEnabled: true}, "blocked", "El equipo no tiene política QA"},
		{"no profile", store.ReviewSignals{TeamAssigned: true, HasPolicy: true}, "blocked", "La política apunta a un perfil LLM deshabilitado"},
		{"disabled profile", with(full, func(s *store.ReviewSignals) { s.ProfileEnabled = false }), "blocked", "La política apunta a un perfil LLM deshabilitado"},
		{"eligible", full, "scheduled", ""},
	}
	for _, tc := range cases {
		state, reason := store.ReviewStateFor(tc.in)
		if state != tc.state || reason != tc.reason {
			t.Errorf("%s: ReviewStateFor() = %q/%q, want %q/%q",
				tc.name, state, reason, tc.state, tc.reason)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
