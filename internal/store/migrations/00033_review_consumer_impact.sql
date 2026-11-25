-- +goose Up
-- The control process resolves the allow-listed consumers because the worker
-- has no master key to reach GitHub (#139), and stores the already-masked
-- evidence on the run. The worker only attaches it to the verdict (#160).
ALTER TABLE review_runs ADD COLUMN consumer_impact jsonb;

-- +goose Down
ALTER TABLE review_runs DROP COLUMN IF EXISTS consumer_impact;
