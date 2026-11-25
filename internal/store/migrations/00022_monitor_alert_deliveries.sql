-- +goose Up
-- Durable outbox between alert detection and the webhook (#154). The alert id
-- is deterministic for one event, so a retry updates the same row and a
-- receiver can deduplicate a delivery that happened before the process died.
CREATE TABLE monitor_alert_deliveries (
    id text PRIMARY KEY,
    code text NOT NULL,
    subject text NOT NULL DEFAULT '',
    message text NOT NULL,
    values jsonb NOT NULL DEFAULT '{}'::jsonb,
    alert_at timestamptz NOT NULL,
    recovered boolean NOT NULL DEFAULT false,
    attempts integer NOT NULL DEFAULT 0,
    last_error text NOT NULL DEFAULT '',
    last_attempt_at timestamptz,
    delivered_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_monitor_alert_deliveries_pending
    ON monitor_alert_deliveries (alert_at, created_at)
    WHERE delivered_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS monitor_alert_deliveries;
