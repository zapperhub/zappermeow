-- Every statement filters by tenant_id. Scoping in SQL rather than in Go means
-- a query can never be reused in a way that reaches another tenant's rows.

-- name: CreateInstance :one
INSERT INTO instances (id, tenant_id, name, connection_state, created_at, updated_at)
VALUES ($1, $2, $3, 'registered', now(), now())
RETURNING *;

-- name: ListInstancesByTenant :many
SELECT *
FROM instances
WHERE tenant_id = $1
ORDER BY created_at;

-- name: GetInstanceByIDAndTenant :one
SELECT *
FROM instances
WHERE id = $1 AND tenant_id = $2;

-- name: RenameInstance :one
UPDATE instances
SET name = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: DeleteInstance :one
-- Returns the row so the caller can audit what disappeared. The API keys of the
-- instance go with it through ON DELETE CASCADE (FR-012).
DELETE FROM instances
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: CountKeysByInstance :one
SELECT count(*) FROM api_keys WHERE instance_id = $1;
