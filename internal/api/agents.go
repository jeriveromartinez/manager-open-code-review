package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// llmProfileResponse is the public shape of an LLM profile. The sealed API key
// is deliberately replaced by a boolean: neither the ciphertext nor the
// plaintext ever leaves the server.
type llmProfileResponse struct {
	ID              uint    `json:"id"`
	Name            string  `json:"name"`
	Provider        string  `json:"provider"`
	Model           string  `json:"model"`
	Variant         *string `json:"variant"`
	Mode            string  `json:"mode"`
	BaseURL         *string `json:"base_url"`
	Concurrency     int     `json:"concurrency"`
	Enabled         bool    `json:"enabled"`
	APIKeySet       bool    `json:"api_key_set"`
	HealthStatus    string  `json:"health_status"`
	HealthCheckedAt *string `json:"health_checked_at"`
	HealthError     string  `json:"health_error"`
	// HealthAgeSeconds is how old the verdict is; HealthStale says the worker
	// stopped refreshing it, so "healthy" is not mistaken for current.
	HealthAgeSeconds int    `json:"health_age_seconds"`
	HealthStale      bool   `json:"health_stale"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

func formatTimePtr(v *time.Time) *string {
	if v == nil {
		return nil
	}
	formatted := v.UTC().Format(time.RFC3339)
	return &formatted
}

func llmProfileDTO(p store.LLMProfile, health store.LLMProfileHealth) llmProfileResponse {
	return llmProfileResponse{
		ID:               p.ID,
		Name:             p.Name,
		Provider:         p.Provider,
		Model:            p.Model,
		Variant:          p.Variant,
		Mode:             p.Mode,
		BaseURL:          p.BaseURL,
		Concurrency:      p.Concurrency,
		Enabled:          p.Enabled,
		APIKeySet:        p.APIKeyEnc != "",
		HealthStatus:     p.HealthStatus,
		HealthCheckedAt:  formatTimePtr(p.HealthCheckedAt),
		HealthError:      p.HealthError,
		HealthAgeSeconds: health.AgeSeconds,
		HealthStale:      health.Stale,
		CreatedAt:        p.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:        p.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// llmProfileDTOs pairs every profile with its verdict age. The staleness window
// is twice QA_LLM_HEALTH_INTERVAL: one delayed sweep must not mark all profiles
// stale, while a stopped sweeper still shows up within one cycle.
func llmProfileDTOs(profiles []store.LLMProfile, now time.Time, staleAfter time.Duration) []llmProfileResponse {
	health := store.ProfileHealthSince(profiles, now, staleAfter)
	result := make([]llmProfileResponse, 0, len(profiles))
	for i, profile := range profiles {
		result = append(result, llmProfileDTO(profile, health[i]))
	}
	return result
}

func (s *server) healthOf(p store.LLMProfile) store.LLMProfileHealth {
	return store.ProfileHealthSince([]store.LLMProfile{p}, time.Now(), s.Cfg.LLMHealthStaleAfter())[0]
}

func parseUintID(w http.ResponseWriter, r *http.Request) (uint, bool) {
	id, err := strconv.ParseUint(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id == 0 || uint64(uint(id)) != id {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return uint(id), true
}

func decodeRawBody(w http.ResponseWriter, r *http.Request) (map[string]json.RawMessage, bool) {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil || raw == nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body")
		return nil, false
	}
	return raw, true
}

func (s *server) llmProfiles(w http.ResponseWriter, r *http.Request) {
	profiles, err := s.Store.ListLLMProfiles(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list llm profiles")
		return
	}
	result := llmProfileDTOs(profiles, time.Now(), s.Cfg.LLMHealthStaleAfter())
	writeJSON(w, http.StatusOK, result)
}

func (s *server) createLLMProfile(w http.ResponseWriter, r *http.Request) {
	raw, ok := decodeRawBody(w, r)
	if !ok {
		return
	}
	var body struct {
		Name        string  `json:"name"`
		Provider    string  `json:"provider"`
		Model       string  `json:"model"`
		Variant     *string `json:"variant"`
		Mode        string  `json:"mode"`
		APIKey      string  `json:"api_key"`
		BaseURL     *string `json:"base_url"`
		Concurrency *int    `json:"concurrency"`
		Enabled     *bool   `json:"enabled"`
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := json.Unmarshal(encoded, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Provider == "" || body.Model == "" || body.APIKey == "" {
		writeAPIError(w, http.StatusBadRequest, "provider, model and api_key are required")
		return
	}
	if s.WorkerBox == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "worker key unavailable")
		return
	}
	sealed, err := s.WorkerBox.Seal([]byte(body.APIKey))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "seal api key")
		return
	}
	name := body.Name
	if name == "" {
		name = body.Model
	}
	concurrency := 1
	if body.Concurrency != nil {
		concurrency = *body.Concurrency
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	created, err := s.Store.CreateLLMProfile(r.Context(), store.LLMProfile{
		Name: name, Provider: body.Provider, Model: body.Model,
		Variant: body.Variant, Mode: body.Mode, APIKeyEnc: sealed,
		BaseURL: body.BaseURL, Concurrency: concurrency, Enabled: enabled,
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "create llm profile")
		return
	}
	writeJSON(w, http.StatusCreated, llmProfileDTO(created, s.healthOf(created)))
}

// patchLLMProfile applies only the keys present in the body. An absent or
// empty api_key leaves the stored ciphertext untouched: UpdateLLMProfile
// preserves it when APIKeyEnc is empty.
func (s *server) patchLLMProfile(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUintID(w, r)
	if !ok {
		return
	}
	raw, ok := decodeRawBody(w, r)
	if !ok {
		return
	}
	current, err := s.Store.LLMProfile(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "llm profile not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "load llm profile")
		return
	}

	applyString := func(key string, dst *string) bool {
		value, present := raw[key]
		if !present {
			return true
		}
		if err := json.Unmarshal(value, dst); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid "+key)
			return false
		}
		return true
	}
	applyOptionalString := func(key string, dst **string) bool {
		value, present := raw[key]
		if !present {
			return true
		}
		var v string
		if err := json.Unmarshal(value, &v); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid "+key)
			return false
		}
		*dst = &v
		return true
	}

	for _, field := range []struct {
		key string
		dst *string
	}{
		{"name", &current.Name},
		{"provider", &current.Provider},
		{"model", &current.Model},
		{"mode", &current.Mode},
	} {
		if !applyString(field.key, field.dst) {
			return
		}
	}
	if !applyOptionalString("variant", &current.Variant) || !applyOptionalString("base_url", &current.BaseURL) {
		return
	}
	if current.Name == "" || current.Provider == "" || current.Model == "" {
		writeAPIError(w, http.StatusBadRequest, "name, provider and model cannot be empty")
		return
	}

	if value, present := raw["concurrency"]; present {
		if err := json.Unmarshal(value, &current.Concurrency); err != nil || current.Concurrency < 0 {
			writeAPIError(w, http.StatusBadRequest, "invalid concurrency")
			return
		}
	}
	if value, present := raw["enabled"]; present {
		if err := json.Unmarshal(value, &current.Enabled); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid enabled")
			return
		}
	}
	if value, present := raw["api_key"]; present && !isJSONNull(value) {
		var key string
		if err := json.Unmarshal(value, &key); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid api_key")
			return
		}
		if key != "" {
			if s.WorkerBox == nil {
				writeAPIError(w, http.StatusServiceUnavailable, "worker key unavailable")
				return
			}
			sealed, err := s.WorkerBox.Seal([]byte(key))
			if err != nil {
				writeAPIError(w, http.StatusInternalServerError, "seal api key")
				return
			}
			current.APIKeyEnc = sealed
		}
	}

	if err := s.Store.UpdateLLMProfile(r.Context(), current); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "llm profile not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "update llm profile")
		return
	}
	updated, err := s.Store.LLMProfile(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "load llm profile")
		return
	}
	writeJSON(w, http.StatusOK, llmProfileDTO(updated, s.healthOf(updated)))
}

func (s *server) deleteLLMProfile(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUintID(w, r)
	if !ok {
		return
	}
	switch err := s.Store.DeleteLLMProfile(r.Context(), id); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrProfileInUse):
		writeAPIError(w, http.StatusConflict, "llm profile is in use by a policy")
	case errors.Is(err, store.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "llm profile not found")
	default:
		writeAPIError(w, http.StatusInternalServerError, "delete llm profile")
	}
}

func (s *server) enqueueLLMHealth(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUintID(w, r)
	if !ok {
		return
	}
	if _, err := s.Store.LLMProfile(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "llm profile not found")
		return
	} else if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "load llm profile")
		return
	}
	if s.Health == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "health checker unavailable")
		return
	}
	if err := s.Health.EnqueueLLMHealth(r.Context(), id); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "enqueue llm health check")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

type qaPolicyResponse struct {
	ID                uint     `json:"id"`
	TeamID            uint     `json:"team_id"`
	LLMProfileID      uint     `json:"llm_profile_id"`
	BlockingThreshold string   `json:"blocking_threshold"`
	ExtraInstructions string   `json:"extra_instructions"`
	ExcludeGlobs      []string `json:"exclude_globs"`
	MaxFiles          int      `json:"max_files"`
	MaxDiffLines      int      `json:"max_diff_lines"`
	MaxPasses         int      `json:"max_passes"`
	UpdatedAt         string   `json:"updated_at"`
}

func qaPolicyDTO(p store.QAPolicy) qaPolicyResponse {
	var globs []string
	if err := json.Unmarshal(p.ExcludeGlobs, &globs); err != nil {
		globs = []string{}
	}
	if globs == nil {
		globs = []string{}
	}
	return qaPolicyResponse{
		ID: p.ID, TeamID: p.TeamID, LLMProfileID: p.LLMProfileID,
		BlockingThreshold: p.BlockingThreshold, ExtraInstructions: p.ExtraInstructions,
		ExcludeGlobs: globs, MaxFiles: p.MaxFilesPerPass,
		MaxDiffLines: p.MaxDiffLinesPerPass, MaxPasses: p.MaxPasses,
		UpdatedAt: p.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// getPolicy returns the saved policy, or the instance defaults for a team that
// has not configured one yet, so the UI always has a complete form to render.
func (s *server) getPolicy(w http.ResponseWriter, r *http.Request) {
	teamID, ok := parseUintID(w, r)
	if !ok {
		return
	}
	if !s.canManageTeam(r, teamID) {
		writeAPIError(w, http.StatusForbidden, "forbidden")
		return
	}
	policy, err := s.Store.QAPolicyForTeam(r.Context(), teamID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, qaPolicyResponse{
			TeamID: teamID, BlockingThreshold: "major", ExcludeGlobs: []string{},
			MaxFiles: 40, MaxDiffLines: 3000, MaxPasses: 4,
		})
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "load qa policy")
		return
	}
	writeJSON(w, http.StatusOK, qaPolicyDTO(policy))
}

var validBlockingThresholds = map[string]bool{
	"blocker": true, "major": true, "minor": true, "nit": true,
}

func (s *server) putPolicy(w http.ResponseWriter, r *http.Request) {
	teamID, ok := parseUintID(w, r)
	if !ok {
		return
	}
	if !s.canManageTeam(r, teamID) {
		writeAPIError(w, http.StatusForbidden, "forbidden")
		return
	}
	raw, ok := decodeRawBody(w, r)
	if !ok {
		return
	}
	confirmed := false
	if value, present := raw["confirm_unhealthy_profile"]; present {
		if err := json.Unmarshal(value, &confirmed); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid confirm_unhealthy_profile")
			return
		}
	}
	policy := store.QAPolicy{TeamID: teamID}
	if value, present := raw["llm_profile_id"]; present {
		if err := json.Unmarshal(value, &policy.LLMProfileID); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid llm_profile_id")
			return
		}
	}
	if value, present := raw["blocking_threshold"]; present {
		if err := json.Unmarshal(value, &policy.BlockingThreshold); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid blocking_threshold")
			return
		}
		if policy.BlockingThreshold != "" && !validBlockingThresholds[policy.BlockingThreshold] {
			writeAPIError(w, http.StatusBadRequest, "invalid blocking_threshold")
			return
		}
	}
	if value, present := raw["extra_instructions"]; present {
		if err := json.Unmarshal(value, &policy.ExtraInstructions); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid extra_instructions")
			return
		}
	}
	if value, present := raw["exclude_globs"]; present {
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
		policy.ExcludeGlobs = encoded
	}
	for _, field := range []struct {
		key string
		dst *int
	}{
		{"max_files", &policy.MaxFilesPerPass},
		{"max_diff_lines", &policy.MaxDiffLinesPerPass},
		{"max_passes", &policy.MaxPasses},
	} {
		if value, present := raw[field.key]; present {
			if err := json.Unmarshal(value, field.dst); err != nil || *field.dst <= 0 {
				writeAPIError(w, http.StatusBadRequest, "invalid "+field.key)
				return
			}
		}
	}

	if !confirmed && policy.LLMProfileID != 0 {
		profile, err := s.Store.LLMProfile(r.Context(), policy.LLMProfileID)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid llm_profile_id")
			return
		}
		if blocks, reason := store.PolicyProfileBlocks(profile); blocks {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":             "unhealthy_profile",
				"profile_id":        profile.ID,
				"reason":            reason,
				"health_status":     profile.HealthStatus,
				"health_checked_at": profile.HealthCheckedAt,
				"hint":              "reenvía con confirm_unhealthy_profile=true para guardar igualmente",
			})
			return
		}
	}

	saved, err := s.Store.SaveQAPolicy(r.Context(), policy)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "save qa policy")
		return
	}
	writeJSON(w, http.StatusOK, qaPolicyDTO(saved))
}
