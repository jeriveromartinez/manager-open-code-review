-- +goose Up
-- One row per log line of a review run (#124). The worker writes its own
-- lifecycle lines and the progress OCR prints on stderr, so a run stays
-- auditable while it executes and after the container is recreated.
CREATE TABLE review_run_logs (
    run_id bigint NOT NULL REFERENCES review_runs(id) ON DELETE CASCADE,
    seq bigint NOT NULL,
    ts timestamptz NOT NULL,
    source text NOT NULL,
    invocation text NOT NULL DEFAULT '',
    level text NOT NULL DEFAULT 'info',
    phase text NOT NULL DEFAULT '',
    line text NOT NULL,
    PRIMARY KEY (run_id, seq)
);

-- Retention deletes by age across every run.
CREATE INDEX idx_review_run_logs_ts ON review_run_logs (ts);

-- +goose Down
DROP TABLE IF EXISTS review_run_logs;
