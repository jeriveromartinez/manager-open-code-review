-- +goose Up
-- Last sign of life of the worker executing a run (#130). The watchdog uses it
-- to tell a vanished worker from a review that overran its maximum time.
ALTER TABLE review_runs ADD COLUMN heartbeat_at timestamptz;

-- +goose Down
ALTER TABLE review_runs DROP COLUMN IF EXISTS heartbeat_at;
