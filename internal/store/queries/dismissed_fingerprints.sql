-- name: IsFingerprintDismissed :one
SELECT EXISTS(
    SELECT 1 FROM dismissed_fingerprints
    WHERE fingerprint = $1 AND repo_full_name = $2
) AS dismissed;

-- name: DismissFingerprint :exec
INSERT INTO dismissed_fingerprints (fingerprint, repo_full_name, dismissed_by, reason)
VALUES ($1, $2, $3, $4)
ON CONFLICT (fingerprint, repo_full_name) DO UPDATE
SET dismissed_by = EXCLUDED.dismissed_by,
    reason = EXCLUDED.reason,
    updated_at = now();
