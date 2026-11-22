-- +goose Up
-- Append-only history of LLM profile health checks. The profile row only keeps
-- the latest verdict, so per-profile token accounting needs every check,
-- including the ones outside a review (#73). Token columns are NULL when the
-- provider reported no usage or the probe failed, so a missing figure is never
-- averaged as zero.
CREATE TABLE llm_health_checks (
    id bigserial PRIMARY KEY,
    profile_id bigint NOT NULL REFERENCES llm_profiles (id) ON DELETE CASCADE,
    status text NOT NULL DEFAULT 'unknown',
    error_message text NOT NULL DEFAULT '',
    input_tokens bigint,
    output_tokens bigint,
    reasoning_tokens bigint,
    cache_read_tokens bigint,
    cache_write_tokens bigint,
    total_tokens bigint,
    max_context_tokens bigint,
    checked_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_llm_health_checks_profile_checked
    ON llm_health_checks (profile_id, checked_at DESC);
CREATE INDEX idx_llm_health_checks_checked_tokens
    ON llm_health_checks (checked_at) WHERE total_tokens IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS llm_health_checks;
