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

		// Passive mode is reapplied here, after every Connected — not once when
		// the tenant asks for it. The library restores active mode on each
		// connection, in the goroutine that runs before it dispatches this
		// event, so a session that came back from a reconnect or a failover is
		// active again until this runs (research R6).
		s.reapplyPassiveMode(ctx, managed)

	case wa.KindDisconnected:
		s.handleDisconnect(ctx, managed, evt)

	case wa.KindManualLoginReconnect:
		s.handleManualLoginReconnect(ctx, managed)

	case wa.KindPasskeyChallenge:
		s.handlePasskeyChallenge(ctx, managed, evt)

	case wa.KindPasskeyCode:
		s.handlePasskeyCode(ctx, managed, evt)
	}
}

// reapplyPassiveMode pushes the stored mode onto a session that just connected.
//
// A failure is logged and dropped rather than propagated: the session is up and
// working, and taking a healthy number offline because one setting could not be
// pushed would be a worse outcome than the setting being late. The next
// reconnect tries again through this same path.
func (s *Supervisor) reapplyPassiveMode(ctx context.Context, managed *managedSession) {
	settings, err := s.queries.GetInstanceSettings(ctx, managed.instanceID)
	if err != nil {
		s.logger.Error("reading passive mode failed",
			slog.String("instance_id", managed.instanceID.String()),
			slog.String("error", err.Error()))
		return
	}
	// Nothing to do when passive mode is off: the library already leaves every
	// connection in active mode, so the default needs no call of its own.
	if !settings.PassiveMode {
		return
	}

	if _, err := s.applyPassiveMode(ctx, managed, true); err != nil {
		s.logger.Error("applying passive mode after connecting failed",
			slog.String("instance_id", managed.instanceID.String()),
			slog.String("error", err.Error()))
	}
}

// handleManualLoginReconnect answers the server asking the client to reconnect
// on its own after pairing.
//
// The reconnect is scheduled rather than performed inline: the library
// dispatches this one synchronously, unlike the other stream events, so
// blocking here would stall its handler while it holds the connection down
// (research R5).
func (s *Supervisor) handleManualLoginReconnect(ctx context.Context, managed *managedSession) {
	s.record(ctx, managed.instanceID, domain.ConnEventManualLoginReconnect, domain.ReasonNone, nil)
	metrics.SessionReconnects.WithLabelValues("manual_login").Inc()

	s.logger.Info("server asked for a manual reconnect after login",
		slog.String("instance_id", managed.instanceID.String()))

	go s.reconnect(managed.ctx, managed)
}

// handlePasskeyChallenge publishes the WebAuthn challenge WhatsApp requires
// mid-attempt and parks the attempt on it.
//
// The snapshot moves to the passkey phase because the QR it used to hold is
// already dead: entering this step rotates the secret that validates the codes
// on screen. A client that opens the channel now must see the challenge, not a
// code that silently stopped working (research R7, R10).
func (s *Supervisor) handlePasskeyChallenge(ctx context.Context, managed *managedSession, evt wa.Event) {
	expiresAt := managed.pairingExpiry()

	snapshot := events.PairingSnapshot{
		Method:    string(wa.MethodQR),
		Phase:     events.PhasePasskeyChallenge,
		Challenge: evt.Challenge,
		ExpiresAt: expiresAt,
	}
	if err := s.publisher.SetPairing(ctx, managed.instanceID, snapshot); err != nil {
		s.logger.Error("storing passkey challenge snapshot failed",
			slog.String("instance_id", managed.instanceID.String()),
			slog.String("error", err.Error()))
	}

	// The challenge itself is not persisted: it lives and dies with the
	// attempt, like the QR code before it (data-model §3).
	s.record(ctx, managed.instanceID, domain.ConnEventPasskeyChallenge, domain.ReasonNone, nil)
	metrics.PasskeyPairings.WithLabelValues("challenged").Inc()

	s.publish(ctx, managed, events.TypePairingPasskeyChallenge, map[string]any{
		"public_key":         evt.Challenge,
		"attempt_expires_at": expiresAt.UTC(),
	})
}

