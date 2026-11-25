-- +goose Up
-- Per-unit state of an interrupted review (#149). The main OCR pass and every
-- diff chunk are units: their raw result and usage are written as each one
-- finishes, so a retried run reuses the compatible ones instead of paying for
-- them again. Rows are removed when the run reaches a terminal state.
CREATE TABLE review_units (
    id bigserial PRIMARY KEY,
    run_id bigint NOT NULL REFERENCES review_runs (id) ON DELETE CASCADE,
    unit_key text NOT NULL,
    kind text NOT NULL DEFAULT '',
    path text NOT NULL DEFAULT '',
    status text NOT NULL DEFAULT '',
    result jsonb,
    usage jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_review_units_run_key ON review_units (run_id, unit_key);
CREATE INDEX idx_review_units_run_id ON review_units (run_id);

-- The run's time budget spans every attempt. started_at is reset when a run is
-- requeued, so the first claim records the origin of the aggregate deadline.
ALTER TABLE review_runs ADD COLUMN budget_started_at timestamptz;
-- OCR's own session is kept across attempts of the same run so the main pass
-- can resume the subtasks it had not finished. The scope fingerprints the
-- comparison, policy, profile and analyzer; a different scope must not resume.
ALTER TABLE review_runs ADD COLUMN ocr_session_id text NOT NULL DEFAULT '';
ALTER TABLE review_runs ADD COLUMN ocr_session_scope text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE review_runs DROP COLUMN IF EXISTS ocr_session_scope;
ALTER TABLE review_runs DROP COLUMN IF EXISTS ocr_session_id;
ALTER TABLE review_runs DROP COLUMN IF EXISTS budget_started_at;
DROP TABLE IF EXISTS review_units;
