-- +goose Up
-- Trusted architecture baseline (#158). The path points to the repository file
-- that declares decisions and forbidden dependency directions. It is always
-- resolved from the reviewed base commit; the pull request head is evidence of
-- what changed, never the rule that validates it.
ALTER TABLE qa_policies ADD COLUMN architecture_baseline_path text NOT NULL DEFAULT '.architecture/baseline.json';
ALTER TABLE qa_policies ADD COLUMN architecture_exception_policy text NOT NULL DEFAULT 'deny';
ALTER TABLE qa_policies ADD CONSTRAINT qa_policies_architecture_exception_policy_check
    CHECK (architecture_exception_policy IN ('deny', 'baseline'));

-- +goose Down
ALTER TABLE qa_policies DROP CONSTRAINT IF EXISTS qa_policies_architecture_exception_policy_check;
ALTER TABLE qa_policies DROP COLUMN IF EXISTS architecture_exception_policy;
ALTER TABLE qa_policies DROP COLUMN IF EXISTS architecture_baseline_path;
