-- +goose Up
CREATE TABLE llm_profiles (
    id bigserial PRIMARY KEY,
    name text NOT NULL,
    provider text NOT NULL DEFAULT '',
    model text NOT NULL DEFAULT '',
    variant text,
    mode text NOT NULL DEFAULT 'api_key',
    api_key_enc text NOT NULL DEFAULT '',
    base_url text,
    concurrency integer NOT NULL DEFAULT 1,
    enabled boolean NOT NULL DEFAULT false,
    health_status text NOT NULL DEFAULT 'unknown',
    health_checked_at timestamptz,
    health_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_llm_profiles_name ON llm_profiles (name);

CREATE TABLE qa_policies (
    id bigserial PRIMARY KEY,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    llm_profile_id bigint NOT NULL REFERENCES llm_profiles (id) ON DELETE RESTRICT,
    blocking_threshold text NOT NULL DEFAULT 'major',
    extra_instructions text NOT NULL DEFAULT '',
    exclude_globs jsonb,
    max_files_per_pass integer NOT NULL DEFAULT 40,
    max_diff_lines_per_pass integer NOT NULL DEFAULT 3000,
    max_passes integer NOT NULL DEFAULT 4,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_qa_policies_team_id ON qa_policies (team_id);

CREATE TABLE review_runs (
    id bigserial PRIMARY KEY,
    pull_request_id bigint NOT NULL REFERENCES pull_requests (id) ON DELETE CASCADE,
    profile_id bigint NOT NULL DEFAULT 0,
    head_sha text NOT NULL DEFAULT '',
    base_sha text NOT NULL DEFAULT '',
    "trigger" text NOT NULL DEFAULT 'webhook',
    status text NOT NULL DEFAULT 'queued',
    attempt integer NOT NULL DEFAULT 1,
    summary text NOT NULL DEFAULT '',
    coverage jsonb,
    usage jsonb,
    error_code text NOT NULL DEFAULT '',
    error_message text NOT NULL DEFAULT '',
    clone_token_enc text NOT NULL DEFAULT '',
    started_at timestamptz,
    finished_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_review_runs_pull_request_id ON review_runs (pull_request_id);
CREATE INDEX idx_review_runs_status ON review_runs (status);
CREATE UNIQUE INDEX idx_review_runs_active ON review_runs (pull_request_id)
  WHERE status IN ('queued','running');

CREATE TABLE findings (
    id text PRIMARY KEY,
    pull_request_id bigint NOT NULL,
    fingerprint text NOT NULL DEFAULT '',
    path text NOT NULL DEFAULT '',
    start_line integer NOT NULL DEFAULT 0,
    end_line integer NOT NULL DEFAULT 0,
    severity text NOT NULL DEFAULT '',
    category text NOT NULL DEFAULT '',
    title text NOT NULL DEFAULT '',
    body text NOT NULL DEFAULT '',
    suggestion text NOT NULL DEFAULT '',
    confidence text NOT NULL DEFAULT '',
    status text NOT NULL DEFAULT 'open',
    first_seen_run_id bigint NOT NULL DEFAULT 0,
    last_seen_run_id bigint NOT NULL DEFAULT 0,
    github_comment_id bigint NOT NULL DEFAULT 0,
    github_comment_node_id text NOT NULL DEFAULT '',
    github_thread_id text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_findings_pr_fingerprint ON findings (pull_request_id, fingerprint);
CREATE INDEX idx_findings_pull_request_id ON findings (pull_request_id);

CREATE TABLE finding_events (
    id bigserial PRIMARY KEY,
    finding_id text NOT NULL,
    review_run_id bigint NOT NULL DEFAULT 0,
    kind text NOT NULL DEFAULT '',
    actor text NOT NULL DEFAULT '',
    data jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_finding_events_finding_id ON finding_events (finding_id);
CREATE INDEX idx_finding_events_review_run_id ON finding_events (review_run_id);

CREATE TABLE github_artifacts (
    id bigserial PRIMARY KEY,
    pull_request_id bigint NOT NULL,
    kind text NOT NULL DEFAULT '',
    head_sha text NOT NULL DEFAULT '',
    external_id text NOT NULL DEFAULT '',
    url text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_artifacts_summary ON github_artifacts (pull_request_id)
  WHERE kind = 'summary_comment';
CREATE UNIQUE INDEX idx_artifacts_sha ON github_artifacts (pull_request_id, kind, head_sha)
  WHERE kind <> 'summary_comment';
CREATE INDEX idx_github_artifacts_pull_request_id ON github_artifacts (pull_request_id);

ALTER TABLE pull_requests ADD COLUMN is_fork boolean NOT NULL DEFAULT false;
ALTER TABLE pull_requests ADD COLUMN head_repo_full_name text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE pull_requests DROP COLUMN IF EXISTS head_repo_full_name;
ALTER TABLE pull_requests DROP COLUMN IF EXISTS is_fork;
DROP TABLE IF EXISTS github_artifacts;
DROP TABLE IF EXISTS finding_events;
DROP TABLE IF EXISTS findings;
DROP TABLE IF EXISTS review_runs;
DROP TABLE IF EXISTS qa_policies;
DROP TABLE IF EXISTS llm_profiles;
