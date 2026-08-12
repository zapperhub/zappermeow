// Package services holds the use cases of the account foundation. Each service
// orchestrates the store and the security-event trail, and returns
// transport-agnostic domain errors.
package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/store"
)

// EventRecorder persists the security-event trail and mirrors it to the
// structured log. It never receives secret material: callers put identifying
// details (a key prefix, an audience, a cascade count) in Metadata and nothing
// that could reconstruct a credential (SC-006).
type EventRecorder struct {
	logger *slog.Logger
}

// NewEventRecorder builds a recorder writing to the given logger.
func NewEventRecorder(logger *slog.Logger) *EventRecorder {
	return &EventRecorder{logger: logger}
}

// Record writes one event through the supplied queries handle. Pass a
// transaction-bound handle to make the event atomic with the action it
// describes; pass the pool for read-path events such as a failed login.
func (r *EventRecorder) Record(ctx context.Context, q *store.Queries, event domain.SecurityEvent) error {
	metadata := []byte("{}")
	if len(event.Metadata) > 0 {
		encoded, err := json.Marshal(event.Metadata)
		if err != nil {
			return fmt.Errorf("encode event metadata: %w", err)
		}
		metadata = encoded
	}

	var targetType *string
	if event.TargetType != "" {
		targetType = &event.TargetType
	}

	params := store.InsertSecurityEventParams{
		ID:          domain.NewID(),
		EventType:   string(event.Type),
		ActorUserID: event.ActorUserID,
		TargetType:  targetType,
		TargetID:    event.TargetID,
		Result:      string(event.Result),
		SourceIp:    event.SourceIP,
		Metadata:    metadata,
	}
	if err := q.InsertSecurityEvent(ctx, params); err != nil {
		return fmt.Errorf("insert security event: %w", err)
	}

	r.log(ctx, event)
	return nil
}

// log mirrors the event to slog so operators see it in the log stream too.
func (r *EventRecorder) log(ctx context.Context, event domain.SecurityEvent) {
	if r.logger == nil {
		return
	}

	attrs := []any{
		slog.String("event_type", string(event.Type)),
		slog.String("result", string(event.Result)),
	}
	if event.ActorUserID != nil {
		attrs = append(attrs, slog.String("actor_user_id", event.ActorUserID.String()))
	}
	if event.TargetType != "" {
		attrs = append(attrs, slog.String("target_type", event.TargetType))
	}
	if event.TargetID != nil {
		attrs = append(attrs, slog.String("target_id", event.TargetID.String()))
	}
	if event.SourceIP != nil {
		attrs = append(attrs, slog.String("source_ip", event.SourceIP.String()))
	}
	for key, value := range event.Metadata {
		attrs = append(attrs, slog.Any("meta_"+key, value))
	}

	r.logger.LogAttrs(ctx, slog.LevelInfo, "security_event", toAttrs(attrs)...)
}

func toAttrs(values []any) []slog.Attr {
	attrs := make([]slog.Attr, 0, len(values))
	for _, value := range values {
		if attr, ok := value.(slog.Attr); ok {
			attrs = append(attrs, attr)
		}
	}
	return attrs
}
