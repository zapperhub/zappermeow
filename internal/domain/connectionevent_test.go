package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConnectionEventTypeValid(t *testing.T) {
	for _, ty := range []ConnectionEventType{
		ConnEventPairingStarted, ConnEventPairingSucceeded, ConnEventPairingExpired,
		ConnEventPairingFailed, ConnEventConnected, ConnEventDisconnected,
		ConnEventLoggedOut, ConnEventBanned, ConnEventNumberChanged,
		ConnEventLeaseAcquired, ConnEventLeaseLost, ConnEventDeleted,
	} {
		assert.True(t, ty.Valid(), "%q must be a known event type", ty)
	}

	for _, ty := range []ConnectionEventType{"", "connected_at", "pairing"} {
		assert.False(t, ty.Valid(), "%q must not be accepted", ty)
	}
}

// The split between transient and permanent is the whole of US5: getting it
// wrong means either hammering a banned number or giving up on a flaky network.
func TestDisconnectReasonPermanence(t *testing.T) {
	transient := []DisconnectReason{ReasonNetwork, ReasonKeepaliveTimeout, ReasonWorkerLost}
	permanent := []DisconnectReason{
		ReasonUserRequested, ReasonLoggedOutFromPhone, ReasonTemporaryBan,
		ReasonSessionReplaced, ReasonClientOutdated, ReasonConnectFailure,
		ReasonCATRefreshFailed, ReasonLogoutLocalOnly, ReasonTenantSuspended,
	}

	for _, r := range transient {
		assert.False(t, r.Permanent(), "%q must keep the system reconnecting", r)
		assert.True(t, r.Valid())
	}
	for _, r := range permanent {
		assert.True(t, r.Permanent(), "%q must stop automatic reconnection", r)
		assert.True(t, r.Valid())
	}

	// No reason on record must never look permanent, or a fresh instance would
	// be skipped by reconciliation and never come online.
	assert.False(t, ReasonNone.Permanent())
	assert.True(t, ReasonNone.Valid())
}

func TestDisconnectReasonValidRejectsUnknown(t *testing.T) {
	for _, r := range []DisconnectReason{"banido", "timeout", "unknown"} {
		assert.False(t, r.Valid(), "%q must not be accepted", r)
	}
}

// Every permanent reason has to be a known reason as well, otherwise a value
// could block reconnection while failing validation on the way to the database.
func TestPermanentReasonsAreValidReasons(t *testing.T) {
	for r := range permanentReasons {
		assert.True(t, r.Valid(), "%q is permanent but not valid", r)
	}
}
