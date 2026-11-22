package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ReviewLogRetention is how long the per-run review logs stay in the database.
// The worker prunes older rows periodically; truncation happens earlier when a
// single run exceeds its size cap.
const ReviewLogRetention = 30 * 24 * time.Hour

// AppendReviewRunLog inserts log lines. Duplicates are ignored so a retried
// flush cannot fail the review.
func (s *Store) AppendReviewRunLog(ctx context.Context, entries []ReviewRunLog) error {
	if len(entries) == 0 {
		return nil
	}
	err := s.DB.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		CreateInBatches(entries, 50).Error
	if err != nil {
		return fmt.Errorf("append review run log: %w", err)
	}
	return nil
}

// ReviewRunLogs returns the lines of a run after afterSeq, oldest first. The
// limit keeps the endpoint bounded; callers page with the last seq they saw.
func (s *Store) ReviewRunLogs(ctx context.Context, runID uint, afterSeq int64, limit int) ([]ReviewRunLog, error) {
	if limit <= 0 || limit > 2000 {
		limit = 2000
	}
	var logs []ReviewRunLog
	err := s.DB.WithContext(ctx).
		Where("run_id = ? AND seq > ?", runID, afterSeq).
		Order("seq").
		Limit(limit).
		Find(&logs).Error
	if err != nil {
		return nil, fmt.Errorf("list review run %d logs: %w", runID, err)
	}
	return logs, nil
}

// PruneReviewRunLogs deletes lines older than before and returns how many rows
// were removed.
func (s *Store) PruneReviewRunLogs(ctx context.Context, before time.Time) (int64, error) {
	res := s.DB.WithContext(ctx).Where("ts < ?", before.UTC()).Delete(&ReviewRunLog{})
	if res.Error != nil {
		return 0, fmt.Errorf("prune review run logs: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// ReviewRunPRID returns the pull request a run belongs to, so the API can apply
// the same visibility scope as the PR detail before exposing its log.
func (s *Store) ReviewRunPRID(ctx context.Context, runID uint) (uint, error) {
	var run ReviewRun
	err := s.DB.WithContext(ctx).Select("pull_request_id").Where("id = ?", runID).First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("look up review run %d: %w", runID, err)
	}
	return run.PullRequestID, nil
}
