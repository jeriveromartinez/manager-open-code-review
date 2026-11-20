-- +goose Up
ALTER TABLE pull_requests ADD COLUMN validations_passed integer NOT NULL DEFAULT 0;
ALTER TABLE pull_requests ADD COLUMN validations_failed integer NOT NULL DEFAULT 0;
ALTER TABLE pull_requests ADD COLUMN validations_total integer NOT NULL DEFAULT 0;
ALTER TABLE qa_policies ADD COLUMN require_green_ci boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE qa_policies DROP COLUMN IF EXISTS require_green_ci;
ALTER TABLE pull_requests DROP COLUMN IF EXISTS validations_total;
ALTER TABLE pull_requests DROP COLUMN IF EXISTS validations_failed;
ALTER TABLE pull_requests DROP COLUMN IF EXISTS validations_passed;
