-- +goose Up
-- Authorized producer–consumer dependencies (#160). The review may look up the
-- pinned revision of a consumer in another repository, but only the configured
-- allow-list is ever contacted and the lookup is bounded in count and bytes.
ALTER TABLE qa_policies ADD COLUMN consumer_allow_list jsonb;
ALTER TABLE qa_policies ADD COLUMN consumer_baseline_path text NOT NULL DEFAULT '';
ALTER TABLE qa_policies ADD COLUMN consumer_max_lookups integer NOT NULL DEFAULT 8;
ALTER TABLE qa_policies ADD COLUMN consumer_max_bytes bigint NOT NULL DEFAULT 1048576;
-- Zero means "use the runtime default": a direct insert that predates this
-- feature must not fail, and SaveQAPolicy replaces it before use.
ALTER TABLE qa_policies ADD CONSTRAINT qa_policies_consumer_max_lookups_check
    CHECK (consumer_max_lookups >= 0 AND consumer_max_lookups <= 50);
ALTER TABLE qa_policies ADD CONSTRAINT qa_policies_consumer_max_bytes_check
    CHECK (consumer_max_bytes >= 0 AND consumer_max_bytes <= 16777216);

-- +goose Down
ALTER TABLE qa_policies DROP CONSTRAINT IF EXISTS qa_policies_consumer_max_bytes_check;
ALTER TABLE qa_policies DROP CONSTRAINT IF EXISTS qa_policies_consumer_max_lookups_check;
ALTER TABLE qa_policies DROP COLUMN IF EXISTS consumer_max_bytes;
ALTER TABLE qa_policies DROP COLUMN IF EXISTS consumer_max_lookups;
ALTER TABLE qa_policies DROP COLUMN IF EXISTS consumer_baseline_path;
ALTER TABLE qa_policies DROP COLUMN IF EXISTS consumer_allow_list;
