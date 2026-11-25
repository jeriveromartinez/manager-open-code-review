-- +goose Up
-- Human outcome of an inconclusive review (#146). The automated result stays in
-- review_runs; this row records who examined the scope the engine could not
-- cover, what they looked at and what they concluded. It is tied to the same
-- comparison identity as the run (#141), so a push or retarget invalidates it
-- instead of letting a stale decision keep a gate closed.
CREATE TABLE human_verdicts (
    id bigserial PRIMARY KEY,
    pull_request_id bigint NOT NULL REFERENCES pull_requests (id) ON DELETE CASCADE,
    head_sha text NOT NULL,
    base_ref text NOT NULL DEFAULT '',
    merge_base_sha text NOT NULL DEFAULT '',
    decision text NOT NULL,
    conclusion text NOT NULL DEFAULT '',
    reason text NOT NULL DEFAULT '',
    scope jsonb NOT NULL DEFAULT '{}',
    scope_complete boolean NOT NULL DEFAULT false,
    actor_login text NOT NULL DEFAULT '',
    actor_user_id bigint REFERENCES users (id) ON DELETE SET NULL,
    source text NOT NULL DEFAULT 'panel',
    invalidated_at timestamptz,
    invalidated_reason text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT human_verdicts_decision CHECK (decision IN ('approved', 'changes_requested'))
);

-- At most one verdict may own the current comparison of a pull request. The
-- previous one is invalidated in the same transaction that records its
-- replacement, and a push or retarget invalidates it without a replacement.
CREATE UNIQUE INDEX idx_human_verdicts_active
    ON human_verdicts (pull_request_id)
    WHERE invalidated_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_human_verdicts_active;
DROP TABLE IF EXISTS human_verdicts;
