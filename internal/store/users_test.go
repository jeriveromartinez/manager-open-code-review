package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

// TestFirstUserIsAdmin guards the bootstrap rule: the first GitHub login owns
// the instance, later logins do not, and re-login must not revoke admin rights.
func TestFirstUserIsAdmin(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()

	first, err := s.UpsertUserFromGitHub(ctx, 1001, "octocat", "https://avatars/octocat.png")
	if err != nil {
		t.Fatalf("first UpsertUserFromGitHub: %v", err)
	}
	if !first.IsAdmin {
		t.Error("first user is not admin")
	}
	if first.ID == 0 || first.GitHubID != 1001 || first.Login != "octocat" ||
		first.AvatarURL != "https://avatars/octocat.png" {
		t.Errorf("first user = %+v, want github_id=1001 login=octocat with avatar", first)
	}

	second, err := s.UpsertUserFromGitHub(ctx, 1002, "hubot", "https://avatars/hubot.png")
	if err != nil {
		t.Fatalf("second UpsertUserFromGitHub: %v", err)
	}
	if second.IsAdmin {
		t.Error("second user is admin, want non-admin")
	}

	again, err := s.UpsertUserFromGitHub(ctx, 1001, "octocat-renamed", "https://avatars/new.png")
	if err != nil {
		t.Fatalf("re-login UpsertUserFromGitHub: %v", err)
	}
	if again.ID != first.ID || !again.IsAdmin {
		t.Errorf("re-login = %+v, want id=%d and IsAdmin=true", again, first.ID)
	}
	if again.Login != "octocat-renamed" || again.AvatarURL != "https://avatars/new.png" {
		t.Errorf("re-login metadata = %+v, want updated login and avatar", again)
	}

	users, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 2 {
		t.Errorf("ListUsers returned %d users, want 2", len(users))
	}
}

// TestSessionLifecycle guards token hashing, expiry, deletion and the
// "never persist the clear token" contract.
func TestSessionLifecycle(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()

	u, err := s.UpsertUserFromGitHub(ctx, 2001, "alice", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	token, err := s.CreateSession(ctx, u.ID, time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		t.Fatalf("token %q is not 32-byte base64url: decode err=%v len=%d", token, err, len(raw))
	}

	resolved, err := s.UserBySessionToken(ctx, token)
	if err != nil {
		t.Fatalf("UserBySessionToken: %v", err)
	}
	if resolved.ID != u.ID || resolved.Login != "alice" {
		t.Errorf("resolved user = %+v, want %+v", resolved, u)
	}

	var hashes []string
	if err := s.DB.WithContext(ctx).Model(&store.Session{}).Order("id").Pluck("token_hash", &hashes).Error; err != nil {
		t.Fatalf("read token hashes: %v", err)
	}
	sum := sha256.Sum256([]byte(token))
	wantHash := hex.EncodeToString(sum[:])
	foundHash := false
	for _, h := range hashes {
		if h == token {
			t.Error("clear session token was persisted in sessions.token_hash")
		}
		if h == wantHash {
			foundHash = true
		}
	}
	if !foundHash {
		t.Errorf("sessions.token_hash = %v, want sha256 hex %q", hashes, wantHash)
	}

	expired, err := s.CreateSession(ctx, u.ID, -time.Minute)
	if err != nil {
		t.Fatalf("CreateSession with expired ttl: %v", err)
	}
	if _, err := s.UserBySessionToken(ctx, expired); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expired token lookup error = %v, want ErrNotFound", err)
	}

	if err := s.DeleteSession(ctx, token); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.UserBySessionToken(ctx, token); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleted token lookup error = %v, want ErrNotFound", err)
	}
	if _, err := s.UserBySessionToken(ctx, "no-such-token"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown token lookup error = %v, want ErrNotFound", err)
	}
}

