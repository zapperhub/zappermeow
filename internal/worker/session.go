package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/events"
	"github.com/zapperhub/zappermeow/internal/lease"
	"github.com/zapperhub/zappermeow/internal/metrics"
	"github.com/zapperhub/zappermeow/internal/store"
	"github.com/zapperhub/zappermeow/internal/wa"
)

// pump turns one session's events into persisted state and published frames.
// It is the only place that writes connection state, so the database and the
// WebSocket can never tell a tenant different stories (FR-038).
func (s *Supervisor) pump(ctx context.Context, managed *managedSession) {
	defer close(managed.done)

	// Same reasoning as pumpPairing: a handler that stops the session cancels
	// this context, and its remaining writes must still complete.
	handlerCtx := context.WithoutCancel(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-managed.inbox:
			if !ok {
				return
			}
			s.handle(handlerCtx, managed, evt)
		}
	}
}

func (s *Supervisor) handle(ctx context.Context, managed *managedSession, evt wa.Event) {
	instanceID := managed.instanceID

	switch evt.Kind {
	case wa.KindPairingCode:
		s.handlePairingCode(ctx, managed, evt)

	case wa.KindPairingSucceeded:
		s.handlePairSuccess(ctx, managed, evt)

	case wa.KindPairingExpired:
		managed.cancelPairing()
		_ = s.publisher.ClearPairing(ctx, instanceID)
		s.restoreStateAfterPairing(ctx, managed)
		metrics.PairingAttempts.WithLabelValues(string(evt.Method), "expired").Inc()
		s.record(ctx, instanceID, domain.ConnEventPairingExpired, domain.ReasonNone,
			map[string]any{"reason": string(evt.Expiry)})
		s.publish(ctx, managed, events.TypePairingExpired, map[string]any{
			"method": string(evt.Method),
			"reason": string(evt.Expiry),
		})

	case wa.KindPairingFailed:
		managed.cancelPairing()
		_ = s.publisher.ClearPairing(ctx, instanceID)
		s.restoreStateAfterPairing(ctx, managed)
		metrics.PairingAttempts.WithLabelValues(string(evt.Method), "failed").Inc()
		s.record(ctx, instanceID, domain.ConnEventPairingFailed, domain.ReasonNone,
			map[string]any{"reason": string(evt.Failure)})
		s.publish(ctx, managed, events.TypePairingFailed, map[string]any{
			"reason": string(evt.Failure),
		})

	case wa.KindConnected:
		// The pairing success and this event arrive on different channels —
		// the QR stream and the client's own event handler — and nothing
		// orders them. If this one lands first, the instance would be
		// "connected" with no device recorded until the other arrived, and a
		// worker restarting in that window would treat it as unpaired.
		s.ensureDeviceIdentity(ctx, managed)

		if err := s.setState(ctx, instanceID, domain.InstanceConnected); err != nil {
			s.logger.Error("persisting connected state failed",
				slog.String("instance_id", instanceID.String()),
				slog.String("error", err.Error()))
			return
		}
		metrics.SessionStateTransitions.WithLabelValues(string(domain.InstanceConnected), "").Inc()
		s.record(ctx, instanceID, domain.ConnEventConnected, domain.ReasonNone, nil)
		s.publish(ctx, managed, events.TypeConnected, map[string]any{
			"connected_at": time.Now().UTC(),
		})

	case wa.KindDisconnected:
		s.handleDisconnect(ctx, managed, evt)
	}
}

func (s *Supervisor) handlePairingCode(ctx context.Context, managed *managedSession, evt wa.Event) {
	snapshot := events.PairingSnapshot{
		Method:    string(evt.Method),
		Code:      evt.Code,
		ExpiresAt: evt.ExpiresAt,
	}
	// Stored before publishing: a client that opens the WebSocket a moment
	// later reads this snapshot and sees the same code, instead of staring at
	// an empty screen until the next rotation.
	if err := s.publisher.SetPairing(ctx, managed.instanceID, snapshot); err != nil {
		s.logger.Error("storing pairing snapshot failed",
			slog.String("instance_id", managed.instanceID.String()),
			slog.String("error", err.Error()))
	}

	s.publish(ctx, managed, events.TypePairingCode, map[string]any{
		"method":     string(evt.Method),
		"code":       evt.Code,
		"expires_at": evt.ExpiresAt.UTC(),
	})
}