// handlePasskeyCode publishes the handoff code for the number's owner to
// compare against their handset.
//
// This only arrives when the confirmation is not automatic: with a valid
// handoff proof the library confirms on its own and emits nothing, so there is
// no auto-confirm path here to write (research R7).
func (s *Supervisor) handlePasskeyCode(ctx context.Context, managed *managedSession, evt wa.Event) {
	expiresAt := managed.pairingExpiry()

	snapshot := events.PairingSnapshot{
		Method:    string(wa.MethodQR),
		Phase:     events.PhasePasskeyCode,
		Code:      evt.Code,
		ExpiresAt: expiresAt,
	}
	if err := s.publisher.SetPairing(ctx, managed.instanceID, snapshot); err != nil {
		s.logger.Error("storing passkey code snapshot failed",
			slog.String("instance_id", managed.instanceID.String()),
			slog.String("error", err.Error()))
	}

	s.record(ctx, managed.instanceID, domain.ConnEventPasskeyResponded, domain.ReasonNone, nil)

	s.publish(ctx, managed, events.TypePairingPasskeyCode, map[string]any{
		"code":               evt.Code,
		"attempt_expires_at": expiresAt.UTC(),
	})
}

// SubmitPasskeyResponse forwards the authenticator's assertion for the attempt
// in flight.
func (s *Supervisor) SubmitPasskeyResponse(ctx context.Context, instanceID domain.ID, webauthnJSON []byte) (domain.InstanceState, error) {
	managed, ok := s.lookup(instanceID)
	if !ok {
		return "", ErrUnknownInstance
	}
	if err := managed.session.SendPasskeyResponse(ctx, webauthnJSON); err != nil {
		return "", err
	}
	return domain.InstancePairing, nil
}

// ConfirmPasskey confirms the handoff code was shown and acknowledged.
func (s *Supervisor) ConfirmPasskey(ctx context.Context, instanceID domain.ID) (domain.InstanceState, error) {
	managed, ok := s.lookup(instanceID)
	if !ok {
		return "", ErrUnknownInstance
	}
	if err := managed.session.ConfirmPasskey(ctx); err != nil {
		return "", err
	}

	s.record(ctx, instanceID, domain.ConnEventPasskeyConfirmed, domain.ReasonNone,
		map[string]any{"automatic": false})
	return domain.InstancePairing, nil
}

// IdentityVerificationCodes derives the safety numbers for a conversation.
func (s *Supervisor) IdentityVerificationCodes(ctx context.Context, instanceID domain.ID, contact string) (*wa.VerificationCodes, error) {
	managed, ok := s.lookup(instanceID)
	if !ok {
		return nil, ErrUnknownInstance
	}
	codes, err := managed.session.IdentityVerificationCodes(ctx, contact)
	if err != nil {
		return nil, err
	}
	return codes, nil
}

func (s *Supervisor) handlePairingCode(ctx context.Context, managed *managedSession, evt wa.Event) {
	snapshot := events.PairingSnapshot{
		Method:    string(evt.Method),
		Phase:     events.PhaseQR,
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
	if evt.StreamErrorCode != "" {
		// The code is the whole diagnostic value of an unknown stream error, and
		// it is all that gets kept: the node it arrived in is server-controlled
		// payload with no place in a queryable trail (research R9).
		detail["stream_error_code"] = evt.StreamErrorCode
	}
	s.record(ctx, instanceID, eventType, evt.Reason, detail)

	data := map[string]any{
		"reason":    string(evt.Reason),
		"permanent": evt.Permanent,
		"at":        time.Now().UTC(),
	}
	if evt.StreamErrorCode != "" {
		data["detail"] = map[string]any{"stream_error_code": evt.StreamErrorCode}
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
