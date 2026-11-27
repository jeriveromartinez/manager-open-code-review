-- +goose Up
-- Critical areas of the repository (#231). A changed path matching one of these
-- globs is prioritised by the review plan before the budget runs out, even when
-- no structural signal (persistence, concurrency, public contract) fires. The
-- syntax is the same subset as exclude_globs.
ALTER TABLE qa_policies ADD COLUMN critical_globs jsonb NOT NULL DEFAULT '[]';
ALTER TABLE repository_policies ADD COLUMN critical_globs jsonb;

-- +goose Down
ALTER TABLE repository_policies DROP COLUMN IF EXISTS critical_globs;
ALTER TABLE qa_policies DROP COLUMN IF EXISTS critical_globs;
