-- +goose Up
-- Structured token consumption per run. The columns stay NULL for runs created
-- before this migration so the panel can tell "no data" apart from a real zero
-- (#73).
ALTER TABLE review_runs
    ADD COLUMN input_tokens bigint,
    ADD COLUMN output_tokens bigint,
    ADD COLUMN cache_read_tokens bigint,
    ADD COLUMN cache_write_tokens bigint,
    ADD COLUMN total_tokens bigint,
    ADD COLUMN max_context_tokens bigint;

CREATE INDEX idx_review_runs_finished_tokens
    ON review_runs (finished_at) WHERE total_tokens IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_review_runs_finished_tokens;
ALTER TABLE review_runs
    DROP COLUMN input_tokens,
    DROP COLUMN output_tokens,
    DROP COLUMN cache_read_tokens,
    DROP COLUMN cache_write_tokens,
    DROP COLUMN total_tokens,
    DROP COLUMN max_context_tokens;
