-- Reverses 0002. HyperMeow tables are not touched here: they are owned and
-- versioned by the library, and dropping them would destroy session material
-- that this migration never created.

DROP TABLE IF EXISTS connection_events;
DROP TABLE IF EXISTS session_leases;

DROP INDEX IF EXISTS instances_tenant_phone_idx;
DROP INDEX IF EXISTS instances_wa_jid_key;

ALTER TABLE instances
    DROP COLUMN IF EXISTS ban_expires_at,
    DROP COLUMN IF EXISTS last_disconnect_reason,
    DROP COLUMN IF EXISTS last_disconnect_at,
    DROP COLUMN IF EXISTS connected_at,
    DROP COLUMN IF EXISTS paired_at,
    DROP COLUMN IF EXISTS business_name,
    DROP COLUMN IF EXISTS platform,
    DROP COLUMN IF EXISTS push_name,
    DROP COLUMN IF EXISTS phone_number,
    DROP COLUMN IF EXISTS wa_lid,
    DROP COLUMN IF EXISTS wa_jid,
    DROP COLUMN IF EXISTS connection_intent;

-- Back to the 001 vocabulary. Any instance that had moved past registration
-- loses that history, which is the expected cost of rolling this feature back.
UPDATE instances SET connection_state = 'registered' WHERE connection_state <> 'registered';

ALTER TABLE instances DROP CONSTRAINT IF EXISTS instances_connection_state_check;
ALTER TABLE instances RENAME COLUMN connection_state TO state;
ALTER TABLE instances
    ADD CONSTRAINT instances_state_check CHECK (state IN ('registered'));
