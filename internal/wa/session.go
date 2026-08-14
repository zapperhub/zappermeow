// Package wa is the boundary between ZapperMeow and the HyperMeow library.
//
// Everything the platform knows about WhatsApp passes through the Session
// interface. That indirection is deliberate: WhatsApp offers no sandbox, a test
// against it needs a real number and a human scanning a QR code, and automating
// one risks banning the number. With the boundary named, the state machine,
// lease, fencing and fan-out are all exercised against real Postgres and Redis
// with a scripted session, and only the last hop stays manual (research R13).
package wa

import (
	"context"
	"encoding/json"
	"time"

	"github.com/zapperhub/zappermeow/internal/domain"
)

// EventKind is the closed set of session events the worker reacts to. It is
// deliberately smaller than HyperMeow's ~75 event types: this slice only cares
// about pairing and connection.
type EventKind string

const (
	// KindPairingCode carries a new QR or phone pairing code.
	KindPairingCode EventKind = "pairing_code"
	// KindPairingSucceeded reports a completed handshake with the device identity.
	KindPairingSucceeded EventKind = "pairing_succeeded"
	// KindPairingExpired reports a pairing window that ran out.
	KindPairingExpired EventKind = "pairing_expired"
	// KindPairingFailed reports a pairing rejected by WhatsApp.
	KindPairingFailed EventKind = "pairing_failed"
	// KindConnected reports an authenticated connection.
	KindConnected EventKind = "connected"
	// KindDisconnected reports a lost connection, transient or permanent.
	KindDisconnected EventKind = "disconnected"
	// KindPasskeyChallenge reports that WhatsApp requires the passkey step. The
	// attempt stays alive waiting for SendPasskeyResponse.
	KindPasskeyChallenge EventKind = "passkey_challenge"
	// KindPasskeyCode reports the handoff code to show the number's owner. It
	// only arrives when the confirmation is not automatic: with a valid handoff
	// proof the library confirms on its own and this never fires (research R7).
	KindPasskeyCode EventKind = "passkey_code"
	// KindManualLoginReconnect reports the server asking the client to reconnect
	// on its own after pairing. The library only raises it when its own
	// post-pairing reconnect is disabled, which this platform does not do — the
	// case is handled defensively so a future flag or fork change cannot leave
	// an attempt hanging on its last step (research R5).
	KindManualLoginReconnect EventKind = "manual_login_reconnect"
)

// PairingMethod distinguishes the two ways of linking a device.
type PairingMethod string

const (
	MethodQR    PairingMethod = "qr"
	MethodPhone PairingMethod = "phone"
)

// PairingFailure is why a pairing attempt was rejected, as published in the
// WebSocket contract.
type PairingFailure string

const (
	FailureScannedWithoutMultidevice PairingFailure = "scanned_without_multidevice"
	FailureClientOutdated            PairingFailure = "client_outdated"
	FailurePairError                 PairingFailure = "pair_error"
	FailureUnexpectedState           PairingFailure = "unexpected_state"
	// FailurePasskeyError covers any failing step of the passkey exchange:
	// an unreadable challenge, a rejected assertion, or a broken continuation.
	FailurePasskeyError PairingFailure = "passkey_error"
)

// PairingExpiry is why a pairing attempt ended without success.
type PairingExpiry string

const (
	ExpiryWindowExhausted PairingExpiry = "window_exhausted"
	ExpiryCancelled       PairingExpiry = "cancelled"
	ExpiryReplaced        PairingExpiry = "replaced_by_new_attempt"
	ExpiryWorkerShutdown  PairingExpiry = "worker_shutdown"
)

