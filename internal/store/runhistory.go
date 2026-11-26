package store

import (
	"context"
	"fmt"
	"time"
)

// RunHistoryFilter narrows the finished-run history. Zero values mean "no
// filter"; Limit falls back to the default page size.
type RunHistoryFilter struct {
	BeforeID int64
	Limit    int
	Statuses []string
	RepoID   int64
	Trigger  string
	Query    string
}

// RunHistoryRow is one finished run with the pull request, repository, team and
// profile the history panel needs. StartedAt and FinishedAt stay nil on runs
// that never recorded them.
type RunHistoryRow struct {
	ID            uint       `gorm:"column:id"`
	PullRequestID uint       `gorm:"column:pull_request_id"`
	PRNumber      int        `gorm:"column:pr_number"`
	PRTitle       string     `gorm:"column:pr_title"`
	Repo          string     `gorm:"column:repo"`
	Team          string     `gorm:"column:team"`
	Profile       string     `gorm:"column:profile"`
	Trigger       string     `gorm:"column:trigger"`
	Status        string     `gorm:"column:status"`
	Attempt       int        `gorm:"column:attempt"`
	ErrorCode     string     `gorm:"column:error_code"`
	ErrorMessage  string     `gorm:"column:error_message"`
	StartedAt     *time.Time `gorm:"column:started_at"`
	FinishedAt    *time.Time `gorm:"column:finished_at"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
}

// RunHistoryPage is one page of finished runs plus the cursor for the next
// page. NextBefore is the id of the last returned row, or zero on an empty page.
type RunHistoryPage struct {
	Runs       []RunHistoryRow
	NextBefore int64
	HasMore    bool
}

const (
	defaultRunHistoryLimit = 50
	maxRunHistoryLimit     = 200
)

// terminalRunStatuses are the statuses a run can no longer leave. Queued and
// running runs belong to the live view instead.
var terminalRunStatuses = []string{RunSucceeded, RunInconclusive, RunFailed, RunSuperseded}

// ReviewRunHistory lists the finished runs the user may see, newest first. It
// keeps the same visibility scope as ListActiveReviewRuns, so the two views
// cannot disagree, but it does include runs of closed pull requests: closed
// history stays relevant. A cursor on the run id keeps pages stable even when
// several runs share a creation time.
func (s *Store) ReviewRunHistory(ctx context.Context, u User, filter RunHistoryFilter) (RunHistoryPage, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultRunHistoryLimit
	}
	if limit > maxRunHistoryLimit {
		limit = maxRunHistoryLimit
	}

	visible := s.teamScopedPRs(ctx, u).Select("pr.id")
	q := s.DB.WithContext(ctx).
		Table("review_runs AS rr").
		Select(`rr.id, rr.pull_request_id, pr.number AS pr_number, pr.title AS pr_title,
			r.full_name AS repo, COALESCE(t.name, '') AS team,
			COALESCE(p.name, '') AS profile,
			rr.trigger, rr.status, rr.attempt, rr.error_code, rr.error_message,
			rr.started_at, rr.finished_at, rr.created_at`).
		Joins("JOIN pull_requests pr ON pr.id = rr.pull_request_id").
		Joins("JOIN repositories r ON r.id = pr.repository_id").
		Joins("LEFT JOIN teams t ON t.id = r.team_id").
		Joins("LEFT JOIN llm_profiles p ON p.id = rr.profile_id").
		Where("rr.pull_request_id IN (?)", visible).
		Where("rr.status IN ?", terminalRunStatuses)

	if filter.BeforeID > 0 {
		q = q.Where("rr.id < ?", filter.BeforeID)
	}
	if len(filter.Statuses) > 0 {
		q = q.Where("rr.status IN ?", filter.Statuses)
	}
	if filter.RepoID > 0 {
		q = q.Where("pr.repository_id = ?", filter.RepoID)
	}
	if filter.Trigger != "" {
		q = q.Where("rr.trigger = ?", filter.Trigger)
	}
	if filter.Query != "" {
		like := "%" + filter.Query + "%"
		q = q.Where("(pr.title ILIKE ? OR r.full_name ILIKE ?)", like, like)
	}

	var rows []RunHistoryRow
	err := q.Order("rr.created_at DESC, rr.id DESC").Limit(limit + 1).Scan(&rows).Error
	if err != nil {
		return RunHistoryPage{}, fmt.Errorf("list review run history: %w", err)
	}

	page := RunHistoryPage{Runs: rows}
	if len(page.Runs) > limit {
		page.Runs = page.Runs[:limit]
		page.HasMore = true
	}
	if len(page.Runs) > 0 {
		page.NextBefore = int64(page.Runs[len(page.Runs)-1].ID)
	}
	return page, nil
}