// TestTeamRoles guards role validation, upserting membership, and the repo
// role lookup through the owning team.
func TestTeamRoles(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()

	lead, err := s.UpsertUserFromGitHub(ctx, 3001, "lead", "")
	if err != nil {
		t.Fatalf("create lead: %v", err)
	}
	qa, err := s.UpsertUserFromGitHub(ctx, 3002, "qa", "")
	if err != nil {
		t.Fatalf("create qa: %v", err)
	}
	outsider, err := s.UpsertUserFromGitHub(ctx, 3003, "outsider", "")
	if err != nil {
		t.Fatalf("create outsider: %v", err)
	}

	team, err := s.CreateTeam(ctx, "Equipo QA & Calidad")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if team.Slug != "equipo-qa-calidad" {
		t.Errorf("team slug = %q, want %q", team.Slug, "equipo-qa-calidad")
	}

	if err := s.SetTeamMember(ctx, team.ID, qa.ID, "owner"); err == nil {
		t.Error("SetTeamMember with role owner succeeded, want error")
	}
	if role, err := s.TeamRole(ctx, qa.ID, team.ID); err != nil || role != "" {
		t.Errorf("TeamRole after invalid role = %q, %v; want empty, nil", role, err)
	}

	if err := s.SetTeamMember(ctx, team.ID, lead.ID, "lead"); err != nil {
		t.Fatalf("SetTeamMember lead: %v", err)
	}
	if err := s.SetTeamMember(ctx, team.ID, qa.ID, "qa"); err != nil {
		t.Fatalf("SetTeamMember qa: %v", err)
	}
	if role, err := s.TeamRole(ctx, lead.ID, team.ID); err != nil || role != "lead" {
		t.Errorf("TeamRole(lead) = %q, %v; want lead, nil", role, err)
	}
	if role, err := s.TeamRole(ctx, outsider.ID, team.ID); err != nil || role != "" {
		t.Errorf("TeamRole(outsider) = %q, %v; want empty, nil", role, err)
	}

	// Upsert on the composite primary key changes the existing role.
	if err := s.SetTeamMember(ctx, team.ID, qa.ID, "developer"); err != nil {
		t.Fatalf("SetTeamMember developer: %v", err)
	}
	if role, err := s.TeamRole(ctx, qa.ID, team.ID); err != nil || role != "developer" {
		t.Errorf("TeamRole(qa) = %q, %v; want developer, nil", role, err)
	}

	installation := store.Installation{ID: 77, AccountLogin: "acme", AccountType: "Organization"}
	if err := s.UpsertInstallation(ctx, installation); err != nil {
		t.Fatalf("create installation: %v", err)
	}
	withTeam := store.Repository{ID: 501, InstallationID: 77, FullName: "acme/app", DefaultBranch: "main"}
	if err := s.UpsertRepository(ctx, withTeam); err != nil {
		t.Fatalf("create repository with team: %v", err)
	}
	noTeam := store.Repository{ID: 502, InstallationID: 77, FullName: "acme/no-team", DefaultBranch: "main"}
	if err := s.UpsertRepository(ctx, noTeam); err != nil {
		t.Fatalf("create repository without team: %v", err)
	}

	if err := s.AssignRepositoryTeam(ctx, withTeam.ID, &team.ID); err != nil {
		t.Fatalf("AssignRepositoryTeam: %v", err)
	}
	if role, err := s.RepoRole(ctx, lead.ID, withTeam.ID); err != nil || role != "lead" {
		t.Errorf("RepoRole(lead, owned repo) = %q, %v; want lead, nil", role, err)
	}
	if role, err := s.RepoRole(ctx, qa.ID, withTeam.ID); err != nil || role != "developer" {
		t.Errorf("RepoRole(qa, owned repo) = %q, %v; want developer, nil", role, err)
	}
	if role, err := s.RepoRole(ctx, outsider.ID, withTeam.ID); err != nil || role != "" {
		t.Errorf("RepoRole(outsider, owned repo) = %q, %v; want empty, nil", role, err)
	}
	if role, err := s.RepoRole(ctx, lead.ID, noTeam.ID); err != nil || role != "" {
		t.Errorf("RepoRole(lead, repo without team) = %q, %v; want empty, nil", role, err)
	}

	if err := s.AssignRepositoryTeam(ctx, withTeam.ID, nil); err != nil {
		t.Fatalf("clear AssignRepositoryTeam: %v", err)
	}
	if role, err := s.RepoRole(ctx, lead.ID, withTeam.ID); err != nil || role != "" {
		t.Errorf("RepoRole after clearing team = %q, %v; want empty, nil", role, err)
	}
}

