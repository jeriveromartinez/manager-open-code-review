package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/jeriveromartinez/qa-control-center/internal/auth"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// repoPolicyOverridesResponse mirrors the stored overrides. A null field means
// the repository inherits the team value; an empty list is an explicit empty
// override.
type repoPolicyOverridesResponse struct {
	ObservationMode      bool                   `json:"observation_mode"`
	BlockingThreshold    *string                `json:"blocking_threshold"`
	ExtraInstructions    *string                `json:"extra_instructions"`
	ExcludeGlobs         *[]string              `json:"exclude_globs"`
	MaxFiles             *int                   `json:"max_files"`
	MaxDiffLines         *int                   `json:"max_diff_lines"`
	MaxPasses            *int                   `json:"max_passes"`
	RequireGreenCI       *bool                  `json:"require_green_ci"`
	RequiredChecks       *[]store.RequiredCheck `json:"required_checks"`
	ReviewContextCeiling *int                   `json:"review_context_ceiling"`
	ReviewEffort         *string                `json:"review_effort"`
	ReviewTokenBudget    *int64                 `json:"review_token_budget"`
	AutoTriageMode       *string                `json:"auto_triage_mode"`
	AutoWithdraw         *bool                  `json:"auto_withdraw"`
	VerifyMaxFindings    *int                   `json:"verify_max_findings"`
	VerifyMaxFiles       *int                   `json:"verify_max_files"`
	VerifyTokenShare     *int                   `json:"verify_token_share"`
	VerifyTimeShare      *int                   `json:"verify_time_share"`
	VerifyTokenMax       *int64                 `json:"verify_token_max"`
}

// repoPolicyResponse is the effective policy of one repository with the
// provenance of every field. Effective and Enforced differ only while the
// repository is in observation mode (#153).
type repoPolicyResponse struct {
	RepositoryID int64                       `json:"repository_id"`
	TeamID       *uint                       `json:"team_id"`
	Inherited    bool                        `json:"inherited"`
	Revision     string                      `json:"revision"`
	Provenance   map[string]string           `json:"provenance"`
	Overrides    repoPolicyOverridesResponse `json:"overrides"`
	Effective    qaPolicyResponse            `json:"effective"`
	Enforced     qaPolicyResponse            `json:"enforced"`
	UpdatedAt    *string                     `json:"updated_at"`
}

func repoPolicyOverridesDTO(p store.RepositoryPolicy) repoPolicyOverridesResponse {
	return repoPolicyOverridesResponse{
		ObservationMode:      p.ObservationMode,
		BlockingThreshold:    p.BlockingThreshold,
		ExtraInstructions:    p.ExtraInstructions,
		ExcludeGlobs:         optionalGlobs(p.ExcludeGlobs),
		MaxFiles:             p.MaxFilesPerPass,
		MaxDiffLines:         p.MaxDiffLinesPerPass,
		MaxPasses:            p.MaxPasses,
		RequireGreenCI:       p.RequireGreenCI,
		RequiredChecks:       optionalChecks(p.RequiredChecks),
		ReviewContextCeiling: p.ReviewContextCeiling,
		ReviewEffort:         p.ReviewEffort,
		ReviewTokenBudget:    p.ReviewTokenBudget,
		AutoTriageMode:       p.AutoTriageMode,
		AutoWithdraw:         p.AutoWithdraw,
		VerifyMaxFindings:    p.VerifyMaxFindings,
		VerifyMaxFiles:       p.VerifyMaxFiles,
		VerifyTokenShare:     p.VerifyTokenShare,
		VerifyTimeShare:      p.VerifyTimeShare,
		VerifyTokenMax:       p.VerifyTokenMax,
	}
}

func optionalGlobs(raw datatypes.JSON) *[]string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var globs []string
	if err := json.Unmarshal(raw, &globs); err != nil {
		return nil
	}
	if globs == nil {
		globs = []string{}
	}
	return &globs
}

func optionalChecks(raw datatypes.JSON) *[]store.RequiredCheck {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	checks := store.ParseRequiredChecks(raw)
	return &checks
}

func (s *server) repoPolicyDTO(ctx context.Context, resolved store.ResolvedPolicy) (repoPolicyResponse, error) {
	global := s.globalContextCeiling(ctx)
	promptCache, err := s.policyPromptCache(ctx, resolved.Effective.LLMProfileID)
	if err != nil {
		return repoPolicyResponse{}, err
	}

	var overrides repoPolicyOverridesResponse
	var updatedAt *string
	if resolved.Override != nil {
		overrides = repoPolicyOverridesDTO(*resolved.Override)
		formatted := resolved.Override.UpdatedAt.UTC().Format(time.RFC3339)
		updatedAt = &formatted
	}
	var teamID *uint
	if resolved.Team.TeamID != 0 {
		id := resolved.Team.TeamID
		teamID = &id
	}
	return repoPolicyResponse{
		RepositoryID: resolved.RepositoryID,
		TeamID:       teamID,
		Inherited:    resolved.Override == nil,
		Revision:     resolved.Revision,
		Provenance:   resolved.Provenance,
		Overrides:    overrides,
		Effective:    qaPolicyDTO(resolved.Effective, global, promptCache),
		Enforced:     qaPolicyDTO(resolved.Enforced, global, promptCache),
		UpdatedAt:    updatedAt,
	}, nil
}

