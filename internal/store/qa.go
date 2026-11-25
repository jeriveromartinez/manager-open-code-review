package store

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
)

const (
	// PrecisionWindow is how far back triage decisions count toward the AI
	// precision shown to QA: long enough to smooth a slow day, short enough to
	// show the effect of a policy change within a week.
	PrecisionWindow = 7 * 24 * time.Hour
	// CategoryWindow is how far back confirmed findings count toward the
	// recurring categories.
	CategoryWindow = 30 * 24 * time.Hour
	// TokenWindow is how far back finished review runs count toward the token
	// consumption tile.
	TokenWindow   = 7 * 24 * time.Hour
	topCategories = 5
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
	// AutoConfirmed and AutoDismissed count the findings the verifier decided,
	// whatever their age and whatever became of them since: the audit figure
	// for how much OCR resolved without a human.
	AutoConfirmed int
	AutoDismissed int
	// Uncertain counts the open findings without a verdict in the queue that
	// waits for a person: what is still left to review.
	Uncertain  int
	Categories []CategoryCount
	// ConfirmedByRepo counts confirmed findings per repository inside
	// CategoryWindow, most first: where a lead's team keeps shipping defects.
	ConfirmedByRepo []RepoCount
	// Tokens aggregates the runs finished inside TokenWindow.
	Tokens RunTokenStats
}

// RunTokenStats is the token consumption of the review runs finished inside
// TokenWindow. Reviews counts only runs that recorded usage, so a run created
// before the accounting existed or a skipped/failed native run with zero tokens
// does not dilute the average. MaxContextKnown is false when no run reported a
// max-context sample; MaxContext is then the wire default, not a measured
// ceiling, and consumers must not average it.
type RunTokenStats struct {
	Reviews         int
	Input           int64
	Output          int64
	CacheRead       int64
	CacheWrite      int64
	Total           int64
	MaxContext      int64
	MaxContextKnown bool
}

// CategoryCount is how many confirmed findings share a category.
type CategoryCount struct {
	Category string
	Count    int
}

// RepoCount is how many confirmed findings a repository has.
type RepoCount struct {
	Repo  string
	Count int
}

// QAFilter narrows the summary to one team. A team the user cannot see yields
// an empty summary rather than an error, the same as an empty team.
type QAFilter struct {
	TeamID *uint
}