func (s *Supervisor) handlePairSuccess(ctx context.Context, managed *managedSession, evt wa.Event) {
	instanceID := managed.instanceID
	managed.cancelPairing()
	_ = s.publisher.ClearPairing(ctx, instanceID)

	if evt.Device == nil {
		s.logger.Error("pairing succeeded without a device identity",
			slog.String("instance_id", instanceID.String()))
		return
	}

	previous, err := s.queries.GetInstanceConnectionByID(ctx, instanceID)
	if err != nil {
		s.logger.Error("loading instance after pairing failed",
			slog.String("instance_id", instanceID.String()),
			slog.String("error", err.Error()))
		return
	}

	previousPhone := ""
	if previous.PhoneNumber != nil {
		previousPhone = *previous.PhoneNumber
	}
	if previousPhone == "" {
		// Logout clears the identity, so a re-pairing has nothing on the row to
		// compare against. The trail remembers, and without this lookup a number
		// swap would never be detected at all (FR-016).
		previousPhone = s.lastPairedPhone(ctx, instanceID)
	}
	numberChanged := previousPhone != "" && previousPhone != evt.Device.PhoneNumber

	err = s.queries.SetDeviceIdentity(ctx, store.SetDeviceIdentityParams{
		ID:           instanceID,
		WaJid:        ptr(evt.Device.JID),
		WaLid:        ptr(evt.Device.LID),
		PhoneNumber:  ptr(evt.Device.PhoneNumber),
		PushName:     ptr(evt.Device.PushName),
		Platform:     ptr(evt.Device.Platform),
		BusinessName: ptr(evt.Device.BusinessName),
	})
	if err != nil {
		s.logger.Error("persisting device identity failed",
			slog.String("instance_id", instanceID.String()),
			slog.String("error", err.Error()))
		return
	}

	// The number is recorded on the event itself: it is what lets the next
	// pairing tell a swap from a first pairing.
	s.record(ctx, instanceID, domain.ConnEventPairingSucceeded, domain.ReasonNone,
		map[string]any{"phone_number": evt.Device.PhoneNumber})
	if numberChanged {
		// The instance is now a companion device of a different number. Not an
		// error — the spec allows it — but the trail must show the swap.
		s.record(ctx, instanceID, domain.ConnEventNumberChanged, domain.ReasonNone, map[string]any{
			"previous_phone": previousPhone,
			"new_phone":      evt.Device.PhoneNumber,
		})
	}

	metrics.PairingAttempts.WithLabelValues(string(wa.MethodQR), "succeeded").Inc()
	s.publish(ctx, managed, events.TypePairingSucceed, map[string]any{
		"device": map[string]any{
			"jid":          evt.Device.JID,
			"phone_number": evt.Device.PhoneNumber,
			"push_name":    evt.Device.PushName,
			"platform":     evt.Device.Platform,
		},
		"number_changed": numberChanged,
	})
}

