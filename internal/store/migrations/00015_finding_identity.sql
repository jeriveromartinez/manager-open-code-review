-- +goose Up
-- Every existing row keeps its version-1 fingerprint and is migrated in place
-- the first time a review matches it. New rows default to the current scheme.
ALTER TABLE findings ADD COLUMN identity_version smallint NOT NULL DEFAULT 1;
ALTER TABLE findings ALTER COLUMN identity_version SET DEFAULT 2;

-- +goose Down
ALTER TABLE findings DROP COLUMN IF EXISTS identity_version;
