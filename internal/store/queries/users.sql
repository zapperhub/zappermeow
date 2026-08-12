-- name: CreateUser :one
INSERT INTO users (
    id, name, email, password_hash, role, tenant_id, must_change_password,
    password_changed_at, created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, now(), now(), now()
)
RETURNING id, name, email, role, tenant_id, must_change_password,
          failed_login_count, locked_until, password_changed_at, created_at, updated_at;

-- name: GetUserCredentialByEmail :one
-- The only query that returns password_hash: it serves the login path alone.
-- The tenant status rides along so a single indexed lookup decides both
-- authentication and the suspension cascade.
SELECT u.id, u.name, u.email, u.password_hash, u.role, u.tenant_id,
       u.must_change_password, u.failed_login_count, u.locked_until,
       u.password_changed_at, u.created_at, u.updated_at,
       t.status AS tenant_status
FROM users u
LEFT JOIN tenants t ON t.id = u.tenant_id
WHERE u.email = $1;

-- name: GetUserByID :one
-- Per-request authorisation lookup. Deliberately excludes password_hash: the
-- hash must never reach the API layer.
SELECT u.id, u.name, u.email, u.role, u.tenant_id, u.must_change_password,
       u.failed_login_count, u.locked_until, u.password_changed_at,
       u.created_at, u.updated_at,
       t.status AS tenant_status
FROM users u
LEFT JOIN tenants t ON t.id = u.tenant_id
WHERE u.id = $1;

-- name: GetTenantAdminByTenantID :one
SELECT id, name, email, role, tenant_id, must_change_password,
       failed_login_count, locked_until, password_changed_at, created_at, updated_at
FROM users
WHERE tenant_id = $1 AND role = 'tenant_admin'
ORDER BY created_at
LIMIT 1;

-- name: CountSuperAdmins :one
SELECT count(*) FROM users WHERE role = 'super_admin';

-- name: LockBootstrap :exec
-- Transaction-scoped advisory lock so concurrently booting replicas cannot each
-- create a super-admin. Released automatically when the transaction ends.
SELECT pg_advisory_xact_lock(4_182_030_921);

-- name: RegisterFailedLogin :one
-- Durable, transactional lockout (FR-020). On reaching the configured failure
-- count the account is locked and the counter resets, so the next window starts
-- clean. Unlocking is passive: locked_until simply falls into the past.
UPDATE users
SET failed_login_count = CASE
        WHEN failed_login_count + 1 >= sqlc.arg(max_failures)::int THEN 0
        ELSE failed_login_count + 1
    END,
    locked_until = CASE
        WHEN failed_login_count + 1 >= sqlc.arg(max_failures)::int
        THEN now() + make_interval(secs => sqlc.arg(lock_seconds)::double precision)
        ELSE locked_until
    END,
    updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING failed_login_count, locked_until;

-- name: ClearLoginFailures :exec
UPDATE users
SET failed_login_count = 0, locked_until = NULL, updated_at = now()
WHERE id = $1;

-- name: UpdatePassword :exec
-- Sets a new password and stamps password_changed_at, which is what invalidates
-- every token issued before this moment (SC-004).
UPDATE users
SET password_hash = $2,
    must_change_password = $3,
    password_changed_at = now(),
    failed_login_count = 0,
    locked_until = NULL,
    updated_at = now()
WHERE id = $1;

-- name: GetUserPasswordHash :one
-- Reads the hash for the password-change flow, which must prove the caller
-- knows the current password.
SELECT password_hash FROM users WHERE id = $1;