// QASummary computes the QA overview at now. Queue figures use the open pull
// requests of ListVisiblePullRequests; triage history also includes pull
// requests merged or closed since, so shipping a PR does not erase its triage.
func (s *Store) QASummary(ctx context.Context, u User, now time.Time, f QAFilter) (QASummary, error) {
	var out QASummary

	narrow := func(q *gorm.DB) *gorm.DB {
		if f.TeamID != nil {
			q = q.Where("r.team_id = ?", *f.TeamID)
		}
		return q
	}
	openPRs := func() *gorm.DB { return narrow(s.visiblePRs(ctx, u, PRFilter{})) }

	var statusRows []struct {
		QAStatus string
		N        int
	}
	err := openPRs().
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

	openIDs := openPRs().Select("pr.id")

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

	readyIDs := openPRs().Where("pr.qa_status = ?", StatusReadyForHuman).Select("pr.id")
	var uncertain int64
	err = s.DB.WithContext(ctx).Table("findings").
		Where("status = ? AND triage_verdict = '' AND pull_request_id IN (?)", FindingOpen, readyIDs).
		Count(&uncertain).Error
	if err != nil {
		return QASummary{}, fmt.Errorf("count findings without a verdict: %w", err)
	}
	out.Uncertain = int(uncertain)

	scopedIDs := narrow(s.teamScopedPRs(ctx, u)).Select("pr.id")

	var auto struct {
		Confirmed int
		Dismissed int
	}
	err = s.DB.WithContext(ctx).Table("findings").
		Where("triage_source = ? AND triage_verdict IN ? AND pull_request_id IN (?)",
			TriageSourceVerifier, []string{VerdictConfirmed, VerdictFalsePositive}, scopedIDs).
		Select("COUNT(*) FILTER (WHERE triage_verdict = ?) AS confirmed, COUNT(*) FILTER (WHERE triage_verdict = ?) AS dismissed",
			VerdictConfirmed, VerdictFalsePositive).
		Scan(&auto).Error
	if err != nil {
		return QASummary{}, fmt.Errorf("count auto triage: %w", err)
	}
	out.AutoConfirmed = auto.Confirmed
	out.AutoDismissed = auto.Dismissed

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

	err = s.DB.WithContext(ctx).Table("findings f").
		Joins("JOIN pull_requests pr ON pr.id = f.pull_request_id").
		Joins("JOIN repositories r ON r.id = pr.repository_id").
		Where("f.triaged_at >= ? AND f.triage_verdict = ? AND f.pull_request_id IN (?)",
			now.Add(-CategoryWindow), VerdictConfirmed, scopedIDs).
		Select("r.full_name AS repo, COUNT(*) AS count").
		Group("r.id, r.full_name").
		Order("count DESC, repo").
		Scan(&out.ConfirmedByRepo).Error
	if err != nil {
		return QASummary{}, fmt.Errorf("count confirmed findings by repository: %w", err)
	}

	// Runs of merged pull requests still consumed tokens, so this uses the
	// broader history scope and not the open queue.
	var tokens struct {
		Reviews    int
		Input      int64
		Output     int64
		CacheRead  int64
		CacheWrite int64
		Total      int64
		MaxContext *int64
	}
	err = s.DB.WithContext(ctx).Table("review_runs rr").
		// A zero total is not a measured run: skipped or failed native runs and
		// legacy rows without usage must not raise Reviews or lower Average.
		Where("rr.finished_at >= ? AND rr.total_tokens > 0 AND rr.pull_request_id IN (?)",
			now.Add(-TokenWindow), scopedIDs).
		Select(`COUNT(*) AS reviews,
			COALESCE(SUM(rr.input_tokens), 0) AS input,
			COALESCE(SUM(rr.output_tokens), 0) AS output,
			COALESCE(SUM(rr.cache_read_tokens), 0) AS cache_read,
			COALESCE(SUM(rr.cache_write_tokens), 0) AS cache_write,
			COALESCE(SUM(rr.total_tokens), 0) AS total,
			MAX(rr.max_context_tokens) AS max_context`).
		Scan(&tokens).Error
	if err != nil {
		return QASummary{}, fmt.Errorf("count review token usage: %w", err)
	}
	out.Tokens = RunTokenStats{
		Reviews:    tokens.Reviews,
		Input:      tokens.Input,
		Output:     tokens.Output,
		CacheRead:  tokens.CacheRead,
		CacheWrite: tokens.CacheWrite,
		Total:      tokens.Total,
	}
	if tokens.MaxContext != nil {
		out.Tokens.MaxContext = *tokens.MaxContext
		out.Tokens.MaxContextKnown = true
	}
	return out, nil
}

// ProfileTokenStats is the token consumption attributed to one LLM profile
// inside TokenWindow: review runs plus health checks. Runs and Checks count
// only the executions that recorded usage, so every profile with HasData has
// a meaningful total. Tokens stay zero when HasData is false, which the panel
// must render as "sin datos" instead of a real zero.
type ProfileTokenStats struct {
	ProfileID  uint
	Name       string
	Provider   string
	Model      string
	Runs       int
	Checks     int
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	Total      int64
}

// HasData reports whether at least one run or check recorded usage.
func (p ProfileTokenStats) HasData() bool { return p.Runs+p.Checks > 0 }

