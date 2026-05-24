-- +goose Up

CREATE TABLE pull_requests (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    external_pr_id  BIGINT NOT NULL,
    repo_full_name  TEXT NOT NULL,
    pr_number       INT NOT NULL,
    head_sha        TEXT NOT NULL,
    base_sha        TEXT NOT NULL,
    author          TEXT NOT NULL,
    state           TEXT NOT NULL CHECK (state IN ('open', 'closed', 'merged')),
    metadata        JSONB NOT NULL DEFAULT '{}',
    deleted_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (external_pr_id, head_sha)
);

CREATE INDEX pull_requests_repo_pr_idx ON pull_requests (repo_full_name, pr_number);
CREATE INDEX pull_requests_active_idx ON pull_requests (id) WHERE deleted_at IS NULL;

CREATE TABLE pipeline_runs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pull_request_id UUID NOT NULL REFERENCES pull_requests(id) ON DELETE RESTRICT,
    head_sha        TEXT NOT NULL,
    prompt_version  TEXT NOT NULL,
    config_hash     TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'running'
                    CHECK (status IN ('running', 'completed', 'failed')),
    tasks_total        INT,
    tasks_completed    INT,
    tasks_failed       INT,
    findings_total     INT,
    findings_posted    INT,
    findings_suppressed INT,
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ,
    metadata        JSONB NOT NULL DEFAULT '{}'
);

CREATE INDEX pipeline_runs_pr_id_idx ON pipeline_runs (pull_request_id);
CREATE UNIQUE INDEX pipeline_runs_active_unique
    ON pipeline_runs (pull_request_id, head_sha)
    WHERE status = 'running';

CREATE TABLE review_tasks (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pull_request_id UUID NOT NULL REFERENCES pull_requests(id) ON DELETE RESTRICT,
    pipeline_run_id UUID NOT NULL REFERENCES pipeline_runs(id) ON DELETE RESTRICT,
    task_type       TEXT NOT NULL CHECK (task_type IN ('security', 'logic', 'test_coverage', 'style')),
    file_path       TEXT NOT NULL,
    symbol          TEXT NOT NULL DEFAULT '',
    risk_score      FLOAT NOT NULL,
    model_id        TEXT NOT NULL,
    diff_hunk       TEXT,
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'running', 'completed', 'failed')),
    error           TEXT,
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX review_tasks_pr_id_idx ON review_tasks (pull_request_id);
CREATE INDEX review_tasks_run_id_idx ON review_tasks (pipeline_run_id);
CREATE INDEX review_tasks_status_idx ON review_tasks (status) WHERE status = 'pending';

CREATE TABLE findings (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    review_task_id      UUID NOT NULL REFERENCES review_tasks(id) ON DELETE RESTRICT,
    pull_request_id     UUID NOT NULL REFERENCES pull_requests(id) ON DELETE RESTRICT,
    pipeline_run_id     UUID NOT NULL REFERENCES pipeline_runs(id) ON DELETE RESTRICT,

    -- Location
    repo_full_name      TEXT NOT NULL,
    file_path           TEXT NOT NULL,
    start_line          INT,
    end_line            INT,
    symbol              TEXT NOT NULL DEFAULT '',

    -- Classification
    category            TEXT NOT NULL
                        CHECK (category IN ('security', 'logic', 'test_coverage', 'style', 'performance')),
    confidence_tier     TEXT NOT NULL CHECK (confidence_tier IN ('high', 'medium', 'low')),
    confidence_score    FLOAT NOT NULL CHECK (confidence_score BETWEEN 0 AND 1),
    severity            TEXT NOT NULL
                        CHECK (severity IN ('critical', 'high', 'medium', 'low', 'info')),

    -- Content
    title               TEXT NOT NULL,
    body                TEXT NOT NULL,
    suggestion          TEXT,

    -- Fingerprinting
    location_hash       TEXT NOT NULL,
    content_hash        TEXT,
    head_sha            TEXT NOT NULL,

    -- Lifecycle
    posted_at           TIMESTAMPTZ,
    external_comment_id BIGINT,
    addressed_status    TEXT NOT NULL DEFAULT 'unaddressed'
                        CHECK (addressed_status IN ('unaddressed', 'likely_addressed', 'confirmed')),
    suppression_reason  TEXT
                        CHECK (suppression_reason IS NULL OR suppression_reason IN
                            ('duplicate', 'low_confidence', 'dismissed_fingerprint')),
    dismissed_at        TIMESTAMPTZ,
    dismissed_by        TEXT,

    -- Provenance
    model_id            TEXT NOT NULL,
    prompt_tokens       INT,
    completion_tokens   INT,
    metadata            JSONB NOT NULL DEFAULT '{}',

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX findings_pr_id_idx ON findings (pull_request_id);
CREATE INDEX findings_run_id_idx ON findings (pipeline_run_id);
CREATE INDEX findings_location_hash_idx ON findings (location_hash);
CREATE UNIQUE INDEX findings_location_hash_pr_sha_idx ON findings (location_hash, pull_request_id, head_sha);
CREATE INDEX findings_addressed_status_idx ON findings (addressed_status)
    WHERE addressed_status = 'unaddressed';
CREATE INDEX findings_repo_full_name_idx ON findings (repo_full_name);

CREATE TABLE dismissed_fingerprints (
    fingerprint     TEXT NOT NULL,
    repo_full_name  TEXT NOT NULL,
    dismissed_by    TEXT NOT NULL,
    reason          TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (fingerprint, repo_full_name)
);

CREATE TABLE finding_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    finding_id  UUID NOT NULL REFERENCES findings(id) ON DELETE RESTRICT,
    event_type  TEXT NOT NULL CHECK (event_type IN (
        'thumbs_up', 'thumbs_down',
        'created', 'posted', 'suppressed',
        'addressed', 'dismissed', 'resolved',
        'confidence_adjusted', 'tier_changed'
    )),
    actor       TEXT NOT NULL,
    old_value   TEXT,
    new_value   TEXT,
    metadata    JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX finding_events_reaction_unique
    ON finding_events (finding_id, event_type, actor)
    WHERE event_type IN ('thumbs_up', 'thumbs_down');

CREATE INDEX finding_events_finding_id_idx ON finding_events (finding_id);
CREATE INDEX finding_events_type_idx ON finding_events (event_type);

-- updated_at trigger: auto-maintain updated_at on tables that have it.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER pull_requests_updated_at
    BEFORE UPDATE ON pull_requests
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER findings_updated_at
    BEFORE UPDATE ON findings
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER dismissed_fingerprints_updated_at
    BEFORE UPDATE ON dismissed_fingerprints
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();


-- +goose Down

DROP TRIGGER IF EXISTS dismissed_fingerprints_updated_at ON dismissed_fingerprints;
DROP TRIGGER IF EXISTS findings_updated_at ON findings;
DROP TRIGGER IF EXISTS pull_requests_updated_at ON pull_requests;
DROP FUNCTION IF EXISTS set_updated_at;

DROP TABLE IF EXISTS finding_events;
DROP TABLE IF EXISTS dismissed_fingerprints;
DROP TABLE IF EXISTS findings;
DROP TABLE IF EXISTS review_tasks;
DROP TABLE IF EXISTS pipeline_runs;
DROP TABLE IF EXISTS pull_requests;
