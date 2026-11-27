-- +goose Up
-- Ownership of a running attempt (#228). The worker that claims a run records
-- a lease; a run whose lease expires is recovered by the next execution instead
-- of waiting for the global stale threshold. The previous owner can no longer
-- write or complete the result because every mutation is checked against it.
ALTER TABLE review_runs ADD COLUMN lease_owner text NOT NULL DEFAULT '';
-- How many times an orphaned attempt was recovered. It bounds the retries: a
-- run that keeps losing its worker ends in a terminal state.
ALTER TABLE review_runs ADD COLUMN recovery_count integer NOT NULL DEFAULT 0;

-- The watchdog scans running runs by lease age. Partial on purpose: terminal
-- and queued rows are never part of the recovery sweep.
CREATE INDEX idx_review_runs_running_lease
    ON review_runs (heartbeat_at) WHERE status = 'running';

-- +goose Down
DROP INDEX IF EXISTS idx_review_runs_running_lease;
ALTER TABLE review_runs DROP COLUMN IF EXISTS recovery_count;
ALTER TABLE review_runs DROP COLUMN IF EXISTS lease_owner;
