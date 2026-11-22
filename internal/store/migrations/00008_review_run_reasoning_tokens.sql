-- +goose Up
-- Reasoning tokens per run. They are part of output_tokens but were only kept
-- in the aggregated usage JSON; the column makes the cost of the review model
-- measurable before and after choosing a low-effort variant (#65). NULL on runs
-- created before this migration.
ALTER TABLE review_runs
    ADD COLUMN reasoning_tokens bigint;

-- +goose Down
ALTER TABLE review_runs
    DROP COLUMN reasoning_tokens;
