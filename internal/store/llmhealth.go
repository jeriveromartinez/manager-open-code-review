package store

import (
	"context"
	"fmt"
	"time"
)

// StaleLLMProfiles returns the profiles that need a health check: enabled ones
// used by at least one QA policy whose last check is older than the given age,
// plus the ones never checked. A profile in error is also returned, so a
// recovered provider is noticed without an admin pressing the button. Profiles
// no policy references are skipped: checking them only spends quota.
func (s *Store) StaleLLMProfiles(ctx context.Context, olderThan time.Time) ([]LLMProfile, error) {
	const query = `
SELECT p.* FROM llm_profiles p
WHERE p.enabled = true
  AND (p.health_checked_at IS NULL OR p.health_checked_at < ?)
  AND EXISTS (SELECT 1 FROM qa_policies q WHERE q.llm_profile_id = p.id)
ORDER BY p.health_checked_at NULLS FIRST, p.id`

	var profiles []LLMProfile
	if err := s.DB.WithContext(ctx).Raw(query, olderThan).Scan(&profiles).Error; err != nil {
		return nil, fmt.Errorf("list stale llm profiles: %w", err)
	}
	return profiles, nil
}

// LLMProfileHealth describes how old a profile's health verdict is. The panel
// shows it so a check that stopped running is visible instead of looking like a
// profile that is healthy today.
type LLMProfileHealth struct {
	ProfileID    uint       `json:"profile_id"`
	Status       string     `json:"status"`
	CheckedAt    *time.Time `json:"checked_at"`
	AgeSeconds   int        `json:"age_seconds"`
	StaleAfter   int        `json:"stale_after_seconds"`
	Stale        bool       `json:"stale"`
	ErrorMessage string     `json:"error_message"`
}

// ProfileHealthSince computes the age of each profile's verdict. A profile
// never checked has no age and counts as stale.
func ProfileHealthSince(profiles []LLMProfile, now time.Time, staleAfter time.Duration) []LLMProfileHealth {
	out := make([]LLMProfileHealth, 0, len(profiles))
	for _, profile := range profiles {
		health := LLMProfileHealth{
			ProfileID:    profile.ID,
			Status:       profile.HealthStatus,
			CheckedAt:    profile.HealthCheckedAt,
			StaleAfter:   int(staleAfter.Seconds()),
			ErrorMessage: profile.HealthError,
		}
		if profile.HealthCheckedAt == nil {
			health.Stale = true
			out = append(out, health)
			continue
		}
		health.AgeSeconds = int(now.Sub(*profile.HealthCheckedAt).Seconds())
		if health.AgeSeconds < 0 {
			health.AgeSeconds = 0
		}
		health.Stale = health.AgeSeconds > health.StaleAfter
		out = append(out, health)
	}
	return out
}

// PolicyProfileBlocks reports whether assigning the profile to a policy is a
// problem worth confirming. A disabled profile, a profile in error and one that
// was never checked all block by default: reviewing with them either cannot
// work or has never been proven to work.
func PolicyProfileBlocks(profile LLMProfile) (bool, string) {
	if !profile.Enabled {
		return true, "el perfil está deshabilitado"
	}
	switch profile.HealthStatus {
	case HealthError:
		return true, "la última comprobación del perfil falló"
	case HealthOK:
		return false, ""
	default:
		return true, "el perfil no tiene una comprobación de salud"
	}
}
