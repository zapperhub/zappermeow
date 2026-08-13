-- name: CreateAPIKey :one
INSERT INTO api_keys (id, instance_id, label, key_prefix, secret_hash, status, created_at)
VALUES ($1, $2, $3, $4, $5, 'active', now())
RETURNING id, instance_id, label, key_prefix, status, created_at, revoked_at;

-- name: ListKeysByInstance :many
-- Never selects secret_hash: a listing has no business touching credential
-- material, and the full secret is unrecoverable by design (FR-011).
SELECT id, instance_id, label, key_prefix, status, created_at, revoked_at
FROM api_keys
WHERE instance_id = $1
ORDER BY created_at;

-- name: RevokeAPIKey :one
-- Scoped through the instance to the tenant, so a key of another tenant cannot
-- be revoked. Revocation is one-way and idempotent on an already revoked key.
UPDATE api_keys k
SET status = 'revoked', revoked_at = COALESCE(k.revoked_at, now())
FROM instances i
WHERE k.id = $1
  AND k.instance_id = i.id
  AND i.id = $2
  AND i.tenant_id = $3
RETURNING k.id, k.instance_id, k.label, k.key_prefix, k.status, k.created_at, k.revoked_at;

-- name: GetKeyForAuth :one
-- The whole operational authorisation chain in a single indexed lookup on the
-- unique secret_hash: the key, the instance it belongs to and the status of the
-- owning tenant. Status columns come back raw so the caller can tell "revoked"
-- apart from "wrong instance" apart from "suspended tenant".
SELECT k.id AS key_id, k.instance_id, k.label, k.key_prefix, k.status AS key_status,
       i.name AS instance_name, i.connection_state AS instance_state,
       t.id AS tenant_id, t.status AS tenant_status
FROM api_keys k
JOIN instances i ON i.id = k.instance_id
JOIN tenants t ON t.id = i.tenant_id
WHERE k.secret_hash = $1;

-- name: CountActiveAPIKeys :one
SELECT count(*) FROM api_keys WHERE status = 'active';
