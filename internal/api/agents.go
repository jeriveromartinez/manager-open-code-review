package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/jeriveromartinez/qa-control-center/internal/config"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/settings"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func llmProfileUpdated(id uint) events.Event {
	return events.Event{Type: "llm_profile.updated", ID: strconv.FormatUint(uint64(id), 10)}
}

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

func derefString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
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
// is twice llm_health_interval: one delayed sweep must not mark all profiles
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

// profileTokenStatsResponse is one profile's consumption in the window. Token
// fields are null when the profile recorded no usage, so the panel can show
// "sin datos" instead of a measured zero.
type profileTokenStatsResponse struct {
	ProfileID  uint   `json:"profile_id"`
	Name       string `json:"name"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	HasData    bool   `json:"has_data"`
	Runs       int    `json:"runs"`
	Checks     int    `json:"checks"`
	Input      *int64 `json:"input_tokens"`
	Output     *int64 `json:"output_tokens"`
	CacheRead  *int64 `json:"cache_read_tokens"`
	CacheWrite *int64 `json:"cache_write_tokens"`
	Total      *int64 `json:"total_tokens"`
	Average    *int64 `json:"average_tokens"`
}

// profileTokenUsageResponse is the admin-only per-profile consumption of
// review runs and health checks over TokenWindow.
type profileTokenUsageResponse struct {
	WindowDays int                         `json:"window_days"`
	HasData    bool                        `json:"has_data"`
	Profiles   []profileTokenStatsResponse `json:"profiles"`
}

func (s *server) llmProfileUsage(w http.ResponseWriter, r *http.Request) {
	stats, err := s.Store.ProfileTokenUsage(r.Context(), time.Now().UTC())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "compute llm profile usage")
		return
	}

	profiles := make([]profileTokenStatsResponse, 0, len(stats))
	hasData := false
	for _, p := range stats {
		entry := profileTokenStatsResponse{
			ProfileID: p.ProfileID, Name: p.Name, Provider: p.Provider, Model: p.Model,
			HasData: p.HasData(), Runs: p.Runs, Checks: p.Checks,
		}
		if p.HasData() {
			hasData = true
			input, output := p.Input, p.Output
			cacheRead, cacheWrite := p.CacheRead, p.CacheWrite
			total := p.Total
			average := total / int64(p.Runs+p.Checks)
			entry.Input, entry.Output = &input, &output
			entry.CacheRead, entry.CacheWrite = &cacheRead, &cacheWrite
			entry.Total, entry.Average = &total, &average
		}
		profiles = append(profiles, entry)
	}

	writeJSON(w, http.StatusOK, profileTokenUsageResponse{
		WindowDays: int(store.TokenWindow / (24 * time.Hour)),
		HasData:    hasData,
		Profiles:   profiles,
	})
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
	s.notify(r.Context(), llmProfileUpdated(created.ID))
	// A new profile is unproven; queue its first check so the panel does not
	// wait for the periodic sweep. A queue failure must not lose the profile.
	if s.Health != nil {
		if err := s.Health.EnqueueLLMHealth(r.Context(), created.ID); err != nil {
			slog.Error("enqueue llm health after create", "profile_id", created.ID, "error", err)
		}
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
	previous := current

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
	s.notify(r.Context(), llmProfileUpdated(id))
	updated, err := s.Store.LLMProfile(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "load llm profile")
		return
	}
	// Any endpoint change invalidates the stored verdict; recheck it now
	// instead of letting the panel show a stale "healthy" until the sweep.
	endpointChanged := updated.Provider != previous.Provider ||
		updated.Model != previous.Model ||
		updated.APIKeyEnc != previous.APIKeyEnc ||
		derefString(updated.BaseURL) != derefString(previous.BaseURL)
	if s.Health != nil && endpointChanged {
		if err := s.Health.EnqueueLLMHealth(r.Context(), id); err != nil {
			slog.Error("enqueue llm health after update", "profile_id", id, "error", err)
		}
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
		s.notify(r.Context(), llmProfileUpdated(id))
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
	s.notify(r.Context(), llmProfileUpdated(id))
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

type qaPolicyResponse struct {
	ID                      uint     `json:"id"`
	TeamID                  uint     `json:"team_id"`
	LLMProfileID            uint     `json:"llm_profile_id"`
	BlockingThreshold       string   `json:"blocking_threshold"`
	ExtraInstructions       string   `json:"extra_instructions"`
	ExcludeGlobs            []string `json:"exclude_globs"`
	MaxFiles                int      `json:"max_files"`
	MaxDiffLines            int      `json:"max_diff_lines"`
	MaxPasses               int      `json:"max_passes"`
	RequireGreenCI          bool     `json:"require_green_ci"`
	ReviewContextCeiling    *int     `json:"review_context_ceiling"`
	GlobalContextCeiling    int      `json:"global_context_ceiling"`
	EffectiveContextCeiling int      `json:"effective_context_ceiling"`
	ReviewEffort            string   `json:"review_effort"`
	ReviewTokenBudget       int64    `json:"review_token_budget"`
	UpdatedAt               string   `json:"updated_at"`
}

// globalContextCeiling returns the global setting as the next start would load
// it. Reading the stored overrides keeps the panel honest right after a save,
// even though this process keeps its startup config until the restart the
// setting asks for.
func (s *server) globalContextCeiling(ctx context.Context) int {
	ceiling := s.Cfg.ReviewContextCeiling
	stored, err := s.Store.Settings(ctx)
	if err == nil {
		next := config.Default()
		if err := settings.Apply(&next, stored); err == nil {
			ceiling = next.ReviewContextCeiling
		}
	}
	if ceiling <= 0 {
		ceiling = config.ReviewContextCeilingDefault
	}
	return ceiling
}

func qaPolicyDTO(p store.QAPolicy, globalCeiling int) qaPolicyResponse {
	var globs []string
	if err := json.Unmarshal(p.ExcludeGlobs, &globs); err != nil {
		globs = []string{}
	}
	if globs == nil {
		globs = []string{}
	}
	effort := p.ReviewEffort
	if effort == "" {
		effort = store.ReviewEffortLow
	}
	effective := globalCeiling
	if p.ReviewContextCeiling != nil {
		effective = *p.ReviewContextCeiling
	}
	return qaPolicyResponse{
		ID: p.ID, TeamID: p.TeamID, LLMProfileID: p.LLMProfileID,
		BlockingThreshold: p.BlockingThreshold, ExtraInstructions: p.ExtraInstructions,
		ExcludeGlobs: globs, MaxFiles: p.MaxFilesPerPass,
		MaxDiffLines: p.MaxDiffLinesPerPass, MaxPasses: p.MaxPasses,
		RequireGreenCI:       p.RequireGreenCI,
		ReviewContextCeiling: p.ReviewContextCeiling,
		GlobalContextCeiling: globalCeiling, EffectiveContextCeiling: effective,
		ReviewEffort: effort, ReviewTokenBudget: p.ReviewTokenBudget,
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
		global := s.globalContextCeiling(r.Context())
		writeJSON(w, http.StatusOK, qaPolicyResponse{
			TeamID: teamID, BlockingThreshold: "major", ExcludeGlobs: []string{},
			MaxFiles: 40, MaxDiffLines: 3000, MaxPasses: 4,
			GlobalContextCeiling: global, EffectiveContextCeiling: global,
			ReviewEffort: store.ReviewEffortLow,
		})
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "load qa policy")
		return
	}
	writeJSON(w, http.StatusOK, qaPolicyDTO(policy, s.globalContextCeiling(r.Context())))
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

	if value, present := raw["require_green_ci"]; present {
		if err := json.Unmarshal(value, &policy.RequireGreenCI); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid require_green_ci")
			return
		}
	}

	// A null or omitted ceiling clears the team override: PUT replaces the whole
	// policy, so inheriting the global value is expressed by leaving it out.
	if value, present := raw["review_context_ceiling"]; present {
		var ceiling *int
		if err := json.Unmarshal(value, &ceiling); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid review_context_ceiling")
			return
		}
		if ceiling != nil &&
			(*ceiling < store.ReviewContextCeilingMin || *ceiling > store.ReviewContextCeilingMax) {
			writeAPIError(w, http.StatusBadRequest, "invalid review_context_ceiling")
			return
		}
		policy.ReviewContextCeiling = ceiling
	}
	if value, present := raw["review_effort"]; present {
		if err := json.Unmarshal(value, &policy.ReviewEffort); err != nil ||
			(policy.ReviewEffort != "" && !store.ValidReviewEffort(policy.ReviewEffort)) {
			writeAPIError(w, http.StatusBadRequest, "invalid review_effort")
			return
		}
	}
	if value, present := raw["review_token_budget"]; present {
		if err := json.Unmarshal(value, &policy.ReviewTokenBudget); err != nil ||
			policy.ReviewTokenBudget < 0 || policy.ReviewTokenBudget > store.ReviewTokenBudgetMax {
			writeAPIError(w, http.StatusBadRequest, "invalid review_token_budget")
			return
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
	s.notify(r.Context(), events.Event{Type: "team.updated", ID: strconv.FormatUint(uint64(teamID), 10)})
	writeJSON(w, http.StatusOK, qaPolicyDTO(saved, s.globalContextCeiling(r.Context())))
}
