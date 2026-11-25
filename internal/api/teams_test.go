package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestPatchTeamRenamesAsAdmin(t *testing.T) {
	f := newAPIFixture(t)
	admin := f.user("admin", true)
	team := f.team("qa-pilot")
	cookie := f.session(admin)

	rec := f.request(http.MethodPatch, fmt.Sprintf("/api/teams/%d", team.ID), map[string]any{"name": "platform-qa"}, cookie)
	assertStatus(t, rec, http.StatusOK)

	var body struct {
		ID              uint   `json:"id"`
		Name            string `json:"name"`
		Slug            string `json:"slug"`
		RepositoryCount int    `json:"repository_count"`
	}
	decodeJSON(t, rec, &body)
	if body.ID != team.ID || body.Name != "platform-qa" || body.Slug != "platform-qa" {
		t.Errorf("response = %+v, want id %d renamed to platform-qa", body, team.ID)
	}
	if body.RepositoryCount != 0 {
		t.Errorf("repository_count = %d, want 0", body.RepositoryCount)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw response %q: %v", rec.Body.String(), err)
	}
	if _, ok := raw["members"]; ok {
		t.Error("rename response includes members; it must be a summary without them")
	}

	var got store.Team
	if err := f.st.DB.WithContext(f.ctx).Take(&got, team.ID).Error; err != nil {
		t.Fatalf("reload team: %v", err)
	}
	if got.Name != "platform-qa" || got.Slug != "platform-qa" {
		t.Errorf("stored team = %+v, want name and slug platform-qa", got)
	}

	rows := auditRows(t, f, "team.updated")
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	if rows[0].ActorUserID == nil || *rows[0].ActorUserID != admin.ID {
		t.Errorf("audit actor = %v, want %d", rows[0].ActorUserID, admin.ID)
	}
	if rows[0].TargetType != "team" || rows[0].TargetID != idString(team.ID) {
		t.Errorf("audit target = %s/%s, want team/%s", rows[0].TargetType, rows[0].TargetID, idString(team.ID))
	}
	var data map[string]string
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatalf("decode audit data %s: %v", rows[0].Data, err)
	}
	if data["from"] != "qa-pilot" || data["to"] != "platform-qa" {
		t.Errorf("audit data = %v, want from qa-pilot to platform-qa", data)
	}

	assertEvents(t, f.pub.snapshot(), events.Event{Type: "team.updated", ID: idString(team.ID)})
}

func TestPatchTeamSameNameIsIdempotent(t *testing.T) {
	f := newAPIFixture(t)
	admin := f.user("admin", true)
	team := f.team("qa-pilot")
	cookie := f.session(admin)

	rec := f.request(http.MethodPatch, fmt.Sprintf("/api/teams/%d", team.ID), map[string]any{"name": "qa-pilot"}, cookie)
	assertStatus(t, rec, http.StatusOK)

	var body struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	decodeJSON(t, rec, &body)
	if body.Name != "qa-pilot" || body.Slug != "qa-pilot" {
		t.Errorf("response = %+v, want the unchanged qa-pilot name and slug", body)
	}

	var got store.Team
	if err := f.st.DB.WithContext(f.ctx).Take(&got, team.ID).Error; err != nil {
		t.Fatalf("reload team: %v", err)
	}
	if got.Name != "qa-pilot" || got.Slug != "qa-pilot" {
		t.Errorf("stored team = %+v, want the unchanged qa-pilot name and slug", got)
	}

	rows := auditRows(t, f, "team.updated")
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	var data map[string]string
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatalf("decode audit data %s: %v", rows[0].Data, err)
	}
	if data["from"] != "qa-pilot" || data["to"] != "qa-pilot" {
		t.Errorf("audit data = %v, want from and to qa-pilot", data)
	}
}

func TestPatchTeamRejectsNonAdmin(t *testing.T) {
	f := newAPIFixture(t)
	team := f.team("qa-pilot")
	lead := f.user("lead", false)
	f.member(team.ID, lead.ID, "lead")
	cookie := f.session(lead)

	rec := f.request(http.MethodPatch, fmt.Sprintf("/api/teams/%d", team.ID), map[string]any{"name": "renamed"}, cookie)
	assertStatus(t, rec, http.StatusForbidden)

	var got store.Team
	if err := f.st.DB.WithContext(f.ctx).Take(&got, team.ID).Error; err != nil {
		t.Fatalf("reload team: %v", err)
	}
	if got.Name != "qa-pilot" {
		t.Errorf("team name = %q, want qa-pilot unchanged", got.Name)
	}
}