// repoForPolicy loads the repository addressed by the {id} route parameter.
func (s *server) repoForPolicy(w http.ResponseWriter, r *http.Request) (store.Repository, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid repository id")
		return store.Repository{}, false
	}
	var repo store.Repository
	if err := s.Store.DB.WithContext(r.Context()).Take(&repo, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeAPIError(w, http.StatusNotFound, "repository not found")
			return store.Repository{}, false
		}
		writeAPIError(w, http.StatusInternalServerError, "load repository")
		return store.Repository{}, false
	}
	// Authorization comes before the missing-team conflict, so an outsider
	// cannot use the status code to learn that the repository has no team.
	user, signedIn := auth.UserFrom(r.Context())
	authorized := signedIn && user.IsAdmin
	if !authorized && signedIn && repo.TeamID != nil {
		authorized = s.canManageTeam(r, *repo.TeamID)
	}
	if !authorized {
		writeAPIError(w, http.StatusForbidden, "forbidden")
		return store.Repository{}, false
	}
	if repo.TeamID == nil {
		writeAPIError(w, http.StatusConflict, "repository has no team")
		return store.Repository{}, false
	}
	return repo, true
}

// getRepoPolicy returns the effective policy of a repository, its provenance,
// the revision and the stored overrides.
func (s *server) getRepoPolicy(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repoForPolicy(w, r)
	if !ok {
		return
	}
	resolved, err := s.Store.ResolveReviewPolicy(r.Context(), repo.ID)
	if err != nil {
		writeRepositoryPolicyError(w, err)
		return
	}
	result, err := s.repoPolicyDTO(r.Context(), resolved)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "load repository policy")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// putRepoPolicy replaces the override of a repository. Only the fields present
// in the body are overridden; a null or absent field inherits the team value,
// and a body without any override clears the row.
func (s *server) putRepoPolicy(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repoForPolicy(w, r)
	if !ok {
		return
	}
	raw, ok := decodeRawBody(w, r)
	if !ok {
		return
	}

	policy := store.RepositoryPolicy{RepositoryID: repo.ID}
	var observationMode *bool
	if !decodeNullable(w, raw, "observation_mode", &observationMode) {
		return
	}
	if observationMode != nil {
		policy.ObservationMode = *observationMode
	}
	if !decodeNullable(w, raw, "blocking_threshold", &policy.BlockingThreshold) ||
		!decodeNullable(w, raw, "extra_instructions", &policy.ExtraInstructions) ||
		!decodeNullable(w, raw, "max_files", &policy.MaxFilesPerPass) ||
		!decodeNullable(w, raw, "max_diff_lines", &policy.MaxDiffLinesPerPass) ||
		!decodeNullable(w, raw, "max_passes", &policy.MaxPasses) ||
		!decodeNullable(w, raw, "require_green_ci", &policy.RequireGreenCI) ||
		!decodeNullable(w, raw, "review_context_ceiling", &policy.ReviewContextCeiling) ||
		!decodeNullable(w, raw, "review_effort", &policy.ReviewEffort) ||
		!decodeNullable(w, raw, "review_token_budget", &policy.ReviewTokenBudget) ||
		!decodeNullable(w, raw, "auto_triage_mode", &policy.AutoTriageMode) ||
		!decodeNullable(w, raw, "auto_withdraw", &policy.AutoWithdraw) ||
		!decodeNullable(w, raw, "verify_max_findings", &policy.VerifyMaxFindings) ||
		!decodeNullable(w, raw, "verify_max_files", &policy.VerifyMaxFiles) ||
		!decodeNullable(w, raw, "verify_token_share", &policy.VerifyTokenShare) ||
		!decodeNullable(w, raw, "verify_time_share", &policy.VerifyTimeShare) ||
		!decodeNullable(w, raw, "verify_token_max", &policy.VerifyTokenMax) {
		return
	}

	if value, present := raw["exclude_globs"]; present && !isJSONNull(value) {
		var globs []string
		if err := json.Unmarshal(value, &globs); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid exclude_globs")
			return
		}
		encoded, err := json.Marshal(globs)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid exclude_globs")
			return
		}
		policy.ExcludeGlobs = datatypes.JSON(encoded)
	}
	if value, present := raw["required_checks"]; present && !isJSONNull(value) {
		var checks []store.RequiredCheck
		if err := json.Unmarshal(value, &checks); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid required_checks")
			return
		}
		encoded, err := store.NormalizeRequiredChecks(checks)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid required_checks")
			return
		}
		policy.RequiredChecks = encoded
	}

	actor, _ := auth.UserFrom(r.Context())
	if !hasRepositoryOverrides(policy) {
		s.clearRepoPolicy(w, r, repo, actor.ID, "repo.policy.cleared")
		return
	}

	saved, err := s.Store.SaveRepositoryPolicy(r.Context(), policy)
	if err != nil {
		if errors.Is(err, store.ErrInvalidPolicy) {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "save repository policy")
		return
	}
	resolved, err := s.Store.ResolveReviewPolicy(r.Context(), repo.ID)
	if err != nil {
		writeRepositoryPolicyError(w, err)
		return
	}
	result, err := s.repoPolicyDTO(r.Context(), resolved)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "load repository policy")
		return
	}
	auditData := map[string]any{
		"revision":         resolved.Revision,
		"observation_mode": saved.ObservationMode,
		"overrides":        saved,
	}
	if err := s.Store.Audit(r.Context(), &actor.ID, "repo.policy.updated", "repo",
		strconv.FormatInt(repo.ID, 10), auditData); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "write audit log")
		return
	}
	s.notify(r.Context(), events.Event{Type: "repo.updated", ID: strconv.FormatInt(repo.ID, 10)})
	writeJSON(w, http.StatusOK, result)
}