// TestTeamListingAndMembership guards the deterministic listing of teams and
// their members and the removal of a member.
func TestTeamListingAndMembership(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()

	zeta, err := s.UpsertUserFromGitHub(ctx, 4001, "zeta", "")
	if err != nil {
		t.Fatalf("create zeta: %v", err)
	}
	alpha, err := s.UpsertUserFromGitHub(ctx, 4002, "alpha", "")
	if err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	teamB, err := s.CreateTeam(ctx, "Beta Team")
	if err != nil {
		t.Fatalf("create beta team: %v", err)
	}
	teamA, err := s.CreateTeam(ctx, "Álpha Equipo")
	if err != nil {
		t.Fatalf("create alpha team: %v", err)
	}
	if teamA.Slug != "alpha-equipo" {
		t.Errorf("slug = %q, want %q", teamA.Slug, "alpha-equipo")
	}

	if err := s.SetTeamMember(ctx, teamB.ID, zeta.ID, "qa"); err != nil {
		t.Fatalf("add zeta: %v", err)
	}
	if err := s.SetTeamMember(ctx, teamA.ID, zeta.ID, "developer"); err != nil {
		t.Fatalf("add zeta to alpha team: %v", err)
	}
	if err := s.SetTeamMember(ctx, teamA.ID, alpha.ID, "lead"); err != nil {
		t.Fatalf("add alpha: %v", err)
	}

	teams, err := s.ListTeams(ctx)
	if err != nil {
		t.Fatalf("ListTeams: %v", err)
	}
	if len(teams) != 2 {
		t.Fatalf("ListTeams returned %d teams, want 2", len(teams))
	}
	if teams[0].Team.ID != teamA.ID || teams[1].Team.ID != teamB.ID {
		t.Errorf("team order = %q, %q; want %q first", teams[0].Name, teams[1].Name, teamA.Name)
	}
	if len(teams[0].Members) != 2 {
		t.Fatalf("alpha team has %d members, want 2", len(teams[0].Members))
	}
	if teams[0].Members[0].User.Login != "alpha" || teams[0].Members[0].Role != "lead" {
		t.Errorf("first member = %+v, want alpha/lead", teams[0].Members[0])
	}
	if teams[0].Members[1].User.Login != "zeta" || teams[0].Members[1].Role != "developer" {
		t.Errorf("second member = %+v, want zeta/developer", teams[0].Members[1])
	}
	if len(teams[1].Members) != 1 || teams[1].Members[0].Role != "qa" {
		t.Errorf("beta team members = %+v, want one qa", teams[1].Members)
	}

	if err := s.RemoveTeamMember(ctx, teamA.ID, zeta.ID); err != nil {
		t.Fatalf("RemoveTeamMember: %v", err)
	}
	if role, err := s.TeamRole(ctx, zeta.ID, teamA.ID); err != nil || role != "" {
		t.Errorf("TeamRole after removal = %q, %v; want empty, nil", role, err)
	}
	teams, err = s.ListTeams(ctx)
	if err != nil || len(teams[0].Members) != 1 {
		t.Errorf("ListTeams after removal = %+v, %v; want one member", teams, err)
	}
}

