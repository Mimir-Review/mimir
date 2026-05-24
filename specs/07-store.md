# Spec 07: Store — Database Schema & Persistence

> **Status:** Approved
> **Date:** 2026-03-27
> **Package:** `internal/store`
> **Implements:** `pkg/adapter.StoreAdapter`
> **ADR:** [0002-database-postgresql.md](../adr/0002-database-postgresql.md)

---

## Responsibilities

1. Own all database schema (goose migrations)
2. Implement `StoreAdapter` via sqlc-generated queries
3. Manage connection pooling (`pgx/v5`)
4. Provide transaction support where needed

---

## Schema

### Migration 001: Core Tables

```sql
-- 001_create_core_tables.sql

CREATE TABLE pull_requests (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    external_pr_id  BIGINT NOT NULL,     -- GitHub PR ID, GitLab MR IID, etc.
    repo_full_name  TEXT NOT NULL,
    pr_number       INT NOT NULL,
    head_sha        TEXT NOT NULL,
    base_sha        TEXT NOT NULL,
    author          TEXT NOT NULL,
    state           TEXT NOT NULL CHECK (state IN ('open', 'closed', 'merged')),
    metadata        JSONB NOT NULL DEFAULT '{}',
    deleted_at      TIMESTAMPTZ,          -- soft delete; NULL = active
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (external_pr_id, head_sha)
);

CREATE INDEX pull_requests_repo_pr_idx ON pull_requests (repo_full_name, pr_number);
CREATE INDEX pull_requests_active_idx ON pull_requests (id) WHERE deleted_at IS NULL;

-- pipeline_runs tracks each execution of the review pipeline for a given PR.
-- This is the primary audit record: "we ran the pipeline at time T with config X."
CREATE TABLE pipeline_runs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pull_request_id UUID NOT NULL REFERENCES pull_requests(id) ON DELETE RESTRICT,
    head_sha        TEXT NOT NULL,
    prompt_version  TEXT NOT NULL,         -- e.g. "v1"; groups findings by prompt generation
    config_hash     TEXT NOT NULL,         -- sha256 of serialized runtime config at execution time
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
    metadata        JSONB NOT NULL DEFAULT '{}'   -- model routing snapshot, budget config, etc.
);

CREATE INDEX pipeline_runs_pr_id_idx ON pipeline_runs (pull_request_id);
-- Prevent duplicate running pipelines for the same PR + commit.
-- If a process crashes mid-run and River re-enqueues the job, the new run
-- must first reconcile the orphaned 'running' row (see ReconcileStalePipelineRuns).
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
    diff_hunk       TEXT,                 -- captured diff hunk used for this task (survives force-push/rebase)
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
    repo_full_name      TEXT NOT NULL,    -- denormalized for direct dismissed_fingerprints lookups
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
    head_sha            TEXT NOT NULL,     -- the commit this finding was produced against

    -- Lifecycle
    posted_at           TIMESTAMPTZ,
    external_comment_id BIGINT,          -- GitHub comment ID, GitLab note ID, etc.
    addressed_status    TEXT NOT NULL DEFAULT 'unaddressed'
                        CHECK (addressed_status IN ('unaddressed', 'likely_addressed', 'confirmed')),
    suppression_reason  TEXT               -- NULL if not suppressed; otherwise: 'duplicate', 'low_confidence', 'dismissed_fingerprint'
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
-- Unique per location per commit, not per PR. Multiple pushes to the same PR
-- produce separate finding rows, preserving the audit trail across pushes.
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

-- finding_events is the append-only audit log for all finding lifecycle transitions.
-- Every mutation to a finding (creation, posting, addressing, suppression, confidence
-- adjustment, tier change) is recorded here, not just reactions.
--
-- M1 scope: reaction events (thumbs_up/down) are required for the eval feedback loop.
-- Full lifecycle auditing (confidence_adjusted, tier_changed, etc.) ships in M1 because
-- the write cost is negligible and it's harder to backfill than to capture from day one.
-- Query/reporting UIs for the audit log are M2+ / enterprise scope.
CREATE TABLE finding_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    finding_id  UUID NOT NULL REFERENCES findings(id) ON DELETE RESTRICT,
    event_type  TEXT NOT NULL CHECK (event_type IN (
        -- Reactions (from GitHub polling)
        'thumbs_up', 'thumbs_down',
        -- Lifecycle transitions
        'created', 'posted', 'suppressed',
        'addressed', 'dismissed', 'resolved',
        -- Classification changes (confidence penalty, tier downgrade)
        'confidence_adjusted', 'tier_changed'
    )),
    actor       TEXT NOT NULL,            -- GitHub username, or 'mimir' for system events
    old_value   TEXT,                     -- previous state (e.g. old confidence score, old tier)
    new_value   TEXT,                     -- new state
    metadata    JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
    -- No table-level UNIQUE constraint. Lifecycle events (created, posted, addressed, etc.)
    -- can repeat — a finding can transition through the same state more than once.
    -- Reaction uniqueness is enforced by a partial unique index below.
);

-- Partial unique index: one reaction per (finding, event_type, actor).
-- Lifecycle events are unconstrained — multiple 'addressed' or 'suppressed' events
-- for the same finding are expected and preserved for audit.
CREATE UNIQUE INDEX finding_events_reaction_unique
    ON finding_events (finding_id, event_type, actor)
    WHERE event_type IN ('thumbs_up', 'thumbs_down');

CREATE INDEX finding_events_finding_id_idx ON finding_events (finding_id);
CREATE INDEX finding_events_type_idx ON finding_events (event_type);
```

