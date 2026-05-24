-- name: CreateFinding :one
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
-- Scoped by pipeline_run_id per CLAUDE.md boundary rule.
SELECT f.* FROM findings f
JOIN pipeline_runs pr ON pr.id = f.pipeline_run_id
WHERE f.pipeline_run_id = $1
  AND f.posted_at IS NULL
  AND f.suppression_reason IS NULL
  AND f.external_comment_id IS NULL
  AND pr.status = 'completed'
ORDER BY f.created_at ASC;
