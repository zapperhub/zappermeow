-- The connection trail: what happened to an instance and why.

-- name: AppendConnectionEvent :one
INSERT INTO connection_events (instance_id, type, reason, detail)
VALUES ($1, $2, $3, $4)
RETURNING id, occurred_at;

-- name: ListConnectionEvents :many
-- Newest first, keyset paginated on (occurred_at, id). Using the id as the tie
-- breaker keeps the cursor stable when several events share a timestamp, which
-- is common during a burst of reconnections.
SELECT id, instance_id, type, reason, detail, occurred_at
FROM connection_events
WHERE instance_id = $1
  AND (sqlc.narg(before_id)::bigint IS NULL OR id < sqlc.narg(before_id)::bigint)
  AND (sqlc.narg(types)::text[] IS NULL OR type = ANY(sqlc.narg(types)::text[]))
ORDER BY occurred_at DESC, id DESC
LIMIT sqlc.arg(max_rows);

-- name: DeleteConnectionEventsBefore :execrows
-- Retention sweep (FR-037). Runs under an advisory lock so only one worker
-- performs it per cycle.
DELETE FROM connection_events WHERE occurred_at < $1;

-- name: TryAdvisoryLock :one
-- Cheap cross-process mutex for periodic maintenance: whoever gets true runs
-- the sweep, everyone else skips it without blocking.
SELECT pg_try_advisory_lock($1) AS acquired;

-- name: AdvisoryUnlock :exec
SELECT pg_advisory_unlock($1);

-- name: LastPairedPhone :one
-- The number this instance was last paired to, read from the trail rather than
-- from the instance row: logout clears the identity, so the row cannot answer
-- "was this a different number?" on the next pairing (FR-016).
SELECT detail->>'phone_number' AS phone_number
FROM connection_events
WHERE instance_id = $1
  AND type = 'pairing_succeeded'
  AND detail->>'phone_number' IS NOT NULL
ORDER BY id DESC
LIMIT 1;
