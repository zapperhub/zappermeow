package domain

import "time"

// ConnectionEventType is the closed vocabulary of the connection trail
// (data-model.md §5). The database CHECK constraint mirrors this list, so
// adding a value here without a migration fails loudly at write time.
type ConnectionEventType string

const (
	ConnEventPairingStarted   ConnectionEventType = "pairing_started"
	ConnEventPairingSucceeded ConnectionEventType = "pairing_succeeded"
	ConnEventPairingExpired   ConnectionEventType = "pairing_expired"
	ConnEventPairingFailed    ConnectionEventType = "pairing_failed"
	ConnEventConnected        ConnectionEventType = "connected"
	ConnEventDisconnected     ConnectionEventType = "disconnected"
	ConnEventLoggedOut        ConnectionEventType = "logged_out"
	ConnEventBanned           ConnectionEventType = "banned"
	ConnEventNumberChanged    ConnectionEventType = "number_changed"
	ConnEventLeaseAcquired    ConnectionEventType = "lease_acquired"
	ConnEventLeaseLost        ConnectionEventType = "lease_lost"
	ConnEventDeleted          ConnectionEventType = "deleted"
)

// Valid reports whether t is a known event type.
func (t ConnectionEventType) Valid() bool {
	switch t {
	case ConnEventPairingStarted, ConnEventPairingSucceeded, ConnEventPairingExpired,
		ConnEventPairingFailed, ConnEventConnected, ConnEventDisconnected,
		ConnEventLoggedOut, ConnEventBanned, ConnEventNumberChanged,
		ConnEventLeaseAcquired, ConnEventLeaseLost, ConnEventDeleted:
		return true
	default:
		return false
	}
}

// DisconnectReason explains why a session stopped. The empty value means "no
// disconnection on record", which is the state of an instance that never
// connected.
type DisconnectReason string

const (
	// ReasonNone is the zero value: nothing to explain.
	ReasonNone DisconnectReason = ""

	// --- transient: the system keeps reconnecting ---

	// ReasonNetwork is a websocket closed by the server or by the network.
	ReasonNetwork DisconnectReason = "network"
	// ReasonKeepaliveTimeout is a keepalive ping that stopped answering.
	ReasonKeepaliveTimeout DisconnectReason = "keepalive_timeout"
	// ReasonWorkerLost is the owning process dying or losing its lease.
	ReasonWorkerLost DisconnectReason = "worker_lost"

	// --- permanent: retrying is useless or harmful ---

	// ReasonUserRequested is an explicit disconnect command.
	ReasonUserRequested DisconnectReason = "user_requested"
	// ReasonLoggedOutFromPhone is the device unlinked from the handset.
	ReasonLoggedOutFromPhone DisconnectReason = "logged_out_from_phone"
	// ReasonTemporaryBan is a ban reported by WhatsApp.
	ReasonTemporaryBan DisconnectReason = "temporary_ban"
	// ReasonSessionReplaced means the same device credentials were opened
	// elsewhere. With the lease working this must never happen: it is an alarm
	// about exclusive ownership (Principle III), not a routine event.
	ReasonSessionReplaced DisconnectReason = "session_replaced"
	// ReasonClientOutdated is the server rejecting this client version.
	ReasonClientOutdated DisconnectReason = "client_outdated"
	// ReasonConnectFailure is a connection failure with an unknown reason code.
	ReasonConnectFailure DisconnectReason = "connect_failure"
	// ReasonCATRefreshFailed is a failure refreshing the CAT credentials.
	ReasonCATRefreshFailed DisconnectReason = "cat_refresh_failed"
	// ReasonLogoutLocalOnly is a logout that could not reach the server, so the
	// device may still be listed on the customer's handset (research R10).
	ReasonLogoutLocalOnly DisconnectReason = "logout_local_only"
	// ReasonTenantSuspended is the tenant being suspended by the platform.
	ReasonTenantSuspended DisconnectReason = "tenant_suspended"
)

// permanentReasons are the causes after which automatic reconnection must stop.
// Hammering a logged-out or banned number does not recover it and makes the
// customer's situation worse, which is exactly what US5 exists to prevent.
var permanentReasons = map[DisconnectReason]struct{}{
	ReasonUserRequested:      {},
	ReasonLoggedOutFromPhone: {},
	ReasonTemporaryBan:       {},
	ReasonSessionReplaced:    {},
	ReasonClientOutdated:     {},
	ReasonConnectFailure:     {},
	ReasonCATRefreshFailed:   {},
	ReasonLogoutLocalOnly:    {},
	ReasonTenantSuspended:    {},
}

// Permanent reports whether the reason blocks automatic reconnection until an
// explicit command clears it.
func (r DisconnectReason) Permanent() bool {
	_, ok := permanentReasons[r]
	return ok
}

// Valid reports whether r is a known reason. ReasonNone is valid: it is how an
// instance that never disconnected is represented.
func (r DisconnectReason) Valid() bool {
	if r == ReasonNone {
		return true
	}
	if r.Permanent() {
		return true
	}
	switch r {
	case ReasonNetwork, ReasonKeepaliveTimeout, ReasonWorkerLost:
		return true
	default:
		return false
	}
}

// ConnectionEvent is one entry of an instance's connection trail.
type ConnectionEvent struct {
	ID         int64
	InstanceID ID
	Type       ConnectionEventType
	Reason     DisconnectReason
	// Detail carries auxiliary context such as a number change or a ban code.
	// It must never carry session material, tokens or QR codes (FR-043).
	Detail     map[string]any
	OccurredAt time.Time
}

// PermanentReasonList returns the permanent reasons as plain strings, for the
// SQL predicate that keeps reconciliation from adopting instances parked on a
// failure no retry can fix. Order is stable so query plans and tests are too.
func PermanentReasonList() []string {
	out := make([]string, 0, len(permanentReasons))
	for _, r := range []DisconnectReason{
		ReasonUserRequested, ReasonLoggedOutFromPhone, ReasonTemporaryBan,
		ReasonSessionReplaced, ReasonClientOutdated, ReasonConnectFailure,
		ReasonCATRefreshFailed, ReasonLogoutLocalOnly, ReasonTenantSuspended,
	} {
		out = append(out, string(r))
	}
	return out
}
