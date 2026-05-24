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
