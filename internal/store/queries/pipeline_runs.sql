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
UPDATE pipeline_runs
SET status = 'failed',
    completed_at = now(),
    metadata = metadata || '{"failure_reason": "abandoned: process crash or timeout"}'::jsonb
WHERE status = 'running'
  AND started_at < now() - $1::interval;
