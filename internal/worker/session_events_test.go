package worker_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zapperhub/zappermeow/internal/domain"
)

// A stream error closed with a code the library does not know must be
// diagnosable afterwards: the trail carries the code, which is the only piece
// of the event worth keeping (research R9).
func TestStreamErrorIsRecordedWithItsCode(t *testing.T) {
	h := newHarness(t)
	session := h.connected(t)

	session.EmitStreamError("999")

	h.waitForTrail(t, string(domain.ConnEventDisconnected))

	row := h.instance(t)
	require.NotNil(t, row.LastDisconnectReason)
	assert.Equal(t, string(domain.ReasonStreamError), *row.LastDisconnectReason,
		"the cause must be specific, not a generic network drop")

	detail := h.trailDetail(t, string(domain.ConnEventDisconnected))
	assert.Equal(t, "999", detail["stream_error_code"])
}

// A stream error is not a logout. The device material must survive it, or the
// instance would need a new QR for what is a transient server hiccup.
func TestStreamErrorKeepsTheSessionMaterial(t *testing.T) {
	h := newHarness(t)
	session := h.connected(t)

	before := h.instance(t)
	require.NotNil(t, before.WaJid)

	session.EmitStreamError("777")
	h.waitForTrail(t, string(domain.ConnEventDisconnected))

	after := h.instance(t)
	require.NotNil(t, after.WaJid, "a stream error must never clear the pairing")
	assert.Equal(t, *before.WaJid, *after.WaJid)

	// The instance stays adoptable: reconciliation refuses to pick up anything
	// parked on a permanent reason, and this reason is deliberately not one.
	assert.False(t, domain.ReasonStreamError.Permanent())
	assert.NotContains(t, domain.PermanentReasonList(), *after.LastDisconnectReason)
}

// The server asking the client to reconnect is an instruction. The platform
// acts on it without the tenant doing anything, and says so in the trail.
func TestManualLoginReconnectIsRecordedAndActedOn(t *testing.T) {
	h := newHarness(t)
	session := h.connected(t)

	session.EmitManualLoginReconnect()

	h.waitForTrail(t, string(domain.ConnEventManualLoginReconnect))

	// The reconnect is scheduled, not performed inside the handler: the library
	// dispatches this event synchronously, so blocking there would stall it.
	require.Eventually(t, func() bool {
		names := session.CallNames()
		connects := 0
		for _, name := range names {
			if name == "Connect" {
				connects++
			}
		}
		return connects >= 2
	}, 20*time.Second, 25*time.Millisecond, "the platform must reconnect on its own (calls: %v)", session.CallNames())
}

// The event carries no reason of its own, and it must not be filed as a drop:
// "the server told us to reconnect" and "the connection failed" are different
// facts, and only one of them warrants investigation.
func TestManualLoginReconnectIsNotFiledAsADisconnect(t *testing.T) {
	h := newHarness(t)
	session := h.connected(t)

	session.EmitManualLoginReconnect()
	h.waitForTrail(t, string(domain.ConnEventManualLoginReconnect))

	assert.NotContains(t, h.trail(t), string(domain.ConnEventDisconnected))
}

// trailDetail reads the detail column of the most recent entry of a type.
func (h *harness) trailDetail(t *testing.T, eventType string) map[string]any {
	t.Helper()

	var raw []byte
	err := h.infra.Pool.QueryRow(h.ctx,
		`SELECT detail FROM connection_events
		  WHERE instance_id = $1 AND type = $2
		  ORDER BY id DESC LIMIT 1`, h.instanceID, eventType).Scan(&raw)
	require.NoError(t, err)

	detail := map[string]any{}
	require.NoError(t, json.Unmarshal(raw, &detail))
	return detail
}