// deleteRepoPolicy removes the override and returns the repository to
// inheritance.
func (s *server) deleteRepoPolicy(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repoForPolicy(w, r)
	if !ok {
		return
	}
	actor, _ := auth.UserFrom(r.Context())
	s.clearRepoPolicy(w, r, repo, actor.ID, "repo.policy.cleared")
}

func (s *server) clearRepoPolicy(w http.ResponseWriter, r *http.Request, repo store.Repository, actorID uint, action string) {
	if err := s.Store.DeleteRepositoryPolicy(r.Context(), repo.ID); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "clear repository policy")
		return
	}
	resolved, err := s.Store.ResolveReviewPolicy(r.Context(), repo.ID)
	if err != nil {
		writeRepositoryPolicyError(w, err)
		return
	}
	result, err := s.repoPolicyDTO(r.Context(), resolved)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "load repository policy")
		return
	}
	if err := s.Store.Audit(r.Context(), &actorID, action, "repo",
		strconv.FormatInt(repo.ID, 10), map[string]any{"revision": resolved.Revision}); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "write audit log")
		return
	}
	s.notify(r.Context(), events.Event{Type: "repo.updated", ID: strconv.FormatInt(repo.ID, 10)})
	writeJSON(w, http.StatusOK, result)
}

// writeRepositoryPolicyError maps a policy resolution failure to the status
// the API promises: a missing repository is 404, a repository without a team
// policy is a conflict.
func writeRepositoryPolicyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "repository not found")
	case errors.Is(err, store.ErrNoReviewConfig):
		writeAPIError(w, http.StatusConflict, "repository has no applicable policy")
	default:
		writeAPIError(w, http.StatusInternalServerError, "load repository policy")
	}
}

// decodeNullable reads an optional scalar. An absent key or an explicit null
// clears the override; a decode failure answers 400 and returns false.
func decodeNullable[T any](w http.ResponseWriter, raw map[string]json.RawMessage, key string, dst **T) bool {
	value, present := raw[key]
	if !present || isJSONNull(value) {
		return true
	}
	var parsed T
	if err := json.Unmarshal(value, &parsed); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid "+key)
		return false
	}
	*dst = &parsed
	return true
}

func hasRepositoryOverrides(p store.RepositoryPolicy) bool {
	return p.ObservationMode ||
		p.BlockingThreshold != nil || p.ExtraInstructions != nil ||
		p.ExcludeGlobs != nil || p.MaxFilesPerPass != nil ||
		p.MaxDiffLinesPerPass != nil || p.MaxPasses != nil ||
		p.RequireGreenCI != nil || p.RequiredChecks != nil ||
		p.ReviewContextCeiling != nil || p.ReviewEffort != nil ||
		p.ReviewTokenBudget != nil || p.AutoTriageMode != nil ||
		p.AutoWithdraw != nil || p.VerifyMaxFindings != nil ||
		p.VerifyMaxFiles != nil || p.VerifyTokenShare != nil ||
		p.VerifyTimeShare != nil || p.VerifyTokenMax != nil
}
