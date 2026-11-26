-- +goose Up
-- The named observation of a head had no timestamp or source (#143), so a
-- slow read that started before a newer one could overwrite it on arrival.
-- The stamp lets the store reject an out-of-order write and report where the
-- stored verdict came from (webhook, polling or publication).
ALTER TABLE pull_requests ADD COLUMN validations_observed_at timestamptz;
ALTER TABLE pull_requests ADD COLUMN validations_source text;

-- +goose Down
ALTER TABLE pull_requests DROP COLUMN IF EXISTS validations_source;
ALTER TABLE pull_requests DROP COLUMN IF EXISTS validations_observed_at;
