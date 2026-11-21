package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"github.com/jeriveromartinez/qa-control-center/internal/auth"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// Response DTOs keep the JSON wire format snake_case and independent of the
// store models' Go field names.
type prResponse struct {
	ID           uint   `json:"id"`
	Repo         string `json:"repo"`
	Number       int    `json:"number"`
	Title        string `json:"title"`
	AuthorLogin  string `json:"author_login"`
	HTMLURL      string `json:"html_url"`
	QAStatus     string `json:"qa_status"`
	Group        string `json:"group"`
	ReviewState  string `json:"review_state"`
	ReviewReason string `json:"review_reason"`
	Validations  struct {
		Passed int `json:"passed"`
		Total  int `json:"total"`
	} `json:"validations"`
	UpdatedAt string `json:"updated_at"`
}

func prDTO(v store.PRView) prResponse {
	out := prResponse{
		ID:           v.ID,
		Repo:         v.Repo,
		Number:       v.Number,
		Title:        v.Title,
		AuthorLogin:  v.AuthorLogin,
		HTMLURL:      v.HTMLURL,
		QAStatus:     v.QAStatus,
		Group:        v.Group,
		ReviewState:  v.ReviewState,
		ReviewReason: v.ReviewReason,
		UpdatedAt:    v.UpdatedAt.UTC().Format(time.RFC3339),
	}
	out.Validations.Passed = v.Validations.Passed
	out.Validations.Total = v.Validations.Total
	return out
}

type statsResponse struct {
	Open          int `json:"open"`
	AwaitingHuman int `json:"awaiting_human"`
	InEvaluation  int `json:"in_evaluation"`
	WithProblems  int `json:"with_problems"`
}

type readinessItemResponse struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Count int    `json:"count"`
	Link  string `json:"link"`
}

type readinessResponse struct {
	Ready bool                    `json:"ready"`
	Items []readinessItemResponse `json:"items"`
}

func readinessLabel(count int, singular, plural string) string {
	word := plural
	if count == 1 {
		word = singular
	}
	return strconv.Itoa(count) + " " + word
}

func statsDTO(s store.Stats) statsResponse {
	return statsResponse{
		Open:          s.Open,
		AwaitingHuman: s.AwaitingHuman,
		InEvaluation:  s.InEvaluation,
		WithProblems:  s.WithProblems,
	}
}

type teamRoleResponse struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

type meResponse struct {
	ID        uint               `json:"id"`
	Login     string             `json:"login"`
	AvatarURL string             `json:"avatar_url"`
	IsAdmin   bool               `json:"is_admin"`
	Teams     []teamRoleResponse `json:"teams"`
}

type repoResponse struct {
	ID             int64  `json:"id"`
	InstallationID int64  `json:"installation_id"`
	FullName       string `json:"full_name"`
	Private        bool   `json:"private"`
	DefaultBranch  string `json:"default_branch"`
	Enabled        bool   `json:"enabled"`
	TeamID         *uint  `json:"team_id"`
	HasPolicy      bool   `json:"has_policy"`
	AccountLogin   string `json:"account_login"`
}

func repoDTO(r store.RepositoryWithPolicy, accountLogin string) repoResponse {
	return repoResponse{
		ID:             r.ID,
		InstallationID: r.InstallationID,
		FullName:       r.FullName,
		Private:        r.Private,
		DefaultBranch:  r.DefaultBranch,
		Enabled:        r.Enabled,
		TeamID:         r.TeamID,
		HasPolicy:      r.TeamHasPolicy,
		AccountLogin:   accountLogin,
	}
}

type installationResponse struct {
	ID           int64   `json:"id"`
	AccountLogin string  `json:"account_login"`
	AccountType  string  `json:"account_type"`
	SuspendedAt  *string `json:"suspended_at"`
}

type teamMemberResponse struct {
	UserID    uint   `json:"user_id"`
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
	Role      string `json:"role"`
}

