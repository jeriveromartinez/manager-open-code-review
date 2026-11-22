-- +goose Up
-- GitHub installation tokens live one hour. A run can wait longer in the queue,
-- so the control process renews the token of queued runs before it expires (#68).
ALTER TABLE review_runs ADD COLUMN clone_token_expires_at timestamptz;
-- The refresh sweep only looks at queued runs, so the partial index keeps it
-- from scanning the finished ones.
CREATE INDEX idx_review_runs_queued_token_expiry
  ON review_runs (clone_token_expires_at) WHERE status = 'queued';

-- +goose Down
DROP INDEX IF EXISTS idx_review_runs_queued_token_expiry;
ALTER TABLE review_runs DROP COLUMN clone_token_expires_at;