// ProfileTokenUsage aggregates the token consumption of every LLM profile over
// TokenWindow: review runs grouped by profile_id plus the profile's health
// checks. Profiles with no recorded usage are still returned with HasData
// false, so the panel can distinguish "nothing yet" from a measured zero.
func (s *Store) ProfileTokenUsage(ctx context.Context, now time.Time) ([]ProfileTokenStats, error) {
	out := []ProfileTokenStats{}
	err := s.DB.WithContext(ctx).Table("llm_profiles").
		Select("id AS profile_id, name, provider, model").
		Order("id").
		Scan(&out).Error
	if err != nil {
		return nil, fmt.Errorf("list profiles for token usage: %w", err)
	}

	type tokenRow struct {
		ProfileID  uint
		N          int
		Input      int64
		Output     int64
		CacheRead  int64
		CacheWrite int64
		Total      int64
	}
	since := now.Add(-TokenWindow)

	var runRows []tokenRow
	err = s.DB.WithContext(ctx).Table("review_runs").
		Where("finished_at >= ? AND total_tokens IS NOT NULL", since).
		// Runs with at least one usage-bearing attempt are attributed per attempt
		// below; counting them here as well would double their consumption. A run
		// whose attempts never reported tokens stays on the run row, so an
		// interrupted attempt does not erase known run usage.
		Where(`NOT EXISTS (
			SELECT 1 FROM review_attempts a
			WHERE a.run_id = review_runs.id AND a.total_tokens IS NOT NULL
		)`).
		Select(`profile_id, COUNT(*) AS n,
			COALESCE(SUM(input_tokens), 0) AS input,
			COALESCE(SUM(output_tokens), 0) AS output,
			COALESCE(SUM(cache_read_tokens), 0) AS cache_read,
			COALESCE(SUM(cache_write_tokens), 0) AS cache_write,
			COALESCE(SUM(total_tokens), 0) AS total`).
		Group("profile_id").
		Scan(&runRows).Error
	if err != nil {
		return nil, fmt.Errorf("count review tokens by profile: %w", err)
	}

	// When a fallback ran, each attempt carries its own destination and usage.
	// The run itself is excluded above, so a partial pass and the retry are
	// credited once each and never to the enqueued profile.
	var attemptRows []tokenRow
	err = s.DB.WithContext(ctx).Table("review_attempts").
		Where("finished_at >= ? AND total_tokens IS NOT NULL", since).
		Select(`profile_id, COUNT(DISTINCT run_id) AS n,
			COALESCE(SUM(input_tokens), 0) AS input,
			COALESCE(SUM(output_tokens), 0) AS output,
			COALESCE(SUM(cache_read_tokens), 0) AS cache_read,
			COALESCE(SUM(cache_write_tokens), 0) AS cache_write,
			COALESCE(SUM(total_tokens), 0) AS total`).
		Group("profile_id").
		Scan(&attemptRows).Error
	if err != nil {
		return nil, fmt.Errorf("count review attempt tokens by profile: %w", err)
	}

	var checkRows []tokenRow
	err = s.DB.WithContext(ctx).Table("llm_health_checks").
		Where("checked_at >= ? AND total_tokens IS NOT NULL", since).
		Select(`profile_id, COUNT(*) AS n,
			COALESCE(SUM(input_tokens), 0) AS input,
			COALESCE(SUM(output_tokens), 0) AS output,
			COALESCE(SUM(cache_read_tokens), 0) AS cache_read,
			COALESCE(SUM(cache_write_tokens), 0) AS cache_write,
			COALESCE(SUM(total_tokens), 0) AS total`).
		Group("profile_id").
		Scan(&checkRows).Error
	if err != nil {
		return nil, fmt.Errorf("count health-check tokens by profile: %w", err)
	}

	byProfile := make(map[uint]*ProfileTokenStats, len(out))
	for i := range out {
		byProfile[out[i].ProfileID] = &out[i]
	}
	add := func(rows []tokenRow, runs bool) {
		for _, row := range rows {
			stats := byProfile[row.ProfileID]
			if stats == nil {
				continue
			}
			if runs {
				stats.Runs += row.N
			} else {
				stats.Checks = row.N
			}
			stats.Input += row.Input
			stats.Output += row.Output
			stats.CacheRead += row.CacheRead
			stats.CacheWrite += row.CacheWrite
			stats.Total += row.Total
		}
	}
	add(runRows, true)
	add(attemptRows, true)
	add(checkRows, false)
	return out, nil
}
