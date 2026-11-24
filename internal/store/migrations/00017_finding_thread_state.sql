-- +goose Up
-- GitHub thread state is not a QA verdict. Resolving or reopening a review
-- thread updates this flag; the triage columns keep the human decision and the
-- gate keeps reading the finding status (#145).
ALTER TABLE findings ADD COLUMN github_thread_resolved boolean NOT NULL DEFAULT false;
-- Payload timestamp of the last applied thread event, used to ignore an older
-- delivery, and the moment an authorized thread transition changed the status,
-- used by a finishing review run to avoid overwriting it.
ALTER TABLE findings ADD COLUMN github_thread_event_at timestamptz;
ALTER TABLE findings ADD COLUMN github_thread_decision_at timestamptz;

-- +goose Down
ALTER TABLE findings DROP COLUMN IF EXISTS github_thread_decision_at;
ALTER TABLE findings DROP COLUMN IF EXISTS github_thread_event_at;
ALTER TABLE findings DROP COLUMN IF EXISTS github_thread_resolved;