### River Tables

River manages its own tables (`river_job`, `river_leader`, `river_queue`, etc.) via its built-in migrator. Do not create or modify these manually. River's migration runs at startup:

```go
migrator, _ := rivermigrate.New(riverpgxv5.New(pool), nil)
migrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
```

---

## sqlc Configuration

```yaml
# sqlc.yaml
version: "2"
sql:
  - engine: "postgresql"
    queries: "internal/store/queries/"
    schema: "internal/store/migrations/"
    gen:
      go:
        package: "store"
        out: "internal/store/generated"
        sql_package: "pgx/v5"
        emit_json_tags: true
        emit_pointers_for_null_types: true
```

### Query Files

Organized by domain entity:

```
internal/store/queries/
├── pull_requests.sql
├── review_tasks.sql
├── findings.sql
├── dismissed_fingerprints.sql
└── finding_events.sql
```

### Key Queries

**pull_requests.sql:**
```sql
-- name: UpsertPullRequest :one
INSERT INTO pull_requests (external_pr_id, repo_full_name, pr_number, head_sha, base_sha, author, state, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (external_pr_id, head_sha) DO UPDATE
SET state = EXCLUDED.state, metadata = EXCLUDED.metadata, updated_at = now()
RETURNING *;

-- name: GetPullRequest :one
SELECT * FROM pull_requests WHERE id = $1 AND deleted_at IS NULL;

-- name: GetPullRequestByGitHubID :one
SELECT * FROM pull_requests
WHERE external_pr_id = $1 AND deleted_at IS NULL
ORDER BY created_at DESC
LIMIT 1;

-- name: SoftDeletePullRequest :execrows
UPDATE pull_requests SET deleted_at = now(), updated_at = now()
WHERE id = $1 AND deleted_at IS NULL;
```

