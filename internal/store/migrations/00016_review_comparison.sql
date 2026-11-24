-- +goose Up
-- Identity of the comparison a pull request is being reviewed at (#141). The
-- base tip comes from the payload; the merge base is resolved through the
-- Compare API and cleared whenever the head or the base moves. Runs snapshot
-- the same key so deduplication and publication can tell two comparisons of
-- the same head apart.
ALTER TABLE pull_requests ADD COLUMN base_sha text NOT NULL DEFAULT '';
ALTER TABLE pull_requests ADD COLUMN merge_base_sha text NOT NULL DEFAULT '';
ALTER TABLE review_runs ADD COLUMN base_ref text NOT NULL DEFAULT '';
ALTER TABLE review_runs ADD COLUMN base_tip_sha text NOT NULL DEFAULT '';

-- Seed the comparison identity of existing pull requests from their latest run
-- of the same head. Without this, a pre-#141 run with a known merge base would
-- stop covering its pull request, while an empty key would cover it forever.
UPDATE pull_requests pr
SET merge_base_sha = latest.base_sha
FROM (
    SELECT DISTINCT ON (pull_request_id) pull_request_id, head_sha, base_sha
    FROM review_runs
    WHERE base_sha <> ''
    ORDER BY pull_request_id, id DESC
) latest
WHERE latest.pull_request_id = pr.id
  AND latest.head_sha = pr.head_sha
  AND pr.merge_base_sha = '';

-- +goose Down
UPDATE pull_requests SET merge_base_sha = '';
ALTER TABLE review_runs DROP COLUMN IF EXISTS base_tip_sha;
ALTER TABLE review_runs DROP COLUMN IF EXISTS base_ref;
ALTER TABLE pull_requests DROP COLUMN IF EXISTS merge_base_sha;
ALTER TABLE pull_requests DROP COLUMN IF EXISTS base_sha;
