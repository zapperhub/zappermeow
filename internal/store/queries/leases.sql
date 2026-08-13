-- Session ownership. Every statement here exists to uphold one invariant: a
-- session is connected in exactly one process at a time (Principle III).

-- name: EnsureLease :exec
-- Creates the lease row on the first connect command. desired_state defaults to
-- 'stopped', so the row alone never starts a session — SetDesiredState does.
INSERT INTO session_leases (instance_id)
VALUES ($1)
ON CONFLICT (instance_id) DO NOTHING;

-- name: SetDesiredState :exec
UPDATE session_leases
SET desired_state = $2, updated_at = now()
WHERE instance_id = $1;

-- name: SetTenantDesiredState :exec
-- Bulk projection used when a tenant is suspended or reactivated. The user's
-- intent lives on the instance and is untouched here, so reactivation can
-- restore exactly what was running before.
UPDATE session_leases l
SET desired_state = $2, updated_at = now()
FROM instances i
WHERE l.instance_id = i.id AND i.tenant_id = $1;

-- name: AcquireLease :one
-- The atomic heart of exclusive ownership. Concurrent workers all run this
-- statement; the WHERE clause means at most one row is updated, so exactly one
-- caller gets a RETURNING row and becomes the owner. The generation increments
-- on every acquisition and is never reused, which is what lets the worker
-- reject commands issued to a previous owner.
UPDATE session_leases
SET worker_id = $2,
    grpc_addr = $3,
    generation = generation + 1,
    heartbeat_at = now(),
    updated_at = now()
WHERE instance_id = $1
  AND desired_state = 'running'
  AND (worker_id IS NULL OR heartbeat_at < now() - sqlc.arg(expiry)::interval)
RETURNING generation;

-- name: HeartbeatLeases :many
-- One statement for every session a worker owns. Returns the instances that
-- were actually renewed: anything missing was stolen or stopped, and the worker
-- must drop those sessions before another process picks them up.
UPDATE session_leases
SET heartbeat_at = now(), updated_at = now()
WHERE worker_id = $1 AND desired_state = 'running'
RETURNING instance_id, generation;

-- name: ReleaseLease :exec
-- Graceful handover: clears ownership while preserving generation and
-- desired_state, so another worker adopts the session in seconds instead of
-- waiting for the expiry.
UPDATE session_leases
SET worker_id = NULL, grpc_addr = NULL, heartbeat_at = NULL, updated_at = now()
WHERE instance_id = $1 AND worker_id = $2;

-- name: ReleaseWorkerLeases :exec
UPDATE session_leases
SET worker_id = NULL, grpc_addr = NULL, heartbeat_at = NULL, updated_at = now()
WHERE worker_id = $1;

-- name: ListAdoptableLeases :many
-- Free or expired leases that should be running, excluding instances parked on
-- a permanent failure: retrying those is pointless until a user command clears
-- the reason (FR-029, FR-031).
SELECT l.instance_id, l.generation
FROM session_leases l
JOIN instances i ON i.id = l.instance_id
WHERE l.desired_state = 'running'
  AND (l.worker_id IS NULL OR l.heartbeat_at < now() - sqlc.arg(expiry)::interval)
  AND (i.last_disconnect_reason IS NULL OR i.last_disconnect_reason <> ALL(sqlc.arg(permanent_reasons)::text[]))
ORDER BY l.updated_at
LIMIT sqlc.arg(max_rows);

-- name: GetLease :one
SELECT instance_id, worker_id, grpc_addr, generation, heartbeat_at, desired_state
FROM session_leases
WHERE instance_id = $1;

-- name: GetLeaseOwner :one
-- Used by the API before dialling: a stale heartbeat means the address in the
-- row points at a process that no longer owns the session.
SELECT grpc_addr, generation,
       (worker_id IS NOT NULL AND heartbeat_at >= now() - sqlc.arg(expiry)::interval) AS is_live
FROM session_leases
WHERE instance_id = $1;

-- name: CountLeasesByWorker :one
SELECT count(*) FROM session_leases WHERE worker_id = $1;
