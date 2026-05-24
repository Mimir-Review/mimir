-- name: CreateFindingEvent :exec
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