func TestPatchTeamRejectsEmptyNameAndMissingTeam(t *testing.T) {
	f := newAPIFixture(t)
	team := f.team("qa-pilot")
	cookie := f.session(f.user("admin", true))

	rec := f.request(http.MethodPatch, fmt.Sprintf("/api/teams/%d", team.ID), map[string]any{"name": "   "}, cookie)
	assertStatus(t, rec, http.StatusBadRequest)

	rec = f.request(http.MethodPatch, "/api/teams/not-a-number", map[string]any{"name": "renamed"}, cookie)
	assertStatus(t, rec, http.StatusBadRequest)

	rec = f.request(http.MethodPatch, "/api/teams/999999", map[string]any{"name": "renamed"}, cookie)
	assertStatus(t, rec, http.StatusNotFound)

	rows := auditRows(t, f, "team.updated")
	if len(rows) != 0 {
		t.Errorf("audit rows = %d, want 0 for rejected renames", len(rows))
	}
}

func TestPatchTeamRejectsNameWithoutSlug(t *testing.T) {
	f := newAPIFixture(t)
	team := f.team("qa-pilot")
	cookie := f.session(f.user("admin", true))

	rec := f.request(http.MethodPatch, fmt.Sprintf("/api/teams/%d", team.ID), map[string]any{"name": "!!!"}, cookie)
	assertStatus(t, rec, http.StatusBadRequest)

	var got store.Team
	if err := f.st.DB.WithContext(f.ctx).Take(&got, team.ID).Error; err != nil {
		t.Fatalf("reload team: %v", err)
	}
	if got.Name != "qa-pilot" || got.Slug != "qa-pilot" {
		t.Errorf("team = %+v, want the original qa-pilot name and slug", got)
	}
	if rows := auditRows(t, f, "team.updated"); len(rows) != 0 {
		t.Errorf("audit rows = %d, want 0 for a rejected name", len(rows))
	}
}

func TestPatchTeamReportsNameCollision(t *testing.T) {
	f := newAPIFixture(t)
	team := f.team("alpha")
	other := f.team("beta")
	cookie := f.session(f.user("admin", true))

	rec := f.request(http.MethodPatch, fmt.Sprintf("/api/teams/%d", team.ID), map[string]any{"name": other.Name}, cookie)
	assertStatus(t, rec, http.StatusConflict)

	var got store.Team
	if err := f.st.DB.WithContext(f.ctx).Take(&got, team.ID).Error; err != nil {
		t.Fatalf("reload team: %v", err)
	}
	if got.Name != "alpha" || got.Slug != "alpha" {
		t.Errorf("team = %+v, want the original alpha name and slug", got)
	}
}

func TestPatchTeamReportsSlugCollision(t *testing.T) {
	f := newAPIFixture(t)
	f.team("platform")
	team := f.team("qa team")
	cookie := f.session(f.user("admin", true))

	// The new name differs from the existing team's, but derives the same slug.
	rec := f.request(http.MethodPatch, fmt.Sprintf("/api/teams/%d", team.ID), map[string]any{"name": "Platform!"}, cookie)
	assertStatus(t, rec, http.StatusConflict)

	var got store.Team
	if err := f.st.DB.WithContext(f.ctx).Take(&got, team.ID).Error; err != nil {
		t.Fatalf("reload team: %v", err)
	}
	if got.Name != "qa team" || got.Slug != "qa team" {
		t.Errorf("team = %+v, want the original qa team name and slug", got)
	}
}

func TestDeleteTeamBlockedByRepositories(t *testing.T) {
	f := newAPIFixture(t)
	team := f.team("ops")
	installation := f.installation(f.nextID())
	teamID := team.ID
	repo := f.repo(7101, installation.ID, "acme/payments", &teamID, true)
	cookie := f.session(f.user("admin", true))

	rec := f.request(http.MethodDelete, fmt.Sprintf("/api/teams/%d", team.ID), nil, cookie)
	assertStatus(t, rec, http.StatusConflict)

	var body struct {
		Error        string `json:"error"`
		Repositories []struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	decodeJSON(t, rec, &body)
	if body.Error == "" {
		t.Error("409 body has no error message")
	}
	if len(body.Repositories) != 1 || body.Repositories[0].ID != repo.ID || body.Repositories[0].FullName != repo.FullName {
		t.Errorf("repositories = %+v, want [{id:%d full_name:%s}]", body.Repositories, repo.ID, repo.FullName)
	}

	var count int64
	if err := f.st.DB.WithContext(f.ctx).Model(&store.Team{}).Where("id = ?", team.ID).Count(&count).Error; err != nil {
		t.Fatalf("count team: %v", err)
	}
	if count != 1 {
		t.Errorf("team rows = %d, want the blocked team to remain", count)
	}
}

func TestDeleteTeamRemovesTeamAndAudits(t *testing.T) {
	f := newAPIFixture(t)
	team := f.team("temporary")
	member := f.user("member", false)
	f.member(team.ID, member.ID, "developer")
	admin := f.user("admin", true)
	cookie := f.session(admin)

	rec := f.request(http.MethodDelete, fmt.Sprintf("/api/teams/%d", team.ID), nil, cookie)
	assertStatus(t, rec, http.StatusNoContent)

	var count int64
	if err := f.st.DB.WithContext(f.ctx).Model(&store.Team{}).Where("id = ?", team.ID).Count(&count).Error; err != nil {
		t.Fatalf("count team: %v", err)
	}
	if count != 0 {
		t.Errorf("team rows = %d, want 0 after deletion", count)
	}

	rows := auditRows(t, f, "team.deleted")
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	if rows[0].ActorUserID == nil || *rows[0].ActorUserID != admin.ID {
		t.Errorf("audit actor = %v, want %d", rows[0].ActorUserID, admin.ID)
	}
	if rows[0].TargetType != "team" || rows[0].TargetID != idString(team.ID) {
		t.Errorf("audit target = %s/%s, want team/%s", rows[0].TargetType, rows[0].TargetID, idString(team.ID))
	}
	var data map[string]any
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatalf("decode audit data %s: %v", rows[0].Data, err)
	}
	if data["name"] != "temporary" {
		t.Errorf("audit name = %v, want temporary", data["name"])
	}
	if data["members"] != float64(1) {
		t.Errorf("audit members = %v, want 1", data["members"])
	}

	assertEvents(t, f.pub.snapshot(), events.Event{Type: "team.updated", ID: idString(team.ID)})
}

