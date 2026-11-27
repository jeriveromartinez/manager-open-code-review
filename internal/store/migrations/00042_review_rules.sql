-- +goose Up
-- Auditable review rules (#233). A human decision with a reason becomes an
-- explicit, versioned rule scoped to one repository and one path. The row keeps
-- the original decision, the positive and negative examples used to observe it,
-- who proposed and who approved it, and the last measured sample. Rules are
-- data: applying one never requires a code change or a new deployment.
CREATE TABLE review_rules (
    id bigserial PRIMARY KEY,
    repository_id bigint NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    -- Stable identity of one rule across its versions: repository + path scope
    -- + category. Changing the decision creates a version, not a new rule.
    rule_key text NOT NULL,
    version integer NOT NULL,
    status text NOT NULL DEFAULT 'proposed',
    decision text NOT NULL,
    reason text NOT NULL DEFAULT '',
    path_scope text NOT NULL DEFAULT '',
    category text NOT NULL DEFAULT '',
    -- The sample the rule was observed against. A positive example is a false
    -- positive the rule must suppress; a negative example is a real defect of
    -- the same class it must keep detecting.
    positive_examples jsonb NOT NULL DEFAULT '[]',
    negative_examples jsonb NOT NULL DEFAULT '[]',
    -- Last measured sample: repeated false positives and severe omissions.
    evaluation jsonb,
    source_finding_id text NOT NULL DEFAULT '',
    source_head_sha text NOT NULL DEFAULT '',
    proposed_by text NOT NULL DEFAULT '',
    proposed_by_id bigint REFERENCES users (id) ON DELETE SET NULL,
    approved_by text NOT NULL DEFAULT '',
    approved_by_id bigint REFERENCES users (id) ON DELETE SET NULL,
    approved_at timestamptz,
    -- Evidence expires; an expired rule stops applying until it is evaluated
    -- again against a fresh sample.
    evidence_expires_at timestamptz,
    superseded_by bigint REFERENCES review_rules (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT review_rules_status CHECK (
        status IN ('proposed', 'observation', 'active', 'disabled', 'superseded')),
    CONSTRAINT review_rules_decision CHECK (decision IN ('withdraw', 'confirm'))
);

-- One row per version of a rule.
CREATE UNIQUE INDEX idx_review_rules_version
    ON review_rules (repository_id, rule_key, version);
-- At most one observation or active version may own a rule at a time.
CREATE UNIQUE INDEX idx_review_rules_applicable
    ON review_rules (repository_id, rule_key)
    WHERE status IN ('observation', 'active');
CREATE INDEX idx_review_rules_repository ON review_rules (repository_id);

-- Which rules influenced each finding. It is written by the review worker after
-- the finding is durable, so a crash can lose the annotation but never the
-- finding. Severe findings keep applied=false: a rule never hides them.
ALTER TABLE findings ADD COLUMN rule_influences jsonb;

-- +goose Down
ALTER TABLE findings DROP COLUMN IF EXISTS rule_influences;
DROP TABLE IF EXISTS review_rules;