**pipeline_runs.sql:**
```sql
-- name: CreatePipelineRun :one
INSERT INTO pipeline_runs (pull_request_id, head_sha, prompt_version, config_hash, metadata)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: CompletePipelineRun :execrows
UPDATE pipeline_runs
SET status = $2, tasks_total = $3, tasks_completed = $4, tasks_failed = $5,
    findings_total = $6, findings_posted = $7, findings_suppressed = $8,
    completed_at = now()
WHERE id = $1;

-- name: GetPipelineRun :one
SELECT * FROM pipeline_runs WHERE id = $1;

-- name: ListPipelineRunsForPR :many
SELECT * FROM pipeline_runs WHERE pull_request_id = $1 ORDER BY started_at DESC;

-- name: ReconcileStalePipelineRuns :exec
-- Mark orphaned 'running' pipeline runs as failed. Called at startup and
-- periodically. A pipeline_run is stale if it has been 'running' longer
-- than the maximum job timeout (default: 2× MIMIR_JOB_TIMEOUT = 10 min).
-- This preserves the audit trail — the row is not deleted, but transitioned
-- to 'failed' with a reason recorded in metadata.
UPDATE pipeline_runs
SET status = 'failed',
    completed_at = now(),
    metadata = metadata || '{"failure_reason": "abandoned: process crash or timeout"}'::jsonb
WHERE status = 'running'
  AND started_at < now() - $1::interval;
```

**review_tasks.sql:**
```sql
-- name: CreateReviewTask :one
INSERT INTO review_tasks (pull_request_id, pipeline_run_id, task_type, file_path, symbol, risk_score, model_id, diff_hunk)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: UpdateReviewTaskStatus :execrows
UPDATE review_tasks
SET status = $2, error = $3, started_at = CASE WHEN $2 = 'running' THEN now() ELSE started_at END,
    completed_at = CASE WHEN $2 IN ('completed', 'failed') THEN now() ELSE completed_at END
WHERE id = $1;

-- name: ListReviewTasksForPR :many
SELECT * FROM review_tasks WHERE pull_request_id = $1;

-- name: ListReviewTasksForRun :many
SELECT * FROM review_tasks WHERE pipeline_run_id = $1;

-- name: CountCompletedAndTotal :one
SELECT
    COUNT(*) AS total,
    COUNT(*) FILTER (WHERE status = 'completed') AS completed,
    COUNT(*) FILTER (WHERE status = 'failed') AS failed
FROM review_tasks
WHERE pipeline_run_id = $1;
```

**findings.sql:**
```sql
-- name: CreateFinding :one
-- INSERT only — no upsert. Within-run dedup is handled in-memory by the runtime.
-- Cross-run dedup is handled at the posting level by the policy layer.
-- Every finding ever produced is preserved for audit.
INSERT INTO findings (
    review_task_id, pull_request_id, pipeline_run_id,
    repo_full_name, file_path, start_line, end_line, symbol,
    category, confidence_tier, confidence_score, severity,
    title, body, suggestion, location_hash, content_hash, head_sha,
    suppression_reason,
    model_id, prompt_tokens, completion_tokens, metadata
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)
RETURNING *;

-- name: ListFindingsForPR :many
SELECT * FROM findings WHERE pull_request_id = $1
ORDER BY CASE severity
    WHEN 'critical' THEN 1
    WHEN 'high'     THEN 2
    WHEN 'medium'   THEN 3
    WHEN 'low'      THEN 4
    WHEN 'info'     THEN 5
END ASC, confidence_score DESC;

-- name: ListFindingsForRun :many
SELECT * FROM findings WHERE pipeline_run_id = $1
ORDER BY CASE severity
    WHEN 'critical' THEN 1
    WHEN 'high'     THEN 2
    WHEN 'medium'   THEN 3
    WHEN 'low'      THEN 4
    WHEN 'info'     THEN 5
END ASC, confidence_score DESC;

-- name: MarkFindingPosted :execrows
UPDATE findings SET posted_at = now(), external_comment_id = $2, updated_at = now() WHERE id = $1;

-- name: MarkFindingAddressed :execrows
UPDATE findings SET addressed_status = $2, updated_at = now() WHERE id = $1;

-- name: FindPriorFinding :one
-- Look across all rows for this logical PR (same external_pr_id), not just one
-- pull_request_id. A force-push creates a new pull_requests row with a different
-- UUID but the same external_pr_id, and we must still find prior findings.
-- NOTE: This intentionally does NOT filter on pull_requests.deleted_at.
-- Soft-deleted PRs' findings are still valid for dedup — suppressing a duplicate
-- against a soft-deleted PR's finding is correct behavior. The finding existed,
-- the code was reviewed, and re-flagging it adds noise without value.
SELECT f.id, f.location_hash, f.content_hash, f.head_sha FROM findings f
JOIN pull_requests pr ON pr.id = f.pull_request_id
WHERE pr.external_pr_id = (SELECT p.external_pr_id FROM pull_requests p WHERE p.id = $1)
  AND f.location_hash = $2
  AND f.addressed_status = 'unaddressed'
ORDER BY f.created_at DESC
LIMIT 1;

-- name: ListUnaddressedFindingsForPR :many
SELECT * FROM findings
WHERE pull_request_id = $1
  AND addressed_status = 'unaddressed'
  AND content_hash IS NOT NULL;

-- name: ListUnpostedFindings :many
-- Finds findings that were persisted but never posted to GitHub.
-- Used by the PostingRetryJob to recover from GitHub API failures.
-- Scoped by pipeline_run_id per the "scope every finding query" boundary rule.
-- Only considers findings from completed pipeline runs (not in-progress ones)
-- that are not suppressed and have no external_comment_id.
SELECT f.* FROM findings f
JOIN pipeline_runs pr ON pr.id = f.pipeline_run_id
WHERE f.pipeline_run_id = $1
  AND f.posted_at IS NULL
  AND f.suppression_reason IS NULL
  AND f.external_comment_id IS NULL
  AND pr.status = 'completed'
ORDER BY f.created_at ASC;
```

