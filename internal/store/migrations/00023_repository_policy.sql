-- +goose Up
-- Per-repository review policy overrides (#153). A NULL column means the field
-- inherits the team policy, so only explicit overrides are stored instead of a
-- duplicated policy. observation_mode resolves and records the overrides
-- without changing what the review enforces.
CREATE TABLE repository_policies (
    repository_id bigint PRIMARY KEY REFERENCES repositories (id) ON DELETE CASCADE,
    observation_mode boolean NOT NULL DEFAULT false,
    blocking_threshold text,
    extra_instructions text,
    exclude_globs jsonb,
    max_files_per_pass integer,
    max_diff_lines_per_pass integer,
    max_passes integer,
    require_green_ci boolean,
    required_checks jsonb,
    review_context_ceiling integer,
    review_effort text,
    review_token_budget bigint,
    auto_triage_mode text,
    auto_withdraw boolean,
    verify_max_findings integer,
    verify_max_files integer,
    verify_token_share integer,
    verify_time_share integer,
    verify_token_max bigint,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Revision of the effective policy a run executed under. An empty value marks
-- runs that predate the recording. Observation runs record the revision of the
-- policy they observed, which is not the one they enforced.
ALTER TABLE review_runs ADD COLUMN policy_revision text NOT NULL DEFAULT '';
ALTER TABLE review_runs ADD COLUMN policy_observation boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE review_runs DROP COLUMN IF EXISTS policy_observation;
ALTER TABLE review_runs DROP COLUMN IF EXISTS policy_revision;
DROP TABLE IF EXISTS repository_policies;
