-- +goose Up
-- A pull request is external when it crosses repositories, not when its head
-- repository happens to be a fork. Comparing the head and base identities also
-- keeps the internal pull requests of a repository that is itself a fork in the
-- normal review path (#148). Rows whose head repository GitHub did not report
-- are marked unknown and escalated instead of reviewed blindly.
ALTER TABLE pull_requests ADD COLUMN origin text NOT NULL DEFAULT '';
ALTER TABLE pull_requests ADD COLUMN additions integer NOT NULL DEFAULT 0;
ALTER TABLE pull_requests ADD COLUMN deletions integer NOT NULL DEFAULT 0;
ALTER TABLE pull_requests ADD COLUMN changed_files integer NOT NULL DEFAULT 0;

UPDATE pull_requests AS pr
SET origin = CASE
        WHEN pr.head_repo_full_name = '' AND NOT pr.is_fork THEN 'internal'
        WHEN pr.head_repo_full_name = '' THEN 'unknown'
        WHEN lower(pr.head_repo_full_name) = lower(r.full_name) THEN 'internal'
        ELSE 'cross_repo'
    END
FROM repositories AS r
WHERE r.id = pr.repository_id;

UPDATE pull_requests SET is_fork = origin <> 'internal';

-- External contributions are reviewed read-only only when the team allows it
-- and the author is trusted. The size, run-rate and daily token limits are
-- enforced before a run is queued; going over any of them escalates to a
-- person instead of spending the engine's budget.
ALTER TABLE qa_policies
    ADD COLUMN allow_external_reviews boolean NOT NULL DEFAULT false,
    ADD COLUMN external_trusted_associations jsonb
        DEFAULT '["OWNER","MEMBER","COLLABORATOR"]',
    ADD COLUMN max_external_diff_lines integer NOT NULL DEFAULT 3000,
    ADD COLUMN max_external_runs_per_day integer NOT NULL DEFAULT 10,
    ADD COLUMN external_daily_token_budget bigint NOT NULL DEFAULT 0;

-- An external run reserves spend as soon as it is queued, so a concurrent
-- contribution sees the in-flight cost instead of an under-counted total.
ALTER TABLE review_runs ADD COLUMN reserved_tokens bigint NOT NULL DEFAULT 0;
CREATE INDEX idx_review_runs_created_at ON review_runs (created_at);

-- +goose Down
DROP INDEX IF EXISTS idx_review_runs_created_at;
ALTER TABLE review_runs DROP COLUMN IF EXISTS reserved_tokens;
ALTER TABLE qa_policies
    DROP COLUMN IF EXISTS external_daily_token_budget,
    DROP COLUMN IF EXISTS max_external_runs_per_day,
    DROP COLUMN IF EXISTS max_external_diff_lines,
    DROP COLUMN IF EXISTS external_trusted_associations,
    DROP COLUMN IF EXISTS allow_external_reviews;
ALTER TABLE pull_requests
    DROP COLUMN IF EXISTS changed_files,
    DROP COLUMN IF EXISTS deletions,
    DROP COLUMN IF EXISTS additions,
    DROP COLUMN IF EXISTS origin;