func (s *Supervisor) handleDisconnect(ctx context.Context, managed *managedSession, evt wa.Event) {
	instanceID := managed.instanceID

	state := domain.InstanceConnecting
	eventType := domain.ConnEventDisconnected
	frameType := events.TypeDisconnected

	switch evt.Reason {
	case domain.ReasonLoggedOutFromPhone:
		state = domain.InstanceLoggedOut
		eventType = domain.ConnEventLoggedOut
		frameType = events.TypeLoggedOut
	case domain.ReasonTemporaryBan:
		state = domain.InstanceBanned
		eventType = domain.ConnEventBanned
		frameType = events.TypeBanned
	default:
		if evt.Permanent {
			state = domain.InstanceDisconnected
		}
	}

	if evt.Reason == domain.ReasonSessionReplaced {
		// This must never happen with the lease working: the same device
		// credentials were opened somewhere else. It is an incident about
		// exclusive ownership (principle III), not routine telemetry — the
		// counter is expected to stay at zero forever.
		metrics.StreamReplaced.Inc()
		s.logger.Error("exclusive session ownership violated: stream replaced elsewhere",
			slog.String("instance_id", instanceID.String()))
	}
	metrics.SessionStateTransitions.WithLabelValues(string(state), string(evt.Reason)).Inc()
	if !evt.Permanent {
		metrics.SessionReconnects.WithLabelValues("client").Inc()
	}

	if err := s.recordDisconnect(ctx, instanceID, state, evt.Reason, evt.BanExpiresAt); err != nil {
		s.logger.Error("persisting disconnect failed",
			slog.String("instance_id", instanceID.String()),
			slog.String("error", err.Error()))
		return
	}

	if evt.Reason == domain.ReasonLoggedOutFromPhone {
		// The library deletes the local material when the server says the
		// device is gone. Leaving the JID on the row would point the next
		// pairing at material that no longer exists, and the instance could
		// never come back. The number stays on the row for context; the trail
		// keeps the full history either way.
		if err := s.queries.ClearDeviceMaterial(ctx, instanceID); err != nil {
			s.logger.Error("clearing device material after remote logout failed",
				slog.String("instance_id", instanceID.String()),
				slog.String("error", err.Error()))
		}
	}

	detail := map[string]any{}
	if evt.BanExpiresAt != nil {
		detail["expires_at"] = evt.BanExpiresAt.UTC()
	}
	s.record(ctx, instanceID, eventType, evt.Reason, detail)

	data := map[string]any{
		"reason":    string(evt.Reason),
		"permanent": evt.Permanent,
		"at":        time.Now().UTC(),
	}
	if frameType == events.TypeLoggedOut {
		data["from_phone"] = true
	}
	if frameType == events.TypeBanned {
		data["expires_at"] = nil
		if evt.BanExpiresAt != nil {
			data["expires_at"] = evt.BanExpiresAt.UTC()
		}
	}
	s.publish(ctx, managed, frameType, data)

	if evt.Permanent {
		// Nothing here can recover the session, and holding the lease would
		// only keep the instance away from a worker that might pair it again.
		s.stopFromPump(instanceID)
		if err := s.leases.SetDesired(ctx, instanceID, lease.DesiredStopped); err != nil {
			s.logger.Error("stopping lease after permanent failure",
				slog.String("instance_id", instanceID.String()),
				slog.String("error", err.Error()))
		}
		_ = s.leases.Release(ctx, instanceID)
	}
}

// lastPairedPhone reads the most recent number from the trail. A miss is not an
// error: a first pairing has nothing before it.
func (s *Supervisor) lastPairedPhone(ctx context.Context, instanceID domain.ID) string {
	raw, err := s.queries.LastPairedPhone(ctx, instanceID)
	if err != nil {
		if !store.IsNoRows(err) {
			s.logger.Error("reading last paired number failed",
				slog.String("instance_id", instanceID.String()),
				slog.String("error", err.Error()))
		}
		return ""
	}
	phone, _ := raw.(string)
	return phone
}

// ensureDeviceIdentity persists what the session already knows about its device
// when the row has nothing yet. It is a no-op on the common path, where the
// pairing handler got there first.
func (s *Supervisor) ensureDeviceIdentity(ctx context.Context, managed *managedSession) {
	status := managed.session.Status()
	if status.Device == nil || status.Device.JID == "" {
		return
	}

	row, err := s.queries.GetInstanceConnectionByID(ctx, managed.instanceID)
	if err != nil || (row.WaJid != nil && *row.WaJid != "") {
		return
	}

	if err := s.queries.SetDeviceIdentity(ctx, store.SetDeviceIdentityParams{
		ID:           managed.instanceID,
		WaJid:        ptr(status.Device.JID),
		WaLid:        ptr(status.Device.LID),
		PhoneNumber:  ptr(status.Device.PhoneNumber),
		PushName:     ptr(status.Device.PushName),
		Platform:     ptr(status.Device.Platform),
		BusinessName: ptr(status.Device.BusinessName),
	}); err != nil {
		s.logger.Error("persisting device identity on connect failed",
			slog.String("instance_id", managed.instanceID.String()),
			slog.String("error", err.Error()))
	}
}

