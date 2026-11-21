-- +goose Up
ALTER TABLE findings ADD COLUMN triaged_by text NOT NULL DEFAULT '';
ALTER TABLE findings ADD COLUMN triaged_at timestamptz;
CREATE INDEX idx_findings_triaged_at ON findings (triaged_at) WHERE triaged_at IS NOT NULL;

ALTER TABLE pull_requests ADD COLUMN qa_status_changed_at timestamptz NOT NULL DEFAULT now();
-- The last write is the closest known moment for rows that predate the column.
UPDATE pull_requests SET qa_status_changed_at = updated_at;

-- A trigger instead of application code: several store paths write qa_status
-- (sync, gate, close sweep) and none of them can forget to move the clock.
-- +goose StatementBegin
CREATE FUNCTION touch_qa_status_changed_at() RETURNS trigger AS $$
BEGIN
    IF NEW.qa_status IS DISTINCT FROM OLD.qa_status THEN
        NEW.qa_status_changed_at := now();
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trg_pull_requests_qa_status_changed_at
    BEFORE UPDATE OF qa_status ON pull_requests
    FOR EACH ROW EXECUTE FUNCTION touch_qa_status_changed_at();

-- +goose Down
DROP TRIGGER IF EXISTS trg_pull_requests_qa_status_changed_at ON pull_requests;
DROP FUNCTION IF EXISTS touch_qa_status_changed_at();
ALTER TABLE pull_requests DROP COLUMN IF EXISTS qa_status_changed_at;
DROP INDEX IF EXISTS idx_findings_triaged_at;
ALTER TABLE findings DROP COLUMN IF EXISTS triaged_at;
ALTER TABLE findings DROP COLUMN IF EXISTS triaged_by;
