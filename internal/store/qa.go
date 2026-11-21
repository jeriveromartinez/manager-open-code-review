package store

import (
	"context"
	"fmt"
	"time"
)

const (
	// PrecisionWindow is how far back triage decisions count toward the AI
	// precision shown to QA: long enough to smooth a slow day, short enough to
	// show the effect of a policy change within a week.
	PrecisionWindow = 7 * 24 * time.Hour
	// CategoryWindow is how far back confirmed findings count toward the
	// recurring categories.
	CategoryWindow = 30 * 24 * time.Hour
	topCategories  = 5
)

// QASummary is the QA engineer's overview of the pull requests they can see.
// Confirmed and FalsePositive count panel verdicts inside PrecisionWindow,
// whatever the finding's status is now; Categories are the most frequent
// categories of confirmed findings inside CategoryWindow, most frequent first.
type QASummary struct {
	ReadyForHuman int
	Inconclusive  int
	RunsRunning   int
	RunsQueued    int
	Untriaged     SeverityCounts
	Confirmed     int
	FalsePositive int
	Categories    []CategoryCount
}

// CategoryCount is how many confirmed findings share a category.
type CategoryCount struct {
	Category string
	Count    int
}

// QASummary computes the QA overview at now. Queue figures use the open pull
// requests of ListVisiblePullRequests; triage history also includes pull
// requests merged or closed since, so shipping a PR does not erase its triage.
func (s *Store) QASummary(ctx context.Context, u User, now time.Time) (QASummary, error) {
	var out QASummary

	var statusRows []struct {
		QAStatus string
		N        int
	}
	err := s.visiblePRs(ctx, u, PRFilter{}).
		Where("pr.qa_status IN ?", []string{StatusReadyForHuman, StatusInconclusive}).
		Select("pr.qa_status AS qa_status, COUNT(*) AS n").
		Group("pr.qa_status").
		Scan(&statusRows).Error
	if err != nil {
		return QASummary{}, fmt.Errorf("count QA queue: %w", err)
	}
	for _, row := range statusRows {
		switch row.QAStatus {
		case StatusReadyForHuman:
			out.ReadyForHuman = row.N
		case StatusInconclusive:
			out.Inconclusive = row.N
		}
	}

	openIDs := s.visiblePRs(ctx, u, PRFilter{}).Select("pr.id")

	var runRows []struct {
		Status string
		N      int
	}
	err = s.DB.WithContext(ctx).Table("review_runs").
		Where("status IN ? AND pull_request_id IN (?)", []string{RunQueued, RunRunning}, openIDs).
		Select("status, COUNT(*) AS n").
		Group("status").
		Scan(&runRows).Error
	if err != nil {
		return QASummary{}, fmt.Errorf("count active review runs: %w", err)
	}
	for _, row := range runRows {
		switch row.Status {
		case RunRunning:
			out.RunsRunning = row.N
		case RunQueued:
			out.RunsQueued = row.N
		}
	}

	var severityRows []struct {
		Severity string
		N        int
	}
	err = s.DB.WithContext(ctx).Table("findings").
		Where("status = ? AND triage_verdict = '' AND pull_request_id IN (?)", FindingOpen, openIDs).
		Select("severity, COUNT(*) AS n").
		Group("severity").
		Scan(&severityRows).Error
	if err != nil {
		return QASummary{}, fmt.Errorf("count untriaged findings: %w", err)
	}
	for _, row := range severityRows {
		switch row.Severity {
		case "blocker":
			out.Untriaged.Blocker = row.N
		case "major":
			out.Untriaged.Major = row.N
		case "minor":
			out.Untriaged.Minor = row.N
		case "nit":
			out.Untriaged.Nit = row.N
		}
	}

	scopedIDs := s.teamScopedPRs(ctx, u).Select("pr.id")

	var precision struct {
		Confirmed     int
		FalsePositive int
	}
	err = s.DB.WithContext(ctx).Table("findings").
		Where("triaged_at >= ? AND pull_request_id IN (?)", now.Add(-PrecisionWindow), scopedIDs).
		Select("COUNT(*) FILTER (WHERE triage_verdict = ?) AS confirmed, COUNT(*) FILTER (WHERE triage_verdict = ?) AS false_positive",
			VerdictConfirmed, VerdictFalsePositive).
		Scan(&precision).Error
	if err != nil {
		return QASummary{}, fmt.Errorf("count triage decisions: %w", err)
	}
	out.Confirmed = precision.Confirmed
	out.FalsePositive = precision.FalsePositive

	err = s.DB.WithContext(ctx).Table("findings").
		Where("triaged_at >= ? AND triage_verdict = ? AND category <> '' AND pull_request_id IN (?)",
			now.Add(-CategoryWindow), VerdictConfirmed, scopedIDs).
		Select("category, COUNT(*) AS count").
		Group("category").
		Order("count DESC, category").
		Limit(topCategories).
		Scan(&out.Categories).Error
	if err != nil {
		return QASummary{}, fmt.Errorf("count confirmed categories: %w", err)
	}
	return out, nil
}
