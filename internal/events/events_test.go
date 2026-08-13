package events_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/events"
	"github.com/zapperhub/zappermeow/internal/store/testutil"
)

func setup(t *testing.T) (*events.Publisher, *events.Subscriber, domain.ID, context.Context) {
	t.Helper()

	infra := testutil.Shared(t)
	infra.Reset(t)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return events.NewPublisher(infra.Redis), events.NewSubscriber(infra.Redis, logger), domain.NewID(), context.Background()
}

func receive(t *testing.T, stream *events.Stream) events.Envelope {
	t.Helper()
	select {
	case envelope, ok := <-stream.Events():
		require.True(t, ok, "stream closed before delivering an event")
		return envelope
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for an event")
		return events.Envelope{}
	}
}

func TestPublishAndReceive(t *testing.T) {
	publisher, subscriber, instanceID, ctx := setup(t)

	stream, err := subscriber.Subscribe(ctx, instanceID)
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()

	published, err := publisher.Publish(ctx, events.Envelope{
		Type:       events.TypePairingCode,
		InstanceID: instanceID,
		Generation: 7,
		Data:       map[string]any{"method": "qr", "code": "2@AbC"},
	})
	require.NoError(t, err)

	got := receive(t, stream)
	assert.Equal(t, events.TypePairingCode, got.Type)
	assert.Equal(t, instanceID, got.InstanceID)
	assert.Equal(t, int64(7), got.Generation, "the lease generation must survive the round trip")
	assert.Equal(t, published.Seq, got.Seq)
	assert.Equal(t, "2@AbC", got.Data["code"])
	assert.False(t, got.OccurredAt.IsZero())
}

// The sequence is what lets a client deduplicate the overlap between the
// opening snapshot and the live stream, so it must never go backwards.
func TestSequenceIsMonotonicPerInstance(t *testing.T) {
	publisher, _, instanceID, ctx := setup(t)
	other := domain.NewID()

	var seqs []int64
	for range 5 {
		envelope, err := publisher.Publish(ctx, events.Envelope{Type: events.TypeConnected, InstanceID: instanceID})
		require.NoError(t, err)
		seqs = append(seqs, envelope.Seq)
	}
	assert.Equal(t, []int64{1, 2, 3, 4, 5}, seqs)

	// Counters are per instance: a busy neighbour must not advance ours.
	envelope, err := publisher.Publish(ctx, events.Envelope{Type: events.TypeConnected, InstanceID: other})
	require.NoError(t, err)
	assert.Equal(t, int64(1), envelope.Seq)

	current, err := publisher.CurrentSeq(ctx, instanceID)
	require.NoError(t, err)
	assert.Equal(t, int64(5), current)
}

func TestCurrentSeqIsZeroBeforeAnyEvent(t *testing.T) {
	publisher, _, instanceID, ctx := setup(t)

	current, err := publisher.CurrentSeq(ctx, instanceID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), current, "an instance with no events starts at zero, not an error")
}

// Multiple listeners per instance is a requirement, not an accident: a tenant
// may watch from a dashboard while an integration listens in parallel (FR-034).
func TestEveryListenerReceivesEveryEvent(t *testing.T) {
	publisher, subscriber, instanceID, ctx := setup(t)

	first, err := subscriber.Subscribe(ctx, instanceID)
	require.NoError(t, err)
	defer func() { _ = first.Close() }()

	second, err := subscriber.Subscribe(ctx, instanceID)
	require.NoError(t, err)
	defer func() { _ = second.Close() }()

	_, err = publisher.Publish(ctx, events.Envelope{Type: events.TypeConnected, InstanceID: instanceID})
	require.NoError(t, err)

	assert.Equal(t, events.TypeConnected, receive(t, first).Type)
	assert.Equal(t, events.TypeConnected, receive(t, second).Type)
}

func TestListenersOnlyReceiveTheirOwnInstance(t *testing.T) {
	publisher, subscriber, instanceID, ctx := setup(t)
	other := domain.NewID()

	stream, err := subscriber.Subscribe(ctx, instanceID)
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()

	_, err = publisher.Publish(ctx, events.Envelope{Type: events.TypeConnected, InstanceID: other})
	require.NoError(t, err)
	_, err = publisher.Publish(ctx, events.Envelope{Type: events.TypeBanned, InstanceID: instanceID})
	require.NoError(t, err)

	// The first event to arrive must be ours: isolation is per channel, so the
	// other instance's frame never reaches this subscription at all.
	assert.Equal(t, events.TypeBanned, receive(t, stream).Type)
}

