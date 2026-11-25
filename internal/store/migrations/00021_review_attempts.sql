-- +goose Up
-- Per-attempt execution and usage attribution (#150). The policy picks one
-- profile at enqueue time, but the worker re-reads it on start, so the profile
-- that actually ran can differ. review_runs keeps the executed profile_id (the
-- one usage is attributed to) and enqueued_profile_id preserves the enqueue
-- snapshot; review_attempts holds the ordered destinations with their own
-- provider, model, config revision and consumption.
ALTER TABLE review_runs ADD COLUMN enqueued_profile_id bigint NOT NULL DEFAULT 0;
UPDATE review_runs SET enqueued_profile_id = profile_id WHERE enqueued_profile_id = 0;

-- Ordered, team-authorized fallback destinations. Empty keeps the previous
-- single-profile behaviour.
ALTER TABLE qa_policies ADD COLUMN fallback_profile_ids jsonb NOT NULL DEFAULT '[]'::jsonb;

-- A fallback destination may only receive private repository code when it is
-- explicitly authorized (opt-in): the capability is off for every profile,
-- including the ones that predate this column.
ALTER TABLE llm_profiles ADD COLUMN allow_private_data boolean NOT NULL DEFAULT false;

CREATE TABLE review_attempts (
    id bigserial PRIMARY KEY,
    run_id bigint NOT NULL REFERENCES review_runs (id) ON DELETE CASCADE,
    seq integer NOT NULL,
    profile_id bigint NOT NULL DEFAULT 0,
    provider text NOT NULL DEFAULT '',
    model text NOT NULL DEFAULT '',
    base_url text NOT NULL DEFAULT '',
    config_revision text NOT NULL DEFAULT '',
    status text NOT NULL DEFAULT 'running',
    error_code text NOT NULL DEFAULT '',
    error_message text NOT NULL DEFAULT '',
    input_tokens bigint,
    output_tokens bigint,
    reasoning_tokens bigint,
    cache_read_tokens bigint,
    cache_write_tokens bigint,
    total_tokens bigint,
    max_context_tokens bigint,
    started_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT review_attempts_status_check
        CHECK (status IN ('running', 'succeeded', 'failed')),
    CONSTRAINT review_attempts_seq_unique UNIQUE (run_id, seq)
);

CREATE INDEX idx_review_attempts_run ON review_attempts (run_id, seq);
CREATE INDEX idx_review_attempts_profile ON review_attempts (profile_id, finished_at);

-- +goose Down
DROP TABLE IF EXISTS review_attempts;
ALTER TABLE llm_profiles DROP COLUMN IF EXISTS allow_private_data;
ALTER TABLE qa_policies DROP COLUMN IF EXISTS fallback_profile_ids;
ALTER TABLE review_runs DROP COLUMN IF EXISTS enqueued_profile_id;
