-- +goose Up
-- Durable publication intent (#138): the terminal transaction records that the
-- run still has artefacts to deliver, so a crash between finishing the review
-- and enqueueing the publish job cannot lose the delivery. The state JSON keeps
-- the steps that already succeeded, so a retry resumes only the missing ones
-- and a redelivery of a settled run becomes a no-op.
ALTER TABLE review_runs ADD COLUMN publish_pending boolean NOT NULL DEFAULT false;
ALTER TABLE review_runs ADD COLUMN publish_state jsonb;
CREATE INDEX idx_review_runs_publish_pending ON review_runs (id) WHERE publish_pending;

-- +goose Down
DROP INDEX IF EXISTS idx_review_runs_publish_pending;
ALTER TABLE review_runs DROP COLUMN publish_state, DROP COLUMN publish_pending;
