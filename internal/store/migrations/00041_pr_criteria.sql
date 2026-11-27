-- +goose Up
-- Per-criterion verdict of the pull request against the criteria of its linked
-- issue (#232). A criterion is identified by the fingerprint of its text and by
-- the version of the source it came from; the evidence cites the head it was
-- read at. The row is invalidated, not deleted, so the history of a criterion
-- that changed or whose evidence a push invalidated stays visible.
CREATE TABLE pr_criteria (
    id bigserial PRIMARY KEY,
    pull_request_id bigint NOT NULL REFERENCES pull_requests (id) ON DELETE CASCADE,
    review_run_id bigint REFERENCES review_runs (id) ON DELETE SET NULL,
    head_sha text NOT NULL,
    base_ref text NOT NULL DEFAULT '',
    criterion_id text NOT NULL,
    fingerprint text NOT NULL,
    source_kind text NOT NULL DEFAULT '',
    source_repo text NOT NULL DEFAULT '',
    source_number integer NOT NULL DEFAULT 0,
    source_url text NOT NULL DEFAULT '',
    source_title text NOT NULL DEFAULT '',
    source_version text NOT NULL DEFAULT '',
    criterion_text text NOT NULL,
    status text NOT NULL,
    evidence jsonb NOT NULL DEFAULT '[]',
    observed text NOT NULL DEFAULT '',
    required text NOT NULL DEFAULT '',
    reason text NOT NULL DEFAULT '',
    uncertainty text NOT NULL DEFAULT '',
    evaluator text NOT NULL DEFAULT '',
    evaluated_at timestamptz NOT NULL DEFAULT now(),
    invalidated_at timestamptz,
    invalidated_reason text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pr_criteria_status CHECK (
        status IN ('cumplido', 'incumplido', 'no verificable', 'no aplicable')
    )
);

-- Only one row per criterion may describe the current evaluation. The previous
-- one is invalidated in the same transaction that records its replacement.
CREATE UNIQUE INDEX idx_pr_criteria_current
    ON pr_criteria (pull_request_id, criterion_id)
    WHERE invalidated_at IS NULL;

-- Publication and the panel read the current verdict of the current head.
CREATE INDEX idx_pr_criteria_current_head
    ON pr_criteria (pull_request_id, head_sha)
    WHERE invalidated_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_pr_criteria_current_head;
DROP INDEX IF EXISTS idx_pr_criteria_current;
DROP TABLE IF EXISTS pr_criteria;
