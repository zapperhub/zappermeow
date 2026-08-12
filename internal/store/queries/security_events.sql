-- name: InsertSecurityEvent :exec
-- Append-only audit record. Written in the same transaction as the action it
-- describes whenever that action writes, so the two cannot diverge (FR-021).
INSERT INTO security_events (
    id, event_type, actor_user_id, target_type, target_id, result, source_ip, metadata, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, now()
);

-- name: CountSecurityEventsByType :one
-- Support query for tests and operational spot checks.
SELECT count(*) FROM security_events WHERE event_type = $1;
