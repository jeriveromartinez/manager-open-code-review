-- +goose Up
-- Architectural verdict of the base–head comparison (#157). OCR 1.14.1 can
-- analyse the behavior and impact of a change alongside the file review; the
-- worker keeps the structured result so the panel, the summary and the check
-- can explain what was evaluated and what scope was left out. The analysis is
-- observational: it never changes a check, a status or an inline comment.
ALTER TABLE review_runs ADD COLUMN architecture_analysis jsonb;

-- Explicit activation and its own budget, separate from Review and
-- verification. Observation is the only mode this branch promotes.
ALTER TABLE qa_policies ADD COLUMN architecture_analysis_mode text NOT NULL DEFAULT 'observation';
ALTER TABLE qa_policies ADD COLUMN architecture_token_share integer NOT NULL DEFAULT 25;
ALTER TABLE qa_policies ADD COLUMN architecture_time_share integer NOT NULL DEFAULT 20;
ALTER TABLE qa_policies ADD COLUMN architecture_token_max bigint NOT NULL DEFAULT 100000;
ALTER TABLE qa_policies ADD CONSTRAINT qa_policies_architecture_analysis_mode_check
    CHECK (architecture_analysis_mode IN ('off', 'observation'));
ALTER TABLE qa_policies ADD CONSTRAINT qa_policies_architecture_token_share_check
    CHECK (architecture_token_share >= 0 AND architecture_token_share <= 90);
ALTER TABLE qa_policies ADD CONSTRAINT qa_policies_architecture_time_share_check
    CHECK (architecture_time_share >= 0 AND architecture_time_share <= 90);
ALTER TABLE qa_policies ADD CONSTRAINT qa_policies_architecture_token_max_check
    CHECK (architecture_token_max >= 0 AND architecture_token_max <= 1000000000);

-- +goose Down
ALTER TABLE qa_policies DROP CONSTRAINT IF EXISTS qa_policies_architecture_token_max_check;
ALTER TABLE qa_policies DROP CONSTRAINT IF EXISTS qa_policies_architecture_time_share_check;
ALTER TABLE qa_policies DROP CONSTRAINT IF EXISTS qa_policies_architecture_token_share_check;
ALTER TABLE qa_policies DROP CONSTRAINT IF EXISTS qa_policies_architecture_analysis_mode_check;
ALTER TABLE qa_policies DROP COLUMN IF EXISTS architecture_token_max;
ALTER TABLE qa_policies DROP COLUMN IF EXISTS architecture_time_share;
ALTER TABLE qa_policies DROP COLUMN IF EXISTS architecture_token_share;
ALTER TABLE qa_policies DROP COLUMN IF EXISTS architecture_analysis_mode;
ALTER TABLE review_runs DROP COLUMN IF EXISTS architecture_analysis;