**dismissed_fingerprints.sql:**
```sql
-- name: IsFingerprintDismissed :one
SELECT EXISTS(
    SELECT 1 FROM dismissed_fingerprints
    WHERE fingerprint = $1 AND repo_full_name = $2
) AS dismissed;

-- name: DismissFingerprint :exec
-- Upsert: if the same fingerprint is dismissed again (different user, different reason),
-- update the row and preserve the new actor's intent. The prior dismissal is not lost —
-- the original created_at is preserved, and the caller records a finding_event for
-- the re-dismissal so the audit trail captures both actors.
INSERT INTO dismissed_fingerprints (fingerprint, repo_full_name, dismissed_by, reason)
VALUES ($1, $2, $3, $4)
ON CONFLICT (fingerprint, repo_full_name) DO UPDATE
SET dismissed_by = EXCLUDED.dismissed_by,
    reason = EXCLUDED.reason,
    updated_at = now();
```

**finding_events.sql:**
```sql
-- name: CreateFindingEvent :exec
-- For reaction events, the partial unique index (finding_events_reaction_unique)
-- deduplicates via ON CONFLICT — one thumbs_up per actor per finding.
-- For lifecycle events, no unique constraint exists, so every insert succeeds.
-- This preserves the full audit trail: a finding can be addressed, un-addressed,
-- and re-addressed, with each transition recorded.
INSERT INTO finding_events (finding_id, event_type, actor, old_value, new_value, metadata)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (finding_id, event_type, actor)
    WHERE event_type IN ('thumbs_up', 'thumbs_down')
    DO NOTHING;

-- name: CountReactionsForFinding :one
SELECT
    COUNT(*) FILTER (WHERE event_type = 'thumbs_up') AS thumbs_up,
    COUNT(*) FILTER (WHERE event_type = 'thumbs_down') AS thumbs_down
FROM finding_events
WHERE finding_id = $1;

-- name: ListEventsForFinding :many
SELECT * FROM finding_events WHERE finding_id = $1 ORDER BY created_at ASC;
```

---

## StoreAdapter Implementation

