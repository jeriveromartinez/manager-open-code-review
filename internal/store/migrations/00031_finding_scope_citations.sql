-- +goose Up
-- Design findings can describe one file, several files or the whole pull
-- request (#162). Citations carry every verified path:line reference, with the
-- base side for evidence found only in the removed code. Existing rows keep the
-- single-file behavior.
ALTER TABLE findings ADD COLUMN scope text NOT NULL DEFAULT 'file';
ALTER TABLE findings ADD COLUMN citations jsonb;
ALTER TABLE findings ADD COLUMN advisory boolean NOT NULL DEFAULT false;
ALTER TABLE findings ADD CONSTRAINT findings_scope_check
    CHECK (scope IN ('file', 'files', 'pr'));

-- +goose Down
ALTER TABLE findings DROP CONSTRAINT IF EXISTS findings_scope_check;
ALTER TABLE findings DROP COLUMN IF EXISTS advisory;
ALTER TABLE findings DROP COLUMN IF EXISTS citations;
ALTER TABLE findings DROP COLUMN IF EXISTS scope;
