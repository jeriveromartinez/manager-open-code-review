package store

import (
	"context"
	"fmt"
	"time"
)

// ActiveRunView is one queued or running run with the pull request, repository,
// team and profile the panel needs. It is read-only and shaped for the live
// reviews view (#123).
type ActiveRunView struct {
	ID             uint       `gorm:"column:id"`
	PullRequestID  uint       `gorm:"column:pull_request_id"`
	PRNumber       int        `gorm:"column:pr_number"`
	PRTitle        string     `gorm:"column:pr_title"`
	Repo           string     `gorm:"column:repo"`
	Team           string     `gorm:"column:team"`
	Profile        string     `gorm:"column:profile"`
	Trigger        string     `gorm:"column:trigger"`
	Status         string     `gorm:"column:status"`
	Attempt        int        `gorm:"column:attempt"`
	Phase          string     `gorm:"column:phase"`
	PhaseStartedAt *time.Time `gorm:"column:phase_started_at"`
	StartedAt      *time.Time `gorm:"column:started_at"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
}

// ListActiveReviewRuns returns the queued and running runs the user may see,
// running first and then oldest first. Visibility reuses teamScopedPRs, the
// same scope as the dashboard, so both views cannot disagree. A run whose pull
// request is no longer open is left out: it will be superseded, not reviewed.
func (s *Store) ListActiveReviewRuns(ctx context.Context, u User) ([]ActiveRunView, error) {
	visible := s.teamScopedPRs(ctx, u).Select("pr.id")
	var runs []ActiveRunView
	err := s.DB.WithContext(ctx).
		Table("review_runs AS rr").
		Select(`rr.id, rr.pull_request_id, pr.number AS pr_number, pr.title AS pr_title,
			r.full_name AS repo, COALESCE(t.name, '') AS team,
			COALESCE(p.name, '') AS profile,
			rr.trigger, rr.status, rr.attempt, rr.phase, rr.phase_started_at,
			rr.started_at, rr.created_at`).
		Joins("JOIN pull_requests pr ON pr.id = rr.pull_request_id").
		Joins("JOIN repositories r ON r.id = pr.repository_id").
		Joins("LEFT JOIN teams t ON t.id = r.team_id").
		Joins("LEFT JOIN llm_profiles p ON p.id = rr.profile_id").
		Where("pr.state = ?", stateOpen).
		Where("rr.status IN ?", []string{RunQueued, RunRunning}).
		Where("rr.pull_request_id IN (?)", visible).
		Order("CASE rr.status WHEN 'running' THEN 0 ELSE 1 END, COALESCE(rr.started_at, rr.created_at), rr.id").
		Scan(&runs).Error
	if err != nil {
		return nil, fmt.Errorf("list active review runs: %w", err)
	}
	return runs, nil
}
