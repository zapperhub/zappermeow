package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstanceStateValid(t *testing.T) {
	for _, s := range []InstanceState{
		InstanceRegistered, InstancePairing, InstanceConnecting,
		InstanceConnected, InstanceDisconnected, InstanceLoggedOut, InstanceBanned,
	} {
		assert.True(t, s.Valid(), "%q must be a known state", s)
	}

	for _, s := range []InstanceState{"", "registrada", "online", "REGISTERED"} {
		assert.False(t, s.Valid(), "%q must not be accepted", s)
	}
}

// The published vocabulary is English: the account foundation already shipped
// `state: "registered"`, and changing a published value is a breaking change.
func TestInstanceStateWireValues(t *testing.T) {
	assert.Equal(t, "registered", string(InstanceRegistered))
	assert.Equal(t, "logged_out", string(InstanceLoggedOut))
	assert.Equal(t, "active", string(IntentActive))
	assert.Equal(t, "stopped", string(IntentStopped))
}

func TestTerminalStates(t *testing.T) {
	assert.True(t, InstanceLoggedOut.Terminal())
	assert.True(t, InstanceBanned.Terminal())

	// Disconnected is not terminal by itself: the reason decides whether the
	// system keeps trying, which is why the reason is persisted next to it.
	assert.False(t, InstanceDisconnected.Terminal())
	assert.False(t, InstanceConnected.Terminal())
}

func TestCanTransition(t *testing.T) {
	tests := []struct {
		name  string
		from  InstanceState
		to    InstanceState
		allow bool
	}{
		{"registered pairs", InstanceRegistered, InstancePairing, true},
		{"pairing succeeds", InstancePairing, InstanceConnected, true},
		{"pairing expires back to registered", InstancePairing, InstanceRegistered, true},
		{"connected drops", InstanceConnected, InstanceConnecting, true},
		{"connected is logged out from the phone", InstanceConnected, InstanceLoggedOut, true},
		{"connected is banned", InstanceConnected, InstanceBanned, true},
		{"disconnected reconnects", InstanceDisconnected, InstanceConnecting, true},
		{"logged out pairs again", InstanceLoggedOut, InstancePairing, true},
		{"banned pairs again", InstanceBanned, InstancePairing, true},
		{"idempotent command stays put", InstanceConnected, InstanceConnected, true},

		// A registered instance has no session material, so it cannot connect
		// without pairing first — this is the guard that keeps a reconnect
		// command from silently doing nothing.
		{"registered cannot connect directly", InstanceRegistered, InstanceConnected, false},
		{"registered cannot be banned", InstanceRegistered, InstanceBanned, false},
		{"logged out cannot connect without pairing", InstanceLoggedOut, InstanceConnected, false},
		{"unknown source", InstanceState("bogus"), InstanceConnected, false},
		{"unknown target", InstanceConnected, InstanceState("bogus"), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.allow, CanTransition(tc.from, tc.to))
		})
	}
}

func TestPaired(t *testing.T) {
	assert.False(t, Instance{}.Paired(), "an instance without a device is not paired")
	assert.False(t, Instance{Device: &DeviceIdentity{}}.Paired(), "an empty JID is not a device")
	assert.True(t, Instance{Device: &DeviceIdentity{JID: "5511999999999:11@s.whatsapp.net"}}.Paired())
}

func TestShouldAutoReconnect(t *testing.T) {
	tests := []struct {
		name   string
		intent ConnectionIntent
		reason DisconnectReason
		want   bool
	}{
		{"active after a network drop", IntentActive, ReasonNetwork, true},
		{"active after losing the worker", IntentActive, ReasonWorkerLost, true},
		{"active with nothing on record", IntentActive, ReasonNone, true},
		{"stopped by the user", IntentStopped, ReasonNone, false},

		// The intent stays active after an invalidation so reactivating is a
		// single explicit command, but the permanent reason blocks retries.
		{"active but logged out from the phone", IntentActive, ReasonLoggedOutFromPhone, false},
		{"active but banned", IntentActive, ReasonTemporaryBan, false},
		{"active but session replaced", IntentActive, ReasonSessionReplaced, false},
		{"active but tenant suspended", IntentActive, ReasonTenantSuspended, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst := Instance{Intent: tc.intent, LastDisconnectReason: tc.reason}
			assert.Equal(t, tc.want, inst.ShouldAutoReconnect())
		})
	}
}

func TestConnectionIntentValid(t *testing.T) {
	assert.True(t, IntentActive.Valid())
	assert.True(t, IntentStopped.Valid())
	assert.False(t, ConnectionIntent("ativa").Valid())
	assert.False(t, ConnectionIntent("").Valid())
}

func TestDeviceIdentityKeepsTheDeviceSuffix(t *testing.T) {
	// Two instances of the same number are distinct companion devices; the
	// suffix is what tells them apart, so it must survive round-trips.
	device := DeviceIdentity{
		JID:         "5511999999999:11@s.whatsapp.net",
		PhoneNumber: "5511999999999",
		PairedAt:    time.Now(),
	}
	inst := Instance{Device: &device}

	require.True(t, inst.Paired())
	assert.Contains(t, inst.Device.JID, ":11@", "the companion device suffix must not be stripped")
}
