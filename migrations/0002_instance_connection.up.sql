-- Instance connection: pairing, session ownership and the connection trail.
-- Only API-owned tables live here; HyperMeow migrates its own schema through
-- Container.Upgrade, versioned separately in whatsmeow_version.

-- The 001 column `state` only ever held 'registered'. It becomes the full
-- connection state machine, so the vocabulary is renamed rather than doubled:
-- an instance has one state, not one registration state plus one connection
-- state that could disagree.
ALTER TABLE instances RENAME COLUMN state TO connection_state;
ALTER TABLE instances DROP CONSTRAINT IF EXISTS instances_state_check;
ALTER TABLE instances
    ADD CONSTRAINT instances_connection_state_check CHECK (
        connection_state IN (
            'registered',   -- no session material; pairing required
            'pairing',      -- pairing attempt in flight (QR or phone code)
            'connecting',   -- paired, trying to establish the connection
            'connected',    -- online
            'disconnected', -- offline by explicit command or permanent failure
            'logged_out',   -- session invalidated from the phone or by the server
            'banned'        -- temporary ban reported by WhatsApp
        )
    );

-- The user's intent, kept apart from the observed state: suspending a tenant
-- must be able to stop sessions without destroying what the user asked for.
ALTER TABLE instances
    ADD COLUMN connection_intent text NOT NULL DEFAULT 'stopped'
        CHECK (connection_intent IN ('active', 'stopped')),
    -- Full JID including the companion device suffix (5511999999999:11@s.whatsapp.net).
    ADD COLUMN wa_jid                 text,
    ADD COLUMN wa_lid                 text,
    -- E.164 without '+', deliberately NOT unique: WhatsApp is multi-device, so
    -- several instances may be distinct companion devices of the same number.
    ADD COLUMN phone_number           text,
    ADD COLUMN push_name              text,
    ADD COLUMN platform               text,
    ADD COLUMN business_name          text,
    ADD COLUMN paired_at              timestamptz,
    ADD COLUMN connected_at           timestamptz,
    ADD COLUMN last_disconnect_at     timestamptz,
    ADD COLUMN last_disconnect_reason text,
    ADD COLUMN ban_expires_at         timestamptz;

-- Two instances sharing a phone number is legitimate; two instances sharing a
-- device JID is the same session in two places, which corrupts Signal state.
-- Last line of defence in the database, below any application bug.
CREATE UNIQUE INDEX instances_wa_jid_key ON instances (wa_jid) WHERE wa_jid IS NOT NULL;

-- Answers "which other instances share this number" without a scan.
CREATE INDEX instances_tenant_phone_idx ON instances (tenant_id, phone_number)
    WHERE phone_number IS NOT NULL;

-- Single source of truth on who owns each session. One row per instance,
-- created lazily on the first connect command.
CREATE TABLE session_leases (
    instance_id   uuid PRIMARY KEY REFERENCES instances (id) ON DELETE CASCADE,
    -- Owning process identity; NULL means the lease is free.
    worker_id     text,
    -- gRPC address of the owner on the private network.
    grpc_addr     text,
    -- Fencing token: incremented on every acquisition, never reused.
    generation    bigint NOT NULL DEFAULT 0,
    heartbeat_at  timestamptz,
    -- Effective state the worker obeys, derived from the instance intent and
    -- the tenant status. Defaults to 'stopped' so a row created by any path
    -- other than an explicit command never starts a session on its own.
    desired_state text NOT NULL DEFAULT 'stopped'
        CHECK (desired_state IN ('running', 'stopped', 'draining')),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- Sustains the reconciliation sweep looking for orphaned running leases.
CREATE INDEX session_leases_desired_heartbeat_idx
    ON session_leases (desired_state, heartbeat_at);

-- Queryable trail of connection transitions, with bounded retention.
CREATE TABLE connection_events (
    id          bigserial PRIMARY KEY,
    instance_id uuid NOT NULL REFERENCES instances (id) ON DELETE CASCADE,
    type        text NOT NULL CHECK (
        type IN (
            'pairing_started', 'pairing_succeeded', 'pairing_expired', 'pairing_failed',
            'connected', 'disconnected', 'logged_out', 'banned', 'number_changed',
            'lease_acquired', 'lease_lost', 'deleted'
        )
    ),
    reason      text,
    -- Auxiliary data only. Never session material, tokens or QR codes.
    detail      jsonb,
    occurred_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX connection_events_instance_idx
    ON connection_events (instance_id, occurred_at DESC, id DESC);

-- Supports the retention sweep.
CREATE INDEX connection_events_occurred_at_idx ON connection_events (occurred_at);
