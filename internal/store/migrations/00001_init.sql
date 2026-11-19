-- +goose Up
CREATE TABLE app_configs (
    id bigserial PRIMARY KEY,
    app_id bigint NOT NULL DEFAULT 0,
    slug text NOT NULL DEFAULT '',
    html_url text NOT NULL DEFAULT '',
    client_id text NOT NULL DEFAULT '',
    client_secret_enc text NOT NULL DEFAULT '',
    private_key_enc text NOT NULL DEFAULT '',
    webhook_secret_enc text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id bigserial PRIMARY KEY,
    github_id bigint NOT NULL,
    login text NOT NULL DEFAULT '',
    avatar_url text NOT NULL DEFAULT '',
    is_admin boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_users_github_id ON users (github_id);

CREATE TABLE sessions (
    id bigserial PRIMARY KEY,
    token_hash text NOT NULL,
    user_id bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_sessions_token_hash ON sessions (token_hash);
CREATE INDEX idx_sessions_user_id ON sessions (user_id);

CREATE TABLE installations (
    id bigint PRIMARY KEY,
    account_login text NOT NULL DEFAULT '',
    account_type text NOT NULL DEFAULT '',
    suspended_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE teams (
    id bigserial PRIMARY KEY,
    name text NOT NULL,
    slug text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_teams_name ON teams (name);
CREATE UNIQUE INDEX idx_teams_slug ON teams (slug);

CREATE TABLE team_members (
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    user_id bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, user_id)
);

CREATE TABLE repositories (
    id bigint PRIMARY KEY,
    installation_id bigint NOT NULL REFERENCES installations (id) ON DELETE CASCADE,
    full_name text NOT NULL,
    private boolean NOT NULL DEFAULT false,
    default_branch text NOT NULL DEFAULT '',
    enabled boolean NOT NULL DEFAULT false,
    team_id bigint REFERENCES teams (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_repositories_full_name ON repositories (full_name);
CREATE INDEX idx_repositories_installation_id ON repositories (installation_id);
CREATE INDEX idx_repositories_team_id ON repositories (team_id);

CREATE TABLE pull_requests (
    id bigserial PRIMARY KEY,
    repository_id bigint NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    number integer NOT NULL,
    title text NOT NULL DEFAULT '',
    author_login text NOT NULL DEFAULT '',
    author_association text NOT NULL DEFAULT '',
    head_sha text NOT NULL DEFAULT '',
    base_ref text NOT NULL DEFAULT '',
    head_ref text NOT NULL DEFAULT '',
    html_url text NOT NULL DEFAULT '',
    draft boolean NOT NULL DEFAULT false,
    state text NOT NULL DEFAULT '',
    qa_status text NOT NULL DEFAULT '',
    github_updated_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_pull_requests_repository_id_number ON pull_requests (repository_id, number);
CREATE INDEX idx_pull_requests_qa_status ON pull_requests (qa_status);

CREATE TABLE audit_logs (
    id bigserial PRIMARY KEY,
    actor_user_id bigint REFERENCES users (id) ON DELETE SET NULL,
    action text NOT NULL DEFAULT '',
    target_type text NOT NULL DEFAULT '',
    target_id text NOT NULL DEFAULT '',
    data jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_audit_logs_actor_user_id ON audit_logs (actor_user_id);

-- +goose Down
DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS pull_requests;
DROP TABLE IF EXISTS repositories;
DROP TABLE IF EXISTS team_members;
DROP TABLE IF EXISTS teams;
DROP TABLE IF EXISTS installations;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS app_configs;