// restoreStateAfterPairing puts the instance back where it was before the
// attempt: registered when it never had material, disconnected when it did.
func (s *Supervisor) restoreStateAfterPairing(ctx context.Context, managed *managedSession) {
	state := domain.InstanceRegistered
	if status := managed.session.Status(); status.Device != nil && status.Device.JID != "" {
		state = domain.InstanceDisconnected
	}
	if err := s.setState(ctx, managed.instanceID, state); err != nil {
		s.logger.Error("restoring state after pairing failed",
			slog.String("instance_id", managed.instanceID.String()),
			slog.String("error", err.Error()))
	}
}

func (s *Supervisor) publishPairingEnded(ctx context.Context, managed *managedSession, reason wa.PairingExpiry) {
	_ = s.publisher.ClearPairing(ctx, managed.instanceID)
	s.record(ctx, managed.instanceID, domain.ConnEventPairingExpired, domain.ReasonNone,
		map[string]any{"reason": string(reason)})
	s.publish(ctx, managed, events.TypePairingExpired, map[string]any{
		"reason": string(reason),
	})
}

// --- persistence and publication helpers ---

func (s *Supervisor) setState(ctx context.Context, instanceID domain.ID, state domain.InstanceState) error {
	err := s.queries.SetConnectionState(ctx, store.SetConnectionStateParams{
		ID:              instanceID,
		ConnectionState: string(state),
	})
	if err != nil {
		return err
	}
	return nil
}

func (s *Supervisor) recordDisconnect(
	ctx context.Context,
	instanceID domain.ID,
	state domain.InstanceState,
	reason domain.DisconnectReason,
	banExpiresAt *time.Time,
) error {
	return s.queries.RecordDisconnect(ctx, store.RecordDisconnectParams{
		ID:                   instanceID,
		ConnectionState:      string(state),
		LastDisconnectReason: ptr(string(reason)),
		BanExpiresAt:         banExpiresAt,
	})
}

// record appends to the connection trail. A failure here is logged rather than
// propagated: losing an audit line must not abort a session transition.
func (s *Supervisor) record(
	ctx context.Context,
	instanceID domain.ID,
	eventType domain.ConnectionEventType,
	reason domain.DisconnectReason,
	detail map[string]any,
) {
	params := store.AppendConnectionEventParams{
		InstanceID: instanceID,
		Type:       string(eventType),
	}
	if reason != domain.ReasonNone {
		params.Reason = ptr(string(reason))
	}
	if len(detail) > 0 {
		raw, err := json.Marshal(detail)
		if err != nil {
			s.logger.Error("encoding event detail failed", slog.String("error", err.Error()))
		} else {
			params.Detail = raw
		}
	}

	if _, err := s.queries.AppendConnectionEvent(ctx, params); err != nil {
		s.logger.Error("appending connection event failed",
			slog.String("instance_id", instanceID.String()),
			slog.String("type", string(eventType)),
			slog.String("error", err.Error()))
	}
}

// publish emits a frame carrying the lease generation, so a client can discard
// anything produced by a former owner.
func (s *Supervisor) publish(ctx context.Context, managed *managedSession, frameType events.Type, data map[string]any) {
	_, err := s.publisher.Publish(ctx, events.Envelope{
		Type:       frameType,
		InstanceID: managed.instanceID,
		Generation: managed.generation,
		Data:       data,
	})
	if err != nil {
		s.logger.Error("publishing event failed",
			slog.String("instance_id", managed.instanceID.String()),
			slog.String("type", string(frameType)),
			slog.String("error", err.Error()))
	}
}

func ptr[T any](v T) *T { return &v }