type teamResponse struct {
	ID      uint                 `json:"id"`
	Name    string               `json:"name"`
	Slug    string               `json:"slug"`
	Members []teamMemberResponse `json:"members"`
}

func teamDTO(t store.TeamWithMembers) teamResponse {
	members := make([]teamMemberResponse, 0, len(t.Members))
	for _, m := range t.Members {
		members = append(members, teamMemberResponse{
			UserID:    m.User.ID,
			Login:     m.User.Login,
			AvatarURL: m.User.AvatarURL,
			Role:      m.Role,
		})
	}
	return teamResponse{ID: t.ID, Name: t.Name, Slug: t.Slug, Members: members}
}

type userResponse struct {
	ID        uint   `json:"id"`
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
	IsAdmin   bool   `json:"is_admin"`
}

func userDTO(u store.User) userResponse {
	return userResponse{ID: u.ID, Login: u.Login, AvatarURL: u.AvatarURL, IsAdmin: u.IsAdmin}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// healthz reports readiness, not just liveness: the process cannot do useful
// work without PostgreSQL, and compose uses this endpoint to decide whether the
// worker may start. A cheap ping with a short deadline keeps the 30 s probe from
// turning a slow database into a restart loop.
func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded", "error": "database not configured"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Store.Ping(ctx); err != nil {
		slog.Error("health check: database unavailable", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "degraded",
			"error":  "database unavailable",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) me(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFrom(r.Context())
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	teams := []teamRoleResponse{}
	all, err := s.Store.ListTeams(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list teams")
		return
	}
	for _, team := range all {
		for _, member := range team.Members {
			if member.User.ID == user.ID {
				teams = append(teams, teamRoleResponse{ID: team.ID, Name: team.Name, Role: member.Role})
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, meResponse{
		ID:        user.ID,
		Login:     user.Login,
		AvatarURL: user.AvatarURL,
		IsAdmin:   user.IsAdmin,
		Teams:     teams,
	})
}

func (s *server) stats(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	stats, err := s.Store.Stats(r.Context(), user)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "compute stats")
		return
	}
	writeJSON(w, http.StatusOK, statsDTO(stats))
}

// readiness lists the configuration links that are still missing, so the panel
// can point the administrator at the screen where each one is fixed.
func (s *server) readiness(w http.ResponseWriter, r *http.Request) {
	report, err := s.Store.ReadinessReport(r.Context(), s.Cfg.LLMHealthStaleAfter())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "compute readiness")
		return
	}

	items := make([]readinessItemResponse, 0, 5)
	if report.NoTeams {
		items = append(items, readinessItemResponse{
			Key:   "no_teams",
			Label: "No hay equipos: crea uno para asignar repositorios",
			Count: 1,
			Link:  "/teams",
		})
	}
	if report.NoProfiles {
		items = append(items, readinessItemResponse{
			Key:   "no_profiles",
			Label: "No hay perfiles LLM: añade uno y comprueba su salud",
			Count: 1,
			Link:  "/agents",
		})
	}
	if report.ReposWithoutTeam > 0 {
		items = append(items, readinessItemResponse{
			Key: "repos_without_team",
			Label: readinessLabel(
				report.ReposWithoutTeam,
				"repositorio habilitado sin equipo asignado",
				"repositorios habilitados sin equipo asignado",
			),
			Count: report.ReposWithoutTeam,
			Link:  "/repos",
		})
	}
	if report.TeamsWithoutPolicy > 0 {
		items = append(items, readinessItemResponse{
			Key: "teams_without_policy",
			Label: readinessLabel(
				report.TeamsWithoutPolicy,
				"equipo sin política de revisión",
				"equipos sin política de revisión",
			),
			Count: report.TeamsWithoutPolicy,
			Link:  "/teams",
		})
	}
	if report.UnhealthyProfiles > 0 {
		items = append(items, readinessItemResponse{
			Key: "unhealthy_profiles",
			Label: readinessLabel(
				report.UnhealthyProfiles,
				"perfil LLM habilitado sin estado saludable",
				"perfiles LLM habilitados sin estado saludable",
			),
			Count: report.UnhealthyProfiles,
			Link:  "/agents",
		})
	}

	writeJSON(w, http.StatusOK, readinessResponse{Ready: len(items) == 0, Items: items})
}

