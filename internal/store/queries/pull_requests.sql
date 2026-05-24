-- name: UpsertPullRequest :one
INSERT INTO pull_requests (external_pr_id, repo_full_name, pr_number, head_sha, base_sha, author, state, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (external_pr_id, head_sha) DO UPDATE
SET state = EXCLUDED.state, metadata = EXCLUDED.metadata, deleted_at = NULL, updated_at = now()
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
