-- Connection state of an instance. Statements that a tenant can reach filter by
-- tenant_id in SQL, so a query can never be reused in a way that crosses tenants.

-- name: SetConnectionIntent :one
-- The user's wish, changed only by an explicit command. Clearing
-- last_disconnect_reason is what re-enables automatic adoption after an
-- invalidation (FR-031), so it happens here and nowhere else.
UPDATE instances
SET connection_intent = $2,
    last_disconnect_reason = CASE WHEN sqlc.arg(clear_reason)::boolean THEN NULL ELSE last_disconnect_reason END,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: SetConnectionState :exec
UPDATE instances
SET connection_state = $2,
    connected_at = CASE WHEN $2 = 'connected' THEN now() ELSE NULL END,
    updated_at = now()
WHERE id = $1;

-- name: RecordDisconnect :exec
UPDATE instances
SET connection_state = $2,
    connected_at = NULL,
    last_disconnect_at = now(),
    last_disconnect_reason = $3,
    ban_expires_at = $4,
    updated_at = now()
WHERE id = $1;

-- name: SetDeviceIdentity :exec
-- Persists the paired companion device. The JID keeps its device suffix: it is
-- what tells two instances of the same number apart.
UPDATE instances
SET wa_jid = $2,
    wa_lid = $3,
    phone_number = $4,
    push_name = $5,
    platform = $6,
    business_name = $7,
    paired_at = now(),
    updated_at = now()
WHERE id = $1;

-- name: ClearDeviceIdentity :exec
-- Logout drops the identity but the trail keeps the history of what was paired.
UPDATE instances
SET wa_jid = NULL,
    wa_lid = NULL,
    phone_number = NULL,
    push_name = NULL,
    platform = NULL,
    business_name = NULL,
    paired_at = NULL,
    connected_at = NULL,
    ban_expires_at = NULL,
    connection_state = 'registered',
    updated_at = now()
WHERE id = $1;

-- name: GetInstanceConnection :one
SELECT * FROM instances WHERE id = $1 AND tenant_id = $2;

-- name: GetInstanceConnectionByID :one
-- Worker-side lookup: the owner acts on an instance it holds a lease for, and
-- the lease is already scoped to a single instance.
SELECT * FROM instances WHERE id = $1;

-- name: ListInstancesSharingNumber :many
-- Same number, same tenant, different instance: legitimate companion devices,
-- surfaced as context and never as a conflict (FR-018).
SELECT id FROM instances
WHERE tenant_id = $1 AND phone_number = $2 AND id <> $3
ORDER BY created_at;