func (s *server) prs(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	views, err := s.Store.ListVisiblePullRequests(r.Context(), user, store.PRFilter{Group: r.URL.Query().Get("group")})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list pull requests")
		return
	}
	result := make([]prResponse, 0, len(views))
	for _, v := range views {
		result = append(result, prDTO(v))
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) repos(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	repos, err := s.Store.ListRepositoriesWithPolicy(r.Context(), user)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list repositories")
		return
	}
	installations, err := s.Store.ListInstallations(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list installations")
		return
	}
	byInstallation := make(map[int64]string, len(installations))
	for _, in := range installations {
		byInstallation[in.ID] = in.AccountLogin
	}
	result := make([]repoResponse, 0, len(repos))
	for _, repo := range repos {
		result = append(result, repoDTO(repo, byInstallation[repo.InstallationID]))
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) installations(w http.ResponseWriter, r *http.Request) {
	installations, err := s.Store.ListInstallations(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list installations")
		return
	}
	sort.Slice(installations, func(i, j int) bool {
		li := strings.ToLower(installations[i].AccountLogin)
		lj := strings.ToLower(installations[j].AccountLogin)
		if li == lj {
			return installations[i].AccountLogin < installations[j].AccountLogin
		}
		return li < lj
	})
	result := make([]installationResponse, 0, len(installations))
	for _, in := range installations {
		var suspendedAt *string
		if in.SuspendedAt != nil {
			formatted := in.SuspendedAt.UTC().Format(time.RFC3339)
			suspendedAt = &formatted
		}
		result = append(result, installationResponse{
			ID:           in.ID,
			AccountLogin: in.AccountLogin,
			AccountType:  in.AccountType,
			SuspendedAt:  suspendedAt,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) teams(w http.ResponseWriter, r *http.Request) {
	teams, err := s.Store.ListTeams(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list teams")
		return
	}
	result := make([]teamResponse, 0, len(teams))
	for _, team := range teams {
		result = append(result, teamDTO(team))
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) users(w http.ResponseWriter, r *http.Request) {
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list users")
		return
	}
	result := make([]userResponse, 0, len(users))
	for _, u := range users {
		result = append(result, userDTO(u))
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) events(w http.ResponseWriter, r *http.Request) {
	if s.Broker == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "event broker unavailable")
		return
	}
	s.Broker.ServeSSE(w, r)
}

// patchRepo applies only the keys present in the JSON body: an absent key is
// untouched and an explicit null team_id clears the assignment (R6).
func (s *server) patchRepo(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid repository id")
		return
	}

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	var enabled *bool
	if value, ok := raw["enabled"]; ok && !isJSONNull(value) {
		var v bool
		if err := json.Unmarshal(value, &v); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid enabled value")
			return
		}
		enabled = &v
	}

	var teamValue *uint
	hasTeam := false
	if value, ok := raw["team_id"]; ok {
		hasTeam = true
		if !isJSONNull(value) {
			var id uint
			if err := json.Unmarshal(value, &id); err != nil || id == 0 {
				writeAPIError(w, http.StatusBadRequest, "invalid team_id value")
				return
			}
			teamValue = &id
		}
	}

	var repo store.Repository
	if err := s.Store.DB.WithContext(r.Context()).Take(&repo, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeAPIError(w, http.StatusNotFound, "repository not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "load repository")
		return
	}

	if enabled != nil && *enabled != repo.Enabled {
		if err := s.Store.SetRepositoryEnabled(r.Context(), id, *enabled); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "update repository")
			return
		}
	}
	if hasTeam {
		if err := s.Store.AssignRepositoryTeam(r.Context(), id, teamValue); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "assign repository team")
			return
		}
	}

	actor, _ := auth.UserFrom(r.Context())
	data := map[string]any{}
	if enabled != nil {
		data["enabled"] = *enabled
	}
	if hasTeam {
		data["team_id"] = teamValue
	}
	if err := s.Store.Audit(r.Context(), &actor.ID, "repo.updated", "repo", strconv.FormatInt(id, 10), data); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "write audit log")
		return
	}
	if s.Events != nil {
		if err := s.Events.Publish(r.Context(), events.Event{Type: "repo.updated", ID: strconv.FormatInt(id, 10)}); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "publish event")
			return
		}
	}

	// Enabling a repository makes its pull requests visible, so schedule the
	// first synchronization of its installation.
	if enabled != nil && *enabled && !repo.Enabled && s.Jobs != nil {
		if err := s.Jobs.EnqueueSync(r.Context(), repo.InstallationID); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "enqueue sync")
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