// TestRepositorySettingsAndVisibility guards local repo settings and the
// admin/team scoping of the repository list.
func TestRepositorySettingsAndVisibility(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()

	admin, err := s.UpsertUserFromGitHub(ctx, 5001, "admin", "")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	member, err := s.UpsertUserFromGitHub(ctx, 5002, "member", "")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	outsider, err := s.UpsertUserFromGitHub(ctx, 5003, "outsider", "")
	if err != nil {
		t.Fatalf("create outsider: %v", err)
	}
	team, err := s.CreateTeam(ctx, "Team One")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if err := s.SetTeamMember(ctx, team.ID, member.ID, "qa"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if err := s.UpsertInstallation(ctx, store.Installation{ID: 88, AccountLogin: "acme", AccountType: "Organization"}); err != nil {
		t.Fatalf("create installation: %v", err)
	}
	for _, r := range []store.Repository{
		{ID: 601, InstallationID: 88, FullName: "acme/one", DefaultBranch: "main"},
		{ID: 602, InstallationID: 88, FullName: "acme/two", DefaultBranch: "main"},
	} {
		if err := s.UpsertRepository(ctx, r); err != nil {
			t.Fatalf("create repository %d: %v", r.ID, err)
		}
	}
	if err := s.AssignRepositoryTeam(ctx, 601, &team.ID); err != nil {
		t.Fatalf("AssignRepositoryTeam: %v", err)
	}
	if err := s.SetRepositoryEnabled(ctx, 601, true); err != nil {
		t.Fatalf("SetRepositoryEnabled: %v", err)
	}

	adminRepos, err := s.ListRepositories(ctx, admin)
	if err != nil || len(adminRepos) != 2 {
		t.Fatalf("ListRepositories(admin) = %d repos, %v; want 2", len(adminRepos), err)
	}
	if !adminRepos[0].Enabled || adminRepos[0].ID != 601 {
		t.Errorf("repo 601 = %+v, want enabled", adminRepos[0])
	}
	memberRepos, err := s.ListRepositories(ctx, member)
	if err != nil || len(memberRepos) != 1 || memberRepos[0].ID != 601 {
		t.Errorf("ListRepositories(member) = %+v, %v; want only repo 601", memberRepos, err)
	}
	outsiderRepos, err := s.ListRepositories(ctx, outsider)
	if err != nil || len(outsiderRepos) != 0 {
		t.Errorf("ListRepositories(outsider) = %+v, %v; want none", outsiderRepos, err)
	}

	if err := s.SetRepositoryEnabled(ctx, 601, false); err != nil {
		t.Fatalf("disable repository: %v", err)
	}
	if err := s.AssignRepositoryTeam(ctx, 601, nil); err != nil {
		t.Fatalf("clear repository team: %v", err)
	}
	memberRepos, err = s.ListRepositories(ctx, member)
	if err != nil || len(memberRepos) != 0 {
		t.Errorf("ListRepositories(member) after unassign = %+v, %v; want none", memberRepos, err)
	}
	adminRepos, err = s.ListRepositories(ctx, admin)
	if err != nil || adminRepos[0].Enabled || adminRepos[0].TeamID != nil {
		t.Errorf("repo 601 settings = %+v, %v; want disabled with no team", adminRepos[0], err)
	}
}

// TestAppConfigNotConfigured guards the bootstrap error and the single-row
// upsert of the GitHub App configuration.
func TestAppConfigNotConfigured(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()

	if _, err := s.LoadAppConfig(ctx); !errors.Is(err, store.ErrNotConfigured) {
		t.Fatalf("LoadAppConfig on empty DB = %v, want ErrNotConfigured", err)
	}

	want := store.AppConfig{
		ID:               1,
		AppID:            12345,
		Slug:             "qa-control-center",
		HTMLURL:          "https://github.com/apps/qa-control-center",
		ClientID:         "Iv1.client",
		ClientSecretEnc:  "enc-secret",
		PrivateKeyEnc:    "enc-key",
		WebhookSecretEnc: "enc-webhook",
	}
	if err := s.SaveAppConfig(ctx, want); err != nil {
		t.Fatalf("SaveAppConfig: %v", err)
	}
	got, err := s.LoadAppConfig(ctx)
	if err != nil {
		t.Fatalf("LoadAppConfig after save: %v", err)
	}
	if got.AppID != want.AppID || got.Slug != want.Slug || got.HTMLURL != want.HTMLURL ||
		got.ClientID != want.ClientID || got.ClientSecretEnc != want.ClientSecretEnc ||
		got.PrivateKeyEnc != want.PrivateKeyEnc || got.WebhookSecretEnc != want.WebhookSecretEnc {
		t.Errorf("LoadAppConfig = %+v, want %+v", got, want)
	}

	want.AppID = 54321
	if err := s.SaveAppConfig(ctx, want); err != nil {
		t.Fatalf("second SaveAppConfig: %v", err)
	}
	got, err = s.LoadAppConfig(ctx)
	if err != nil || got.AppID != 54321 {
		t.Errorf("LoadAppConfig after update = %+v, %v; want app_id=54321", got, err)
	}
	var count int64
	if err := s.DB.WithContext(ctx).Model(&store.AppConfig{}).Count(&count).Error; err != nil || count != 1 {
		t.Errorf("app_configs rows = %d, %v; want 1", count, err)
	}
}

// TestAuditWritesRow guards JSON serialization and nullable actor IDs.
func TestAuditWritesRow(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()

	actor, err := s.UpsertUserFromGitHub(ctx, 6001, "auditor", "")
	if err != nil {
		t.Fatalf("create actor: %v", err)
	}
	if err := s.Audit(ctx, &actor.ID, "team.created", "team", "42", map[string]any{"name": "QA"}); err != nil {
		t.Fatalf("Audit with actor: %v", err)
	}
	if err := s.Audit(ctx, nil, "config.saved", "app_config", "1", nil); err != nil {
		t.Fatalf("Audit without actor: %v", err)
	}

	var rows []store.AuditLog
	if err := s.DB.WithContext(ctx).Order("id").Find(&rows).Error; err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want 2", len(rows))
	}
	if rows[0].ActorUserID == nil || *rows[0].ActorUserID != actor.ID ||
		rows[0].Action != "team.created" || rows[0].TargetType != "team" || rows[0].TargetID != "42" {
		t.Errorf("first audit row = %+v, want actor=%d team.created/team/42", rows[0], actor.ID)
	}
	var data map[string]any
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatalf("unmarshal audit data %s: %v", rows[0].Data, err)
	}
	if data["name"] != "QA" {
		t.Errorf("audit data = %v, want name QA", data)
	}
	if rows[1].ActorUserID != nil || rows[1].Action != "config.saved" ||
		rows[1].TargetType != "app_config" || rows[1].TargetID != "1" {
		t.Errorf("second audit row = %+v, want nil actor config.saved/app_config/1", rows[1])
	}
}
