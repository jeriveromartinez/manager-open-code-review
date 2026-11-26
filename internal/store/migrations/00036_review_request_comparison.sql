-- +goose Up
-- A review request belongs to one base-head comparison, not only to a head.
-- Retargeting a pull request to another base must ask for a review again, and
-- an old request must not count as a confirmation of the new comparison.
DROP INDEX IF EXISTS idx_review_requests_pr_sha;
CREATE UNIQUE INDEX idx_review_requests_comparison
    ON review_requests (pull_request_id, head_sha, base_ref, merge_base_sha);

-- +goose Down
DROP INDEX IF EXISTS idx_review_requests_comparison;
CREATE UNIQUE INDEX idx_review_requests_pr_sha ON review_requests (pull_request_id, head_sha);
