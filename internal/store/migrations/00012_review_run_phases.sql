-- +goose Up
-- Current step of a running review (#123). The panel shows it live instead of
-- waiting for the terminal state; queued and terminal runs keep it empty.
ALTER TABLE review_runs ADD COLUMN phase text NOT NULL DEFAULT '';
ALTER TABLE review_runs ADD COLUMN phase_started_at timestamptz;

-- +goose Down
ALTER TABLE review_runs DROP COLUMN IF EXISTS phase_started_at;
ALTER TABLE review_runs DROP COLUMN IF EXISTS phase;
