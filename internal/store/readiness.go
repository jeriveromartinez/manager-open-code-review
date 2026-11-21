package store

import (
	"context"
	"fmt"
	"time"
)

// ReadinessReport counts the configuration links the review engine still needs.
// NoTeams and NoProfiles are zero-state flags: the counters alone cannot tell
// "every team has a policy" from "there are no teams at all".
type ReadinessReport struct {
	ReposWithoutTeam   int
	TeamsWithoutPolicy int
	UnhealthyProfiles  int
	NoTeams            bool
	NoProfiles         bool
}

// ReadinessReport aggregates the gaps that leave a synced installation without
// reviews. staleAfter is the age at which an enabled profile's health verdict
// no longer counts as current.
func (s *Store) ReadinessReport(ctx context.Context, staleAfter time.Duration) (ReadinessReport, error) {
	db := s.DB.WithContext(ctx)
	var report ReadinessReport

	var reposWithoutTeam int64
	if err := db.Model(&Repository{}).
		Where("enabled = ? AND team_id IS NULL", true).
		Count(&reposWithoutTeam).Error; err != nil {
		return ReadinessReport{}, fmt.Errorf("count repositories without team: %w", err)
	}
	report.ReposWithoutTeam = int(reposWithoutTeam)

	var teamsWithoutPolicy int64
	if err := db.Model(&Team{}).
		Where("NOT EXISTS (SELECT 1 FROM qa_policies WHERE qa_policies.team_id = teams.id)").
		Count(&teamsWithoutPolicy).Error; err != nil {
		return ReadinessReport{}, fmt.Errorf("count teams without policy: %w", err)
	}
	report.TeamsWithoutPolicy = int(teamsWithoutPolicy)

	cutoff := time.Now().UTC().Add(-staleAfter)
	var unhealthyProfiles int64
	if err := db.Model(&LLMProfile{}).
		Where("enabled = ?", true).
		Where("health_status <> ? OR health_checked_at IS NULL OR health_checked_at < ?", HealthOK, cutoff).
		Count(&unhealthyProfiles).Error; err != nil {
		return ReadinessReport{}, fmt.Errorf("count unhealthy llm profiles: %w", err)
	}
	report.UnhealthyProfiles = int(unhealthyProfiles)

	var teams int64
	if err := db.Model(&Team{}).Count(&teams).Error; err != nil {
		return ReadinessReport{}, fmt.Errorf("count teams: %w", err)
	}
	report.NoTeams = teams == 0

	var profiles int64
	if err := db.Model(&LLMProfile{}).Count(&profiles).Error; err != nil {
		return ReadinessReport{}, fmt.Errorf("count llm profiles: %w", err)
	}
	report.NoProfiles = profiles == 0

	return report, nil
}