func TestDeleteTeamRejectsNonAdmin(t *testing.T) {
	f := newAPIFixture(t)
	team := f.team("temporary")
	lead := f.user("lead", false)
	f.member(team.ID, lead.ID, "lead")
	cookie := f.session(lead)

	rec := f.request(http.MethodDelete, fmt.Sprintf("/api/teams/%d", team.ID), nil, cookie)
	assertStatus(t, rec, http.StatusForbidden)

	var count int64
	if err := f.st.DB.WithContext(f.ctx).Model(&store.Team{}).Where("id = ?", team.ID).Count(&count).Error; err != nil {
		t.Fatalf("count team: %v", err)
	}
	if count != 1 {
		t.Errorf("team rows = %d, want the team to remain", count)
	}
}

func TestDeleteTeamRejectsInvalidIDAndMissingTeam(t *testing.T) {
	f := newAPIFixture(t)
	cookie := f.session(f.user("admin", true))

	rec := f.request(http.MethodDelete, "/api/teams/not-a-number", nil, cookie)
	assertStatus(t, rec, http.StatusBadRequest)

	rec = f.request(http.MethodDelete, "/api/teams/0", nil, cookie)
	assertStatus(t, rec, http.StatusBadRequest)

	rec = f.request(http.MethodDelete, "/api/teams/999999", nil, cookie)
	assertStatus(t, rec, http.StatusNotFound)
}

// TestPatchRepoRejectsMissingTeam covers the delete race from the assignment
// side: pointing a repository at a team that no longer exists is a conflict,
// not an internal error.
func TestPatchRepoRejectsMissingTeam(t *testing.T) {
	f := newAPIFixture(t)
	installation := f.installation(f.nextID())
	repo := f.repo(7301, installation.ID, "acme/orphan", nil, true)
	cookie := f.session(f.user("admin", true))

	rec := f.request(http.MethodPatch, fmt.Sprintf("/api/repos/%d", repo.ID), map[string]any{"team_id": 999999}, cookie)
	assertStatus(t, rec, http.StatusConflict)

	var stored store.Repository
	if err := f.st.DB.WithContext(f.ctx).Take(&stored, repo.ID).Error; err != nil {
		t.Fatalf("reload repository: %v", err)
	}
	if stored.TeamID != nil {
		t.Errorf("repository team_id = %v, want nil after the rejected assignment", stored.TeamID)
	}
}

func TestTeamsReportRepositoryCount(t *testing.T) {
	f := newAPIFixture(t)
	cookie := f.session(f.user("viewer", false))
	alpha := f.team("alpha")
	beta := f.team("beta")
	installation := f.installation(f.nextID())
	f.repo(7201, installation.ID, "acme/one", &alpha.ID, true)
	f.repo(7202, installation.ID, "acme/two", &alpha.ID, true)

	rec := f.request(http.MethodGet, "/api/teams", nil, cookie)
	assertStatus(t, rec, http.StatusOK)

	var teams []struct {
		ID              uint `json:"id"`
		RepositoryCount int  `json:"repository_count"`
	}
	decodeJSON(t, rec, &teams)
	counts := make(map[uint]int, len(teams))
	for _, team := range teams {
		counts[team.ID] = team.RepositoryCount
	}
	if counts[alpha.ID] != 2 {
		t.Errorf("alpha repository_count = %d, want 2", counts[alpha.ID])
	}
	if counts[beta.ID] != 0 {
		t.Errorf("beta repository_count = %d, want 0", counts[beta.ID])
	}
}
