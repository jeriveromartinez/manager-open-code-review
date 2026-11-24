-- +goose Up
ALTER TABLE findings ADD COLUMN existing_code text NOT NULL DEFAULT '';
ALTER TABLE findings ADD COLUMN verification jsonb;
ALTER TABLE findings ADD COLUMN triage_source text NOT NULL DEFAULT '';
UPDATE findings SET triage_source = 'human' WHERE triaged_by <> '' AND triage_source = '';
-- Every finding withdrawn before the verifier existed was a person's decision,
-- including the /qa dismissals that never recorded triaged_by.
UPDATE findings SET triage_source = 'human' WHERE status = 'withdrawn' AND triage_source = '';
CREATE INDEX idx_findings_triage_source ON findings (triage_source);

ALTER TABLE review_runs ADD COLUMN verify_session_id text;
ALTER TABLE review_runs ADD COLUMN verify_usage jsonb;

ALTER TABLE qa_policies ADD COLUMN auto_triage_mode text NOT NULL DEFAULT 'off';
ALTER TABLE qa_policies ADD COLUMN auto_withdraw boolean NOT NULL DEFAULT false;
ALTER TABLE qa_policies ADD COLUMN verify_max_findings integer NOT NULL DEFAULT 50;
ALTER TABLE qa_policies ADD COLUMN verify_max_files integer NOT NULL DEFAULT 20;
ALTER TABLE qa_policies ADD COLUMN verify_token_share integer NOT NULL DEFAULT 20;
ALTER TABLE qa_policies ADD COLUMN verify_time_share integer NOT NULL DEFAULT 15;
-- Fixed cap used when the review has no token budget (review_token_budget = 0),
-- otherwise verification would never run for those teams.
ALTER TABLE qa_policies ADD COLUMN verify_token_max bigint NOT NULL DEFAULT 100000;

-- +goose Down
ALTER TABLE qa_policies DROP COLUMN verify_token_max,
  DROP COLUMN verify_time_share, DROP COLUMN verify_token_share,
  DROP COLUMN verify_max_files, DROP COLUMN verify_max_findings,
  DROP COLUMN auto_withdraw, DROP COLUMN auto_triage_mode;
ALTER TABLE review_runs DROP COLUMN verify_usage, DROP COLUMN verify_session_id;
DROP INDEX IF EXISTS idx_findings_triage_source;
ALTER TABLE findings DROP COLUMN triage_source, DROP COLUMN verification, DROP COLUMN existing_code;