func TestPairingSnapshotRoundTrip(t *testing.T) {
	publisher, _, instanceID, ctx := setup(t)

	_, found, err := publisher.Pairing(ctx, instanceID)
	require.NoError(t, err)
	assert.False(t, found, "no attempt in flight means no snapshot")

	snapshot := events.PairingSnapshot{
		Method:    "qr",
		Code:      "2@AbC",
		ExpiresAt: time.Now().Add(20 * time.Second),
	}
	require.NoError(t, publisher.SetPairing(ctx, instanceID, snapshot))

	got, found, err := publisher.Pairing(ctx, instanceID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "2@AbC", got.Code)
	assert.Equal(t, "qr", got.Method)
	assert.WithinDuration(t, snapshot.ExpiresAt, got.ExpiresAt, time.Second)

	require.NoError(t, publisher.ClearPairing(ctx, instanceID))
	_, found, err = publisher.Pairing(ctx, instanceID)
	require.NoError(t, err)
	assert.False(t, found)
}

// A code that already expired must never be served to a client arriving late:
// it would render a QR nobody can scan.
func TestExpiredPairingCodeIsNotStored(t *testing.T) {
	publisher, _, instanceID, ctx := setup(t)

	require.NoError(t, publisher.SetPairing(ctx, instanceID, events.PairingSnapshot{
		Method:    "qr",
		Code:      "stale",
		ExpiresAt: time.Now().Add(-time.Second),
	}))

	_, found, err := publisher.Pairing(ctx, instanceID)
	require.NoError(t, err)
	assert.False(t, found)
}

func TestPairingSnapshotExpiresWithTheCode(t *testing.T) {
	publisher, _, instanceID, ctx := setup(t)

	require.NoError(t, publisher.SetPairing(ctx, instanceID, events.PairingSnapshot{
		Method:    "qr",
		Code:      "short-lived",
		ExpiresAt: time.Now().Add(time.Second),
	}))

	_, found, err := publisher.Pairing(ctx, instanceID)
	require.NoError(t, err)
	require.True(t, found)

	// Redis forgets the code exactly when WhatsApp does; no sweeper needed.
	assert.Eventually(t, func() bool {
		_, found, err := publisher.Pairing(ctx, instanceID)
		return err == nil && !found
	}, 4*time.Second, 100*time.Millisecond)
}

func TestControlChannels(t *testing.T) {
	publisher, subscriber, instanceID, ctx := setup(t)

	claims, closeClaims, err := subscriber.SubscribeControl(ctx, events.ClaimChannel)
	require.NoError(t, err)
	defer func() { _ = closeClaims() }()

	stops, closeStops, err := subscriber.SubscribeControl(ctx, events.StopChannel)
	require.NoError(t, err)
	defer func() { _ = closeStops() }()

	require.NoError(t, publisher.Claim(ctx, instanceID))
	select {
	case got := <-claims:
		assert.Equal(t, instanceID, got)
	case <-time.After(3 * time.Second):
		t.Fatal("claim was never delivered; the first QR would wait for the reconciliation tick")
	}

	require.NoError(t, publisher.Stop(ctx, instanceID))
	select {
	case got := <-stops:
		assert.Equal(t, instanceID, got)
	case <-time.After(3 * time.Second):
		t.Fatal("stop was never delivered; suspending a tenant would not drop its sessions")
	}
}

func TestEnvelopeMarshalRoundTrip(t *testing.T) {
	original := events.Envelope{
		Seq:        42,
		Type:       events.TypeDisconnected,
		InstanceID: domain.NewID(),
		Generation: 3,
		OccurredAt: time.Now().UTC().Truncate(time.Millisecond),
		Data:       map[string]any{"reason": "network", "permanent": false},
	}

	raw, err := original.Marshal()
	require.NoError(t, err)

	got, err := events.Unmarshal(raw)
	require.NoError(t, err)
	assert.Equal(t, original.Seq, got.Seq)
	assert.Equal(t, original.Type, got.Type)
	assert.Equal(t, original.InstanceID, got.InstanceID)
	assert.Equal(t, original.Generation, got.Generation)
	assert.Equal(t, "network", got.Data["reason"])
	assert.Equal(t, false, got.Data["permanent"])
}
