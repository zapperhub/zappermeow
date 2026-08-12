DROP TABLE IF EXISTS security_events;
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS instances;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS tenants;

-- citext is left installed on purpose: other schemas in the same database may
-- depend on it, and dropping an extension is not reversible per-table.
