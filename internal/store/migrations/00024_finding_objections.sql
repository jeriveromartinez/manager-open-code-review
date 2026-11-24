-- +goose Up
-- Durable objection queue (#152). A reply on a finding's review thread is
-- recorded here by the app process and evaluated by the worker, which owns OCR.
-- The row is the intent: the decision and the published reply are written back
-- to it, so a crash or a redelivered webhook can resume without duplicating the
-- reply or losing the decision.
CREATE TABLE finding_objections (
    id bigserial PRIMARY KEY,
    finding_id text NOT NULL REFERENCES findings(id) ON DELETE CASCADE,
    pull_request_id bigint NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    head_sha text NOT NULL DEFAULT '',
    comment_id bigint NOT NULL,
    delivery_id text NOT NULL DEFAULT '',
    actor text NOT NULL DEFAULT '',
    actor_id bigint,
    authorized boolean NOT NULL DEFAULT false,
    body text NOT NULL DEFAULT '',
    file_path text NOT NULL DEFAULT '',
    file_content bytea,
    -- Read-only installation token sealed with the worker key. The app mints it
    -- so the worker, which has OCR but no App credentials, can fetch the file.
    read_token_enc text NOT NULL DEFAULT '',
    state text NOT NULL DEFAULT 'queued',
    action text NOT NULL DEFAULT '',
    status text NOT NULL DEFAULT '',
    apply boolean NOT NULL DEFAULT false,
    reason text NOT NULL DEFAULT '',
    evidence text NOT NULL DEFAULT '',
    verification jsonb,
    failure text NOT NULL DEFAULT '',
    replied_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- One intent per review comment, so a redelivery is the same objection.
CREATE UNIQUE INDEX idx_finding_objections_comment
    ON finding_objections (finding_id, comment_id);
CREATE INDEX idx_finding_objections_state ON finding_objections (state);

-- +goose Down
DROP TABLE IF EXISTS finding_objections;
