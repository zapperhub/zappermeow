package events

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/redis/go-redis/v9"

	"github.com/zapperhub/zappermeow/internal/domain"
)

// bufferSize bounds how far a WebSocket client may fall behind before the
// server gives up on it. A slow client must not hold events for everyone else.
const bufferSize = 64

// Subscriber is the API side of the bus.
type Subscriber struct {
	redis  *redis.Client
	logger *slog.Logger
}

// NewSubscriber builds a Subscriber.
func NewSubscriber(client *redis.Client, logger *slog.Logger) *Subscriber {
	return &Subscriber{redis: client, logger: logger}
}

// Stream is a live subscription to one instance's events.
type Stream struct {
	pubsub *redis.PubSub
	events chan Envelope
	// Overflowed reports that the client fell behind and frames were dropped;
	// the handler closes such a connection instead of pretending it is in sync.
	overflowed chan struct{}
}

// Events is the stream of envelopes.
func (s *Stream) Events() <-chan Envelope { return s.events }

// Overflowed is closed when the consumer fell too far behind.
func (s *Stream) Overflowed() <-chan struct{} { return s.overflowed }

// Close releases the subscription.
func (s *Stream) Close() error { return s.pubsub.Close() }

// Subscribe starts listening to an instance's channel.
//
// The caller must subscribe BEFORE reading the snapshot: doing it the other way
// round leaves a window where an event published between the two steps is lost
// forever. Subscribing first duplicates instead of losing, and duplicates are
// resolved by the sequence number.
func (s *Subscriber) Subscribe(ctx context.Context, instanceID domain.ID) (*Stream, error) {
	pubsub := s.redis.Subscribe(ctx, Channel(instanceID))
	if _, err := pubsub.Receive(ctx); err != nil {
		_ = pubsub.Close()
		return nil, fmt.Errorf("subscribe to instance channel: %w", err)
	}

	stream := &Stream{
		pubsub:     pubsub,
		events:     make(chan Envelope, bufferSize),
		overflowed: make(chan struct{}),
	}

	go s.pump(ctx, instanceID, pubsub, stream)
	return stream, nil
}

func (s *Subscriber) pump(ctx context.Context, instanceID domain.ID, pubsub *redis.PubSub, stream *Stream) {
	defer close(stream.events)

	ch := pubsub.Channel(redis.WithChannelSize(bufferSize))
	overflowSignalled := false

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			envelope, err := Unmarshal([]byte(msg.Payload))
			if err != nil {
				// A malformed frame is a bug on the publishing side; dropping
				// this one is better than tearing down every client's stream.
				s.logger.Error("discarding malformed event",
					slog.String("instance_id", instanceID.String()),
					slog.String("error", err.Error()))
				continue
			}

			select {
			case stream.events <- envelope:
			default:
				if !overflowSignalled {
					overflowSignalled = true
					close(stream.overflowed)
					s.logger.Warn("websocket consumer fell behind, closing stream",
						slog.String("instance_id", instanceID.String()))
				}
				return
			}
		}
	}
}

// SubscribeControl listens on a worker coordination channel and delivers the
// instance IDs published to it.
func (s *Subscriber) SubscribeControl(ctx context.Context, channel string) (<-chan domain.ID, func() error, error) {
	pubsub := s.redis.Subscribe(ctx, channel)
	if _, err := pubsub.Receive(ctx); err != nil {
		_ = pubsub.Close()
		return nil, nil, fmt.Errorf("subscribe to %s: %w", channel, err)
	}

	out := make(chan domain.ID, bufferSize)
	go func() {
		defer close(out)
		ch := pubsub.Channel(redis.WithChannelSize(bufferSize))
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				id, err := domain.ParseID("instance_id", msg.Payload)
				if err != nil {
					s.logger.Error("discarding malformed control message",
						slog.String("channel", channel),
						slog.String("error", err.Error()))
					continue
				}
				select {
				case out <- id:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return out, pubsub.Close, nil
}
