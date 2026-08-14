-- Reverses 0003.

-- Rows carrying the vocabulary introduced here must go before the old CHECK is
-- restored, otherwise the constraint cannot be validated and the rollback
-- fails. Losing this slice of the trail is the expected cost of rolling back.
DELETE FROM connection_events
WHERE type IN (
    'stream_error', 'manual_login_reconnect', 'proxy_updated',
    'passive_mode_updated', 'passkey_challenge', 'passkey_responded', 'passkey_confirmed'
);

ALTER TABLE connection_events DROP CONSTRAINT IF EXISTS connection_events_type_check;
ALTER TABLE connection_events
    ADD CONSTRAINT connection_events_type_check CHECK (
        type IN (
            'pairing_started', 'pairing_succeeded', 'pairing_expired', 'pairing_failed',
            'connected', 'disconnected', 'logged_out', 'banned', 'number_changed',
            'lease_acquired', 'lease_lost', 'deleted'
        )
    );

ALTER TABLE instances
    DROP COLUMN IF EXISTS passive_mode,
    DROP COLUMN IF EXISTS proxy_url;
