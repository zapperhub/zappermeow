package domain

import "time"

// InstanceState is the observed connection state of an instance.
//
// Values are English identifiers because that is what the account foundation
// already published (`state: "registered"`); the Portuguese names used in the
// specification are domain vocabulary for prose, not wire values.
type InstanceState string

const (
	// InstanceRegistered has no session material: pairing is required.
	InstanceRegistered InstanceState = "registered"
	// InstancePairing has a pairing attempt in flight (QR or phone code).
	InstancePairing InstanceState = "pairing"
	// InstanceConnecting is paired and trying to establish the connection.
	InstanceConnecting InstanceState = "connecting"
	// InstanceConnected is online.
	InstanceConnected InstanceState = "connected"
	// InstanceDisconnected is offline by explicit command, or by a permanent
	// failure that has no state of its own (see ReasonSessionReplaced and
	// friends in connectionevent.go).
	InstanceDisconnected InstanceState = "disconnected"
	// InstanceLoggedOut had its session invalidated from the phone or by the
	// server. Reconnecting requires pairing again.
	InstanceLoggedOut InstanceState = "logged_out"
	// InstanceBanned is under a temporary ban reported by WhatsApp.
	InstanceBanned InstanceState = "banned"
)

// Valid reports whether s is a known state.
func (s InstanceState) Valid() bool {
	switch s {
	case InstanceRegistered, InstancePairing, InstanceConnecting,
		InstanceConnected, InstanceDisconnected, InstanceLoggedOut, InstanceBanned:
		return true
	default:
		return false
	}
}

// Terminal reports whether the state requires human action to leave: no amount
// of automatic reconnection recovers from it.
func (s InstanceState) Terminal() bool {
	return s == InstanceLoggedOut || s == InstanceBanned
}

// ConnectionIntent is what the user asked for, kept apart from what is observed.
// Suspending a tenant must be able to stop sessions without destroying the
// intent, so it can be restored on reactivation.
type ConnectionIntent string

const (
	// IntentActive means the instance should be online whenever possible.
	IntentActive ConnectionIntent = "active"
	// IntentStopped means the instance should stay offline until told otherwise.
	IntentStopped ConnectionIntent = "stopped"
)

// Valid reports whether i is a known intent.
func (i ConnectionIntent) Valid() bool {
	return i == IntentActive || i == IntentStopped
}

// allowedTransitions is the state machine of data-model.md §4. A transition
// missing from this map is a bug in the caller, not an unusual runtime event:
// the worker translates WhatsApp events through wa.Classify precisely so that
// only legal transitions ever reach here.
var allowedTransitions = map[InstanceState][]InstanceState{
	// Pairing is the only way out of a registered instance.
	InstanceRegistered: {InstancePairing},
	// A pairing attempt either succeeds, or falls back to where it came from.
	InstancePairing: {InstanceConnected, InstanceConnecting, InstanceRegistered, InstanceDisconnected},
	InstanceConnecting: {
		InstanceConnected, InstanceDisconnected, InstanceLoggedOut, InstanceBanned,
		// Re-pairing after the session material is dropped mid-reconnect.
		InstanceRegistered,
	},
	InstanceConnected: {InstanceConnecting, InstanceDisconnected, InstanceLoggedOut, InstanceBanned},
	// From offline, an explicit command either reconnects (paired) or pairs again.
	InstanceDisconnected: {InstanceConnecting, InstancePairing, InstanceRegistered, InstanceLoggedOut, InstanceBanned},
	// Terminal states leave only through an explicit command from the user.
	InstanceLoggedOut: {InstancePairing, InstanceRegistered},
	InstanceBanned:    {InstancePairing, InstanceConnecting, InstanceRegistered},
}

// CanTransition reports whether moving from one state to another is legal.
// Staying put is always legal: it is how idempotent commands behave (FR-008).
func CanTransition(from, to InstanceState) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	if from == to {
		return true
	}
	for _, allowed := range allowedTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// DeviceIdentity is the companion device an instance is paired to.
//
// An instance is one companion device, not one number: WhatsApp is multi-device,
// so several instances may legitimately point at the same PhoneNumber with
// different JIDs. The JID is what is unique.
type DeviceIdentity struct {
	// JID is the full identifier including the device suffix, e.g.
	// "5511999999999:11@s.whatsapp.net".
	JID          string
	LID          string
	PhoneNumber  string
	PushName     string
	Platform     string
	BusinessName string
	PairedAt     time.Time
}

// Instance is one companion device of a WhatsApp number. It is the unit of
// isolation: credentials belong to an instance, not to its tenant nor to the
// phone number.
type Instance struct {
	ID        ID
	TenantID  ID
	Name      string
	State     InstanceState
	Intent    ConnectionIntent
	CreatedAt time.Time
	UpdatedAt time.Time

	// Device is nil until the first successful pairing.
	Device *DeviceIdentity

	ConnectedAt          *time.Time
	LastDisconnectAt     *time.Time
	LastDisconnectReason DisconnectReason
	BanExpiresAt         *time.Time
}

// Paired reports whether the instance has session material to reconnect with.
func (i Instance) Paired() bool { return i.Device != nil && i.Device.JID != "" }

// ShouldAutoReconnect reports whether the reconciliation loop may adopt this
// instance. Intent alone is not enough: after an invalidation the intent stays
// active — so the user's wish is not silently discarded — while the permanent
// reason blocks retries until an explicit command clears it (FR-029, FR-031).
func (i Instance) ShouldAutoReconnect() bool {
	return i.Intent == IntentActive && !i.LastDisconnectReason.Permanent()
}

// ValidateInstanceName applies the shared name rules to an instance name.
func ValidateInstanceName(location, name string) error { return ValidateName(location, name) }
