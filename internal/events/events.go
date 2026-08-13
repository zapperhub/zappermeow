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

// PairingSnapshot is the live pairing attempt, as stored in Redis.
type PairingSnapshot struct {
	Method    string    `json:"method"`
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
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
