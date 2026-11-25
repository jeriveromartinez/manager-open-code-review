package store

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// publishLockNamespace is a two-int advisory-lock namespace distinct from the
// profile slots and the pull request lock, so a publication never collides with
// them. The second key is the pull request id: two runs of one pull request
// share the summary, check and status, so serializing per pull request keeps
// them from racing each other.
const publishLockNamespace = 138

// LockPublishPR serializes the delivery of every run of one pull request across
// processes. Redeliveries, the reconciler and two different runs of the same PR
// can reach publication at once; the lock keeps them from racing on the shared
// summary, check or thread state. It is safe to call unlock more than once.
func (s *Store) LockPublishPR(ctx context.Context, prID uint) (unlock func(), err error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection for pull request %d publish lock: %w", prID, err)
	}

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1, $2)", publishLockNamespace, int64(prID)); err != nil {
		conn.Release()
		return nil, fmt.Errorf("lock publication of pull request %d: %w", prID, err)
	}

	var once sync.Once
	unlock = func() {
		once.Do(func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// Closing the connection releases the session lock even when the
			// explicit unlock fails, so no error can leak into the func() API.
			_, _ = conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1, $2)", publishLockNamespace, int64(prID))
			conn.Release()
		})
	}
	return unlock, nil
}

// PendingPublishRuns lists terminal runs whose GitHub delivery has not been
// settled, oldest first. The durable intent makes them recoverable when the
// worker finished a run but crashed before enqueueing its publish job.
func (s *Store) PendingPublishRuns(ctx context.Context, limit int) ([]ReviewRun, error) {
	var runs []ReviewRun
	err := s.DB.WithContext(ctx).
		Where("publish_pending = ? AND status IN ?", true,
			[]string{RunSucceeded, RunInconclusive, RunFailed, RunSuperseded}).
		Order("id").Limit(limit).Find(&runs).Error
	if err != nil {
		return nil, fmt.Errorf("list review runs pending publication: %w", err)
	}
	return runs, nil
}

// MarkPublishStep records that one delivery step of the run already succeeded.
// A retried publish reads the state and skips it instead of writing the same
// summary, check or thread again.
func (s *Store) MarkPublishStep(ctx context.Context, id uint, step string) error {
	res := s.DB.WithContext(ctx).Exec(`
UPDATE review_runs
SET publish_state = COALESCE(publish_state, '{}'::jsonb) || jsonb_build_object(CAST(? AS text), true),
    updated_at = ?
WHERE id = ?`, step, time.Now().UTC(), id)
	if res.Error != nil {
		return fmt.Errorf("mark publish step %q of review run %d: %w", step, id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("mark publish step %q of review run %d: %w", step, id, ErrNotFound)
	}
	return nil
}

// AdvancePublishCursor records how much of a multi-request effect GitHub has
// already accepted. Annotations are appended per request, so a retry resumes
// after this durable count instead of resending a page that was accepted
// before the response was lost (#138).
func (s *Store) AdvancePublishCursor(ctx context.Context, id uint, cursor string, accepted int) error {
	res := s.DB.WithContext(ctx).Exec(`
UPDATE review_runs
SET publish_state = COALESCE(publish_state, '{}'::jsonb) || jsonb_build_object(CAST(? AS text), CAST(? AS int)),
    updated_at = ?
WHERE id = ?`, cursor, accepted, time.Now().UTC(), id)
	if res.Error != nil {
		return fmt.Errorf("advance publish cursor %q of review run %d: %w", cursor, id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("advance publish cursor %q of review run %d: %w", cursor, id, ErrNotFound)
	}
	return nil
}

// CompletePublish settles the run's delivery: the pending intent is cleared and
// the state records that every owed step was delivered or deliberately skipped.
// A redelivery after this point is a no-op.
func (s *Store) CompletePublish(ctx context.Context, id uint) error {
	res := s.DB.WithContext(ctx).Exec(`
UPDATE review_runs
SET publish_pending = false,
    publish_state = COALESCE(publish_state, '{}'::jsonb) || '{"completed":true}'::jsonb,
    updated_at = ?
WHERE id = ?`, time.Now().UTC(), id)
	if res.Error != nil {
		return fmt.Errorf("complete publication of review run %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("complete publication of review run %d: %w", id, ErrNotFound)
	}
	return nil
}
