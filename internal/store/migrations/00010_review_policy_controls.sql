-- +goose Up
-- Per-team cost controls for the review engine (#72). The context ceiling is
-- nullable because NULL inherits the global review_context_ceiling setting;
-- effort defaults to low like the issue asks, and a zero budget disables the
-- optional spending cap. Enforcement lands with the native OCR engine (#71).
ALTER TABLE qa_policies
    ADD COLUMN review_context_ceiling integer,
    ADD COLUMN review_effort text NOT NULL DEFAULT 'low',
    ADD COLUMN review_token_budget bigint NOT NULL DEFAULT 0;

ALTER TABLE qa_policies
    ADD CONSTRAINT qa_policies_review_context_ceiling_check
        CHECK (review_context_ceiling IS NULL OR review_context_ceiling BETWEEN 4096 AND 2000000),
    ADD CONSTRAINT qa_policies_review_effort_check
        CHECK (review_effort IN ('low', 'medium', 'high')),
    ADD CONSTRAINT qa_policies_review_token_budget_check
        CHECK (review_token_budget BETWEEN 0 AND 1000000000);

-- +goose Down
ALTER TABLE qa_policies
    DROP CONSTRAINT IF EXISTS qa_policies_review_token_budget_check,
    DROP CONSTRAINT IF EXISTS qa_policies_review_effort_check,
    DROP CONSTRAINT IF EXISTS qa_policies_review_context_ceiling_check,
    DROP COLUMN IF EXISTS review_token_budget,
    DROP COLUMN IF EXISTS review_effort,
    DROP COLUMN IF EXISTS review_context_ceiling;