```go
type PostgresStore struct {
    pool    *pgxpool.Pool
    queries *generated.Queries
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
    return &PostgresStore{
        pool:    pool,
        queries: generated.New(pool),
    }
}
```

### Transaction Support

Operations that must be atomic (e.g., webhook receive: upsert PR + enqueue River job) require a transaction-bound store. The callback receives both a `StoreAdapter` that executes queries on the transaction and the raw `pgx.Tx` for River's `InsertTx`:

```go
func (s *PostgresStore) WithTx(ctx context.Context, fn func(txStore adapter.StoreAdapter, tx pgx.Tx) error) error {
    tx, err := s.pool.Begin(ctx)
    if err != nil {
        return fmt.Errorf("begin tx: %w", err)
    }
    defer tx.Rollback(ctx)

    // Construct a store bound to the transaction, not the pool.
    // All queries executed through txStore run on the same transaction.
    txStore := &PostgresStore{
        pool:    s.pool,           // kept for reference, but queries use tx
        queries: generated.New(tx), // sqlc accepts pgx.Tx (implements DBTX)
    }

    if err := fn(txStore, tx); err != nil {
        return err
    }
    return tx.Commit(ctx)
}
```

River's `InsertTx` method accepts the raw `pgx.Tx`, ensuring job enqueue is atomic with PR persistence. The `txStore` ensures all store operations within the callback participate in the same transaction.

---

## Connection Pool Configuration

```go
poolConfig, _ := pgxpool.ParseConfig(cfg.DatabaseURL)
poolConfig.MaxConns = 20                          // default; tune per instance
poolConfig.MinConns = 2
poolConfig.HealthCheckPeriod = 30 * time.Second
poolConfig.MaxConnLifetime = 30 * time.Minute
poolConfig.MaxConnIdleTime = 5 * time.Minute
```

These defaults are for a single `mimir serve` instance. In multi-instance deployments, `MaxConns` should be lowered to avoid exhausting PostgreSQL's `max_connections`. Document this in the config spec.

---

## Data Durability & Operational Requirements

### Backup Strategy

Production deployments **must** run PostgreSQL with:

1. **`synchronous_commit = on` (the default).** Do not disable this. Mimir's durability model depends on committed transactions surviving a crash. With `synchronous_commit = off`, a crash between commit acknowledgment and WAL flush silently loses the transaction — including webhook-enqueued jobs and persisted findings. This is a hard requirement for "no data loss."
2. **WAL archiving enabled.** Continuous archiving to object storage (S3, GCS, or equivalent). This enables point-in-time recovery (PITR) to any second within the retention window.
3. **Periodic base backups.** At least daily via `pg_basebackup` or equivalent. Base backups + WAL archive = full PITR capability.
4. **Retention period.** Minimum 30 days of WAL archives. Findings are long-lived audit data; the retention period should match your compliance requirements.

These are PostgreSQL operational requirements, not Mimir application requirements. Mimir does not implement its own backup logic. At startup, `mimir serve` logs a warning if it detects `synchronous_commit = off` via `SHOW synchronous_commit`.

### Deletion Policy

All foreign keys use `ON DELETE RESTRICT`. Deleting a `pull_requests` row fails if any `review_tasks`, `findings`, or `pipeline_runs` reference it. This is intentional — audit data is never silently destroyed.

To remove old data:

1. **Soft delete** the `pull_requests` row (`SoftDeletePullRequest` query sets `deleted_at`). All downstream data remains intact but the PR is excluded from active queries.
2. For hard deletion (e.g., GDPR compliance), delete in reverse dependency order: `finding_events` → `findings` → `review_tasks` → `pipeline_runs` → `pull_requests`. This must be an explicit, deliberate operation — never automated without review.

### No Cascade, No Silent Loss

The `ON DELETE RESTRICT` constraint on every FK is a guardrail against accidental data loss. If a migration, admin script, or future cleanup job attempts to delete a parent row with children, PostgreSQL will reject the operation with a clear error. This is the correct failure mode for audit-critical data.