// Event is what a Session emits. Only the fields relevant to Kind are set.
type Event struct {
	Kind EventKind

	// Pairing code and its validity (KindPairingCode), or the handoff code to
	// be compared against the handset (KindPasskeyCode).
	Method    PairingMethod
	Code      string
	ExpiresAt time.Time

	// Challenge is the WebAuthn publicKey object (KindPasskeyChallenge), passed
	// through as opaque JSON: only the authenticator on the tenant's side knows
	// how to answer it, and the platform gains nothing from parsing it.
	Challenge json.RawMessage

	// Device identity (KindPairingSucceeded).
	Device *domain.DeviceIdentity

	// Why the attempt ended (KindPairingExpired / KindPairingFailed).
	Expiry  PairingExpiry
	Failure PairingFailure

	// Why the connection dropped and whether retrying is pointless
	// (KindDisconnected).
	Reason    domain.DisconnectReason
	Permanent bool
	// BanExpiresAt is set only when WhatsApp reports a deadline for a ban.
	BanExpiresAt *time.Time
	// StreamErrorCode is the unknown code that closed the stream, set only with
	// ReasonStreamError. The raw node it came from is deliberately dropped: it
	// is server-controlled payload with no place in a queryable trail.
	StreamErrorCode string

	OccurredAt time.Time
}

// Status is a snapshot of what the client believes about itself. It complements
// the persisted state rather than replacing it: the database is authoritative,
// this is the live view of the process that owns the session.
type Status struct {
	Connected bool
	LoggedIn  bool
	Device    *domain.DeviceIdentity
}

// VerificationCodes is the safety-number material for one conversation: the
// numeric code both sides compare, and the two QR payloads (one to display, one
// to scan when verifying). All of it is opaque to the platform — it is produced
// for the tenant to render and never stored.
type VerificationCodes struct {
	LID         string
	PhoneNumber string
	Username    string
	// NumericCode is the 60-digit safety number.
	NumericCode string
	// DisplayQR omits the raw keys; VerificationQR carries them.
	DisplayQR      []byte
	VerificationQR []byte
}

// Session is one WhatsApp companion device, owned by exactly one process.
//
// Implementations are not safe for concurrent commands: the worker serialises
// them per instance, which is also what the lease guarantees across processes.
type Session interface {
	// QRChannel starts a QR pairing attempt and returns the stream of codes.
	// It must be called before Connect and only on a session with no device
	// material; the underlying library rejects it otherwise. The caller must
	// keep draining the channel — a slow consumer makes HyperMeow close it and
	// disconnect the client (research R2).
	QRChannel(ctx context.Context) (<-chan Event, error)

	// PairPhone starts a phone-code pairing attempt and returns the code to be
	// typed on the handset.
	PairPhone(ctx context.Context, phoneNumber string) (code string, expiresAt time.Time, err error)

	// Connect establishes the connection, pairing first when a QR attempt is in
	// flight. It returns once the socket is up, not once the handshake with the
	// phone completes: watch Events for that.
	Connect(ctx context.Context) error

	// Disconnect closes the connection but keeps the session material, so a
	// later Connect needs no new pairing.
	Disconnect()

	// Logout removes the companion device on the server and deletes the local
	// material. remoteRemoved is false when the server could not be reached and
	// only the local material was dropped — the device may still be listed on
	// the customer's handset (research R10).
	Logout(ctx context.Context) (remoteRemoved bool, err error)

	// Events is the stream of connection events for this session.
	Events() <-chan Event

	// Status reports the live view of the session.
	Status() Status

	// SetPassive tells the server whether this device announces itself as
	// active. It needs a live connection, and it does not survive one: the
	// library restores active mode on every connection, before it reports
	// Connected. The caller must reapply after each Connected, not once
	// (research R6).
	SetPassive(ctx context.Context, passive bool) error

	// SendPasskeyResponse forwards the authenticator's WebAuthn assertion for
	// the pending challenge. Only valid while a passkey challenge is in flight.
	SendPasskeyResponse(ctx context.Context, webauthnResponseJSON []byte) error

	// ConfirmPasskey confirms the handoff code was shown and acknowledged. It
	// requires a code to be pending and consumes it: a second call fails, which
	// is what turns a double submit into a clear error rather than a corrupted
	// attempt (research R7).
	ConfirmPasskey(ctx context.Context) error

	// IdentityVerificationCodes derives the safety numbers for a conversation.
	// contact may be a LID or a phone number; a phone number is resolved
	// through the mappings this session already knows. It talks to WhatsApp, so
	// it needs a connected session, and it may persist identities it learns
	// along the way (research R8).
	IdentityVerificationCodes(ctx context.Context, contact string) (*VerificationCodes, error)

	// Close releases the client without touching the session material.
	Close()
}
