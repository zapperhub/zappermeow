package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zapperhub/zappermeow/internal/domain"
)

// seqTTL keeps the counter alive well past any plausible client gap, without
// leaking a key per instance forever.
const seqTTL = 24 * time.Hour

// Publisher is the worker side of the bus.
type Publisher struct {
	redis *redis.Client
}

// NewPublisher builds a Publisher.
func NewPublisher(client *redis.Client) *Publisher {
	return &Publisher{redis: client}
}

// Publish stamps the envelope with the next sequence number and broadcasts it.
//
// The sequence is allocated by Redis rather than by the worker so it stays
// monotonic across a failover: the new owner continues the numbering instead of
// restarting it, which would make a client discard live frames as duplicates.
func (p *Publisher) Publish(ctx context.Context, envelope Envelope) (Envelope, error) {
	seq, err := p.redis.Incr(ctx, seqKey(envelope.InstanceID)).Result()
	if err != nil {
		return Envelope{}, fmt.Errorf("allocate sequence: %w", err)
	}
	if err := p.redis.Expire(ctx, seqKey(envelope.InstanceID), seqTTL).Err(); err != nil {
		return Envelope{}, fmt.Errorf("refresh sequence ttl: %w", err)
	}

	envelope.Seq = seq
	if envelope.OccurredAt.IsZero() {
		envelope.OccurredAt = time.Now().UTC()
	}
	if envelope.Data == nil {
		envelope.Data = map[string]any{}
	}

	payload, err := envelope.Marshal()
	if err != nil {
		return Envelope{}, err
	}
	if err := p.redis.Publish(ctx, Channel(envelope.InstanceID), payload).Err(); err != nil {
		return Envelope{}, fmt.Errorf("publish event: %w", err)
	}
	return envelope, nil
}

// CurrentSeq reports the sequence number already allocated for an instance,
// which is what the snapshot is stamped with when nothing has been published.
func (p *Publisher) CurrentSeq(ctx context.Context, instanceID domain.ID) (int64, error) {
	seq, err := p.redis.Get(ctx, seqKey(instanceID)).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read sequence: %w", err)
	}
	return seq, nil
}

// SetPairing stores the pairing code currently valid for an instance. The TTL
// is the code's own validity, so an expired code cannot be served to a client
// that connects late — Redis forgets it exactly when WhatsApp does.
func (p *Publisher) SetPairing(ctx context.Context, instanceID domain.ID, snapshot PairingSnapshot) error {
	ttl := time.Until(snapshot.ExpiresAt)
	if ttl <= 0 {
		// Already expired: storing it would hand a dead code to the next client.
		return p.ClearPairing(ctx, instanceID)
	}

	payload, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("marshal pairing snapshot: %w", err)
	}
	if err := p.redis.Set(ctx, pairingKey(instanceID), payload, ttl).Err(); err != nil {
		return fmt.Errorf("store pairing snapshot: %w", err)
	}
	return nil
}

// ClearPairing drops the stored code when an attempt ends.
func (p *Publisher) ClearPairing(ctx context.Context, instanceID domain.ID) error {
	if err := p.redis.Del(ctx, pairingKey(instanceID)).Err(); err != nil {
		return fmt.Errorf("clear pairing snapshot: %w", err)
	}
	return nil
}

// Pairing reads the code currently valid for an instance. The second result is
// false when no attempt is in flight.
func (p *Publisher) Pairing(ctx context.Context, instanceID domain.ID) (PairingSnapshot, bool, error) {
	raw, err := p.redis.Get(ctx, pairingKey(instanceID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return PairingSnapshot{}, false, nil
	}
	if err != nil {
		return PairingSnapshot{}, false, fmt.Errorf("read pairing snapshot: %w", err)
	}

	var snapshot PairingSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return PairingSnapshot{}, false, fmt.Errorf("unmarshal pairing snapshot: %w", err)
	}
	return snapshot, true, nil
}

// --- worker coordination channels ---

const (
	// ClaimChannel wakes workers to race for a free lease. Without it the first
	// pairing would wait for the reconciliation tick, missing the five-second
	// budget for showing a QR code.
	ClaimChannel = "sessions:claim"
	// StopChannel tells the current owner to stop a session immediately, which
	// is how tenant suspension takes effect in seconds rather than at the next
	// reconciliation.
	StopChannel = "sessions:stop"
)

// Claim asks whichever worker has capacity to take a session now.
func (p *Publisher) Claim(ctx context.Context, instanceID domain.ID) error {
	if err := p.redis.Publish(ctx, ClaimChannel, instanceID.String()).Err(); err != nil {
		return fmt.Errorf("publish claim: %w", err)
	}
	return nil
}

// Stop asks the current owner to drop a session now.
func (p *Publisher) Stop(ctx context.Context, instanceID domain.ID) error {
	if err := p.redis.Publish(ctx, StopChannel, instanceID.String()).Err(); err != nil {
		return fmt.Errorf("publish stop: %w", err)
	}
	return nil
}
