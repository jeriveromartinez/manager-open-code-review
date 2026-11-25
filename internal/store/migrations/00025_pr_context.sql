-- +goose Up
-- The PR description is review context (#139): it carries the intent and the
-- acceptance criteria that the diff alone cannot explain. It is untrusted text
-- and is bounded and fenced before reaching the model.
ALTER TABLE pull_requests ADD COLUMN body text NOT NULL DEFAULT '';

-- The linked-issue context is resolved by the control process, which holds the
-- master key and GitHub App credentials. The review worker has neither, so the
-- snapshot travels with the run.
ALTER TABLE review_runs ADD COLUMN linked_issues jsonb;

-- +goose Down
ALTER TABLE review_runs DROP COLUMN IF EXISTS linked_issues;
ALTER TABLE pull_requests DROP COLUMN IF EXISTS body;
