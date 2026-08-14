-- Connection extras: per-instance egress proxy, passive mode and the trail
-- vocabulary for the stream events, proxy changes and the passkey pairing step.

-- Egress proxy for every connection this instance makes (websocket and media).
-- NULL means direct. The URL carries its credentials because the library needs
-- to use them on every connect — hashing is impossible. It never leaves the
-- domain layer unmasked; see domain.MaskProxyURL.
--
-- The scheme/host validation lives in the application (domain.ValidateProxyURL)
-- rather than in a CHECK: expressing URL parsing in SQL would duplicate the
-- rule, and two copies of a rule eventually disagree.
ALTER TABLE instances
    ADD COLUMN proxy_url    text,
    -- Desired passive mode. Reapplied after every Connected, because the
    -- library unconditionally restores active mode on each connection.
    ADD COLUMN passive_mode boolean NOT NULL DEFAULT false;

-- The trail vocabulary grows with this feature. The CHECK is recreated rather
-- than relaxed: a closed set is what keeps the trail queryable.
ALTER TABLE connection_events DROP CONSTRAINT connection_events_type_check;
ALTER TABLE connection_events
    ADD CONSTRAINT connection_events_type_check CHECK (
        type IN (
            'pairing_started', 'pairing_succeeded', 'pairing_expired', 'pairing_failed',
            'connected', 'disconnected', 'logged_out', 'banned', 'number_changed',
            'lease_acquired', 'lease_lost', 'deleted',
            -- 003
            'stream_error',           -- stream closed with an unknown code
            'manual_login_reconnect', -- server asked the client to reconnect itself
            'proxy_updated',          -- tenant set, changed or removed the proxy
            'passive_mode_updated',   -- tenant toggled passive mode
            'passkey_challenge',      -- WhatsApp required the passkey step
            'passkey_responded',      -- authenticator assertion forwarded
            'passkey_confirmed'       -- confirmation sent (by the tenant or automatically)
        )
    );
