// Package events carries connection events from the worker that owns a session
// to the API replicas that serve WebSocket clients.
//
// Redis pub/sub is the transport because any replica may hold the client: a
// gRPC stream would tie an event to the replica that issued the command, and
// the tenant's browser is rarely connected to that one.
package events

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/zapperhub/zappermeow/internal/domain"
)

// Type is the closed vocabulary published on the wire, namespaced by dot. It is
// part of the WebSocket contract, so values here are published API.
type Type string

const (
	TypeStateSnapshot  Type = "state.snapshot"
	TypePairingCode    Type = "pairing.code"
	TypePairingSucceed Type = "pairing.succeeded"
	TypePairingExpired Type = "pairing.expired"
	TypePairingFailed  Type = "pairing.failed"
	TypeConnected      Type = "connection.connected"
	TypeDisconnected   Type = "connection.disconnected"
	TypeLoggedOut      Type = "connection.logged_out"
	TypeBanned         Type = "connection.banned"
	TypeNumberChanged  Type = "connection.number_changed"

	// TypePairingPasskeyChallenge carries the WebAuthn challenge WhatsApp
	// requires during a pairing attempt. Entering this step invalidates any QR
	// code already on screen (research R7), so a client that receives it must
	// stop showing the QR: no further pairing.code arrives for this attempt.
	TypePairingPasskeyChallenge Type = "pairing.passkey_challenge"
	// TypePairingPasskeyCode carries the handoff code to be compared against the
	// handset. It is only emitted when the confirmation is not automatic.
	TypePairingPasskeyCode Type = "pairing.passkey_code"
)

// Envelope is one frame as delivered to a WebSocket client.
type Envelope struct {
	// Seq is monotonic per instance. It lets a client deduplicate the overlap
	// between the opening snapshot and the live stream, and notice a gap.
	Seq int64 `json:"seq"`

	Type       Type      `json:"type"`
	InstanceID domain.ID `json:"instance_id"`

	// Generation is the lease generation that produced the event. A frame with
	// a lower generation than the last one seen comes from a former owner and
	// must be ignored.
	Generation int64 `json:"generation"`

	OccurredAt time.Time `json:"occurred_at"`

	// Data is the type-specific payload. Never carries session material, tokens
	// or credentials (FR-043).
	Data map[string]any `json:"data"`
}

// Channel is the Redis pub/sub channel of an instance.
func Channel(instanceID domain.ID) string {
	return fmt.Sprintf("events:%s", instanceID)
}

// seqKey is the per-instance event counter.
func seqKey(instanceID domain.ID) string {
	return fmt.Sprintf("wa:seq:%s", instanceID)
}

// pairingKey holds the pairing code currently valid for an instance, so a
// client that opens the WebSocket mid-attempt sees the code immediately instead
// of waiting for the next rotation.
func pairingKey(instanceID domain.ID) string {
	return fmt.Sprintf("wa:pairing:%s", instanceID)
}

// PairingPhase discriminates what a pairing attempt is currently waiting on.
// Without it, a client that opens the channel during the passkey step would see
// a QR code that no longer works and nothing else (research R10).
type PairingPhase string

const (
	// PhaseQR is a QR or phone code awaiting a scan.
	PhaseQR PairingPhase = "qr"
	// PhasePasskeyChallenge is a WebAuthn challenge awaiting an assertion.
	PhasePasskeyChallenge PairingPhase = "passkey_challenge"
	// PhasePasskeyCode is a handoff code awaiting confirmation.
	PhasePasskeyCode PairingPhase = "passkey_code"
)

// PairingSnapshot is the live pairing attempt, as stored in Redis.
//
// Which fields are set depends on Phase: Code for qr and passkey_code,
// Challenge for passkey_challenge. Phase is empty on snapshots written before
// this feature, which read as PhaseQR.
type PairingSnapshot struct {
	Method    string       `json:"method"`
	Phase     PairingPhase `json:"phase,omitempty"`
	Code      string       `json:"code,omitempty"`
	ExpiresAt time.Time    `json:"expires_at"`

	// Challenge is the WebAuthn publicKey object, passed through opaque: the
	// platform never inspects or validates it.
	Challenge json.RawMessage `json:"challenge,omitempty"`
}

// CurrentPhase reports the phase, treating the empty value as a QR attempt so
// snapshots written by the previous version still read correctly.
func (s PairingSnapshot) CurrentPhase() PairingPhase {
	if s.Phase == "" {
		return PhaseQR
	}
	return s.Phase
}

// Marshal encodes an envelope for the wire.
func (e Envelope) Marshal() ([]byte, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("marshal envelope: %w", err)
	}
	return raw, nil
}

// Unmarshal decodes an envelope received from Redis.
func Unmarshal(raw []byte) (Envelope, error) {
	var envelope Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return Envelope{}, fmt.Errorf("unmarshal envelope: %w", err)
	}
	return envelope, nil
}
