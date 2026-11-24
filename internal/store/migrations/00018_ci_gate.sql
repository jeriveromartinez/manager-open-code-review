-- +goose Up
-- Named observation of the head's checks (#143). The counters alone cannot tell
-- a required check that is absent from one that already passed, so the gate
-- keeps the name, origin and state of every check it saw. The column is
-- cleared with the counters when the head moves.
ALTER TABLE pull_requests ADD COLUMN validations_detail jsonb;

-- Checks the gate must see green before a review is scheduled or a success is
-- published. Each entry is {"name": "...", "origin": "app:<id>"|"status"} with
-- an optional origin; an empty list falls back to "every reported check".
ALTER TABLE qa_policies ADD COLUMN required_checks jsonb DEFAULT '[]';

-- +goose Down
ALTER TABLE qa_policies DROP COLUMN IF EXISTS required_checks;
ALTER TABLE pull_requests DROP COLUMN IF EXISTS validations_detail;