func isJSONNull(value json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(value), []byte("null"))
}

func (s *server) createTeam(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Name == "" {
		writeAPIError(w, http.StatusBadRequest, "name is required")
		return
	}
	team, err := s.Store.CreateTeam(r.Context(), body.Name)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "create team")
		return
	}
	actor, _ := auth.UserFrom(r.Context())
	if err := s.Store.Audit(r.Context(), &actor.ID, "team.created", "team", strconv.FormatUint(uint64(team.ID), 10), map[string]any{"name": team.Name}); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "write audit log")
		return
	}
	writeJSON(w, http.StatusCreated, teamResponse{ID: team.ID, Name: team.Name, Slug: team.Slug, Members: []teamMemberResponse{}})
}

// canManageTeam allows administrators and leads of that particular team.
func (s *server) canManageTeam(r *http.Request, teamID uint) bool {
	user, ok := auth.UserFrom(r.Context())
	if !ok {
		return false
	}
	if user.IsAdmin {
		return true
	}
	role, err := s.Store.TeamRole(r.Context(), user.ID, teamID)
	return err == nil && role == "lead"
}

func (s *server) teamMemberIDs(w http.ResponseWriter, r *http.Request) (uint, uint, bool) {
	teamID, err := strconv.ParseUint(chi.URLParam(r, "id"), 10, 64)
	if err != nil || teamID == 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid team id")
		return 0, 0, false
	}
	userID, err := strconv.ParseUint(chi.URLParam(r, "userId"), 10, 64)
	if err != nil || userID == 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid user id")
		return 0, 0, false
	}
	return uint(teamID), uint(userID), true
}

func (s *server) auditTeamMember(r *http.Request, action string, teamID, userID uint, data any) error {
	actor, _ := auth.UserFrom(r.Context())
	return s.Store.Audit(r.Context(), &actor.ID, action, "team_member", strconv.FormatUint(uint64(teamID), 10)+":"+strconv.FormatUint(uint64(userID), 10), data)
}

func (s *server) setTeamMember(w http.ResponseWriter, r *http.Request) {
	teamID, userID, ok := s.teamMemberIDs(w, r)
	if !ok {
		return
	}
	if !s.canManageTeam(r, teamID) {
		writeAPIError(w, http.StatusForbidden, "forbidden")
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := s.Store.SetTeamMember(r.Context(), teamID, userID, body.Role); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid team role")
		return
	}
	if err := s.auditTeamMember(r, "team.member.added", teamID, userID, map[string]any{"role": body.Role}); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "write audit log")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) removeTeamMember(w http.ResponseWriter, r *http.Request) {
	teamID, userID, ok := s.teamMemberIDs(w, r)
	if !ok {
		return
	}
	if !s.canManageTeam(r, teamID) {
		writeAPIError(w, http.StatusForbidden, "forbidden")
		return
	}
	if err := s.Store.RemoveTeamMember(r.Context(), teamID, userID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "team member not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "remove team member")
		return
	}
	if err := s.auditTeamMember(r, "team.member.removed", teamID, userID, map[string]any{}); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "write audit log")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
