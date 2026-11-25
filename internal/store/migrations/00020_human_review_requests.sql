-- +goose Up
-- A team can point the review escalation at a GitHub user, a team, or both.
-- The request itself is recorded per pull request and head so a redelivery or
-- a retried publish never asks GitHub twice for the same revision.
ALTER TABLE qa_policies ADD COLUMN reviewer_login text NOT NULL DEFAULT '';
ALTER TABLE qa_policies ADD COLUMN reviewer_team text NOT NULL DEFAULT '';

CREATE TABLE review_requests (
    id bigserial PRIMARY KEY,
    pull_request_id bigint NOT NULL REFERENCES pull_requests (id) ON DELETE CASCADE,
    head_sha text NOT NULL,
    base_ref text NOT NULL DEFAULT '',
    merge_base_sha text NOT NULL DEFAULT '',
    cause text NOT NULL DEFAULT '',
    reviewer_login text NOT NULL DEFAULT '',
    reviewer_team text NOT NULL DEFAULT '',
    status text NOT NULL DEFAULT 'pending',
    attempts integer NOT NULL DEFAULT 0,
    last_error text NOT NULL DEFAULT '',
    delivered_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_review_requests_pr_sha ON review_requests (pull_request_id, head_sha);
CREATE INDEX idx_review_requests_status ON review_requests (status);

-- +goose Down
DROP TABLE IF EXISTS review_requests;
ALTER TABLE qa_policies DROP COLUMN IF EXISTS reviewer_team;
ALTER TABLE qa_policies DROP COLUMN IF EXISTS reviewer_login;
