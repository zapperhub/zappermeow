-- Account foundation: tenants, users, instances, API keys and security events.
-- Only API-owned tables live here; HyperMeow migrates its own schema.

CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE tenants (
    id         uuid PRIMARY KEY,
    name       citext NOT NULL UNIQUE CHECK (char_length(name) BETWEEN 1 AND 120),
    status     text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id                   uuid PRIMARY KEY,
    name                 text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),
    email                citext NOT NULL UNIQUE,
    password_hash        text NOT NULL,
    role                 text NOT NULL CHECK (role IN ('super_admin', 'tenant_admin')),
    tenant_id            uuid REFERENCES tenants (id) ON DELETE CASCADE,
    must_change_password boolean NOT NULL DEFAULT false,
    failed_login_count   integer NOT NULL DEFAULT 0,
    locked_until         timestamptz,
    password_changed_at  timestamptz NOT NULL DEFAULT now(),
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    -- A super-admin belongs to the platform (no tenant); a tenant admin always
    -- belongs to exactly one tenant.
    CONSTRAINT users_role_tenant_ck CHECK (
        (role = 'super_admin' AND tenant_id IS NULL)
        OR (role = 'tenant_admin' AND tenant_id IS NOT NULL)
    )
);

CREATE INDEX users_tenant_id_idx ON users (tenant_id);

CREATE TABLE instances (
    id         uuid PRIMARY KEY,
    tenant_id  uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name       text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),
    -- Pairing/connection states arrive in a later feature by extending this CHECK.
    state      text NOT NULL DEFAULT 'registered' CHECK (state IN ('registered')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX instances_tenant_id_idx ON instances (tenant_id);
CREATE UNIQUE INDEX instances_tenant_id_name_uk ON instances (tenant_id, lower(name));

CREATE TABLE api_keys (
    id          uuid PRIMARY KEY,
    instance_id uuid NOT NULL REFERENCES instances (id) ON DELETE CASCADE,
    label       text CHECK (label IS NULL OR char_length(label) <= 60),
    -- First 12 characters of the token (zmk_xxxxxxxx), safe to display.
    key_prefix  text NOT NULL,
    -- SHA-256 of the full token; the plaintext secret is never stored.
    secret_hash bytea NOT NULL UNIQUE,
    status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    revoked_at  timestamptz
);

CREATE INDEX api_keys_instance_id_idx ON api_keys (instance_id);

-- Append-only audit trail. No UPDATE/DELETE is ever issued against this table.
CREATE TABLE security_events (
    id            uuid PRIMARY KEY,
    event_type    text NOT NULL,
    -- No FK: events outlive the actor they refer to, and failed logins for an
    -- unknown email have no actor at all.
    actor_user_id uuid,
    target_type   text,
    target_id     uuid,
    result        text NOT NULL CHECK (result IN ('success', 'failure', 'denied')),
    source_ip     inet,
    metadata      jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX security_events_created_at_idx ON security_events (created_at);
CREATE INDEX security_events_event_type_created_at_idx ON security_events (event_type, created_at);
CREATE INDEX security_events_actor_user_id_created_at_idx ON security_events (actor_user_id, created_at);
