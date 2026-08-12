-- name: CreateTenant :one
INSERT INTO tenants (id, name, status, created_at, updated_at)
VALUES ($1, $2, 'active', now(), now())
RETURNING id, name, status, created_at, updated_at;

-- name: GetTenantByID :one
SELECT id, name, status, created_at, updated_at
FROM tenants
WHERE id = $1;

-- name: ListTenants :many
SELECT id, name, status, created_at, updated_at
FROM tenants
ORDER BY created_at;

-- name: UpdateTenantName :one
UPDATE tenants
SET name = $2, updated_at = now()
WHERE id = $1
RETURNING id, name, status, created_at, updated_at;

-- name: SetTenantStatus :one
-- Suspension and activation are idempotent: applying the current status again
-- succeeds and simply touches updated_at.
UPDATE tenants
SET status = $2, updated_at = now()
WHERE id = $1
RETURNING id, name, status, created_at, updated_at;

-- name: DeleteTenantByID :one
-- Irreversible. Users, instances and API keys follow through ON DELETE CASCADE
-- in this single statement's transaction (FR-007).
DELETE FROM tenants
WHERE id = $1
RETURNING id, name, status, created_at, updated_at;

-- name: CountTenantCascade :one
-- Counted before deletion so the audit record can state exactly what went with
-- the tenant.
SELECT
    (SELECT count(*) FROM users u WHERE u.tenant_id = $1) AS user_count,
    (SELECT count(*) FROM instances i WHERE i.tenant_id = $1) AS instance_count,
    (SELECT count(*) FROM api_keys k JOIN instances i2 ON i2.id = k.instance_id WHERE i2.tenant_id = $1) AS api_key_count;
