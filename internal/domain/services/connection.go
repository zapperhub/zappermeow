package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/zapperhub/zappermeow/internal/api/sessionclient"
	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/events"
	"github.com/zapperhub/zappermeow/internal/lease"
	sessionv1 "github.com/zapperhub/zappermeow/internal/pb/sessionv1"
	"github.com/zapperhub/zappermeow/internal/store"
)

// ConnectionService drives a session from the stateless plane.
//
// It never touches WhatsApp: it records the user's intent, then either reaches
// the worker that owns the session or wakes the fleet to claim it. That split
// is what keeps the API horizontally scalable while sessions stay pinned to one
// process (principle III).
type ConnectionService struct {
	queries   *store.Queries
	leases    *lease.Manager
	sessions  *sessionclient.Client
	publisher *events.Publisher
	logger    *slog.Logger
	// claimWait bounds how long a command waits for a worker to take the
	// session before reporting no capacity.
	claimWait time.Duration
}

// NewConnectionService builds the service.
func NewConnectionService(
	queries *store.Queries,
	leases *lease.Manager,
	sessions *sessionclient.Client,
	publisher *events.Publisher,
	logger *slog.Logger,
	claimWait time.Duration,
) *ConnectionService {
	return &ConnectionService{
		queries:   queries,
		leases:    leases,
		sessions:  sessions,
		publisher: publisher,
		logger:    logger,
		claimWait: claimWait,
	}
}

// ConnectResult is what the API reports back after a connect command.
type ConnectResult struct {
	State          domain.InstanceState
	Intent         domain.ConnectionIntent
	PairingStarted bool
	PairingExpires *string
}

// Connect records the intent to be online and gets a worker moving.
//
// The response is deliberately not the end state: pairing needs a human with a
// phone, and a reconnection needs a handshake. The caller learns the outcome
// through the event channel, which is why this answers 202 rather than blocking.
func (s *ConnectionService) Connect(ctx context.Context, instanceID domain.ID) (ConnectResult, error) {
	if err := s.leases.Ensure(ctx, instanceID); err != nil {
		return ConnectResult{}, domain.ErrInternal(err)
	}

	// Clearing the disconnect reason is what re-enables automatic adoption
	// after an invalidation: without it, reconciliation keeps skipping the
	// instance and an explicit command would do nothing (FR-031).
	if _, err := s.queries.SetConnectionIntent(ctx, store.SetConnectionIntentParams{
		ID:               instanceID,
		ConnectionIntent: string(domain.IntentActive),
		ClearReason:      true,
	}); err != nil {
		return ConnectResult{}, domain.ErrInternal(err)
	}

	if err := s.leases.SetDesired(ctx, instanceID, lease.DesiredRunning); err != nil {
		return ConnectResult{}, domain.ErrInternal(err)
	}

	resp, err := s.sessions.Connect(ctx, instanceID)
	if err != nil {
		if errors.Is(err, sessionclient.ErrNoOwner) {
			// Nobody holds the session yet. Waking the fleet is only half the
			// job: adopting a lease does not start a pairing window, so the
			// command has to be delivered once a worker has the session.
			// Returning here would leave the tenant watching an empty channel
			// for a QR nobody was ever asked to produce.
			resp, err = s.claimAndRetryConnect(ctx, instanceID)
			if err != nil {
				return ConnectResult{}, err
			}
		} else {
			return ConnectResult{}, translateSessionError(err)
		}
	}

	result := ConnectResult{
		State:          stateFromProto(resp.GetState()),
		Intent:         domain.IntentActive,
		PairingStarted: resp.GetPairingStarted(),
	}
	if resp.GetPairingExpiresAt() != nil {
		expires := resp.GetPairingExpiresAt().AsTime().UTC().Format("2006-01-02T15:04:05Z07:00")
		result.PairingExpires = &expires
	}
	return result, nil
}

// Disconnect takes the instance offline while keeping its pairing.
func (s *ConnectionService) Disconnect(ctx context.Context, instanceID domain.ID) (domain.InstanceState, error) {
	if err := s.leases.Ensure(ctx, instanceID); err != nil {
		return "", domain.ErrInternal(err)
	}
	if _, err := s.queries.SetConnectionIntent(ctx, store.SetConnectionIntentParams{
		ID:               instanceID,
		ConnectionIntent: string(domain.IntentStopped),
	}); err != nil {
		return "", domain.ErrInternal(err)
	}
	if err := s.leases.SetDesired(ctx, instanceID, lease.DesiredStopped); err != nil {
		return "", domain.ErrInternal(err)
	}

	if _, err := s.sessions.Disconnect(ctx, instanceID); err != nil {
		if errors.Is(err, sessionclient.ErrNoOwner) {
			// No owner means nothing is running, which is the requested
			// outcome. Reporting an error would make a harmless repeat look
			// like a failure (FR-008).
			if err := s.setStateDirect(ctx, instanceID, domain.InstanceDisconnected); err != nil {
				return "", err
			}
			return domain.InstanceDisconnected, nil
		}
		return "", translateSessionError(err)
	}
	return domain.InstanceDisconnected, nil
}

// LogoutResult reports what the logout actually achieved.
type LogoutResult struct {
	State domain.InstanceState
	// RemoteRemoved is false when only the local material could be deleted: the
	// device may still be listed on the customer's handset.
	RemoteRemoved bool
}

// Logout ends the session on WhatsApp and deletes the local material.
func (s *ConnectionService) Logout(ctx context.Context, instanceID domain.ID) (LogoutResult, error) {
	instance, err := s.queries.GetInstanceConnectionByID(ctx, instanceID)
	if err != nil {
		if store.IsNoRows(err) {
			return LogoutResult{}, domain.ErrNotFound()
		}
		return LogoutResult{}, domain.ErrInternal(err)
	}
	if instance.WaJid == nil {
		// Nothing paired: already the requested end state.
		return LogoutResult{State: domain.InstanceRegistered, RemoteRemoved: false}, nil
	}

	if _, err := s.queries.SetConnectionIntent(ctx, store.SetConnectionIntentParams{
		ID:               instanceID,
		ConnectionIntent: string(domain.IntentStopped),
	}); err != nil {
		return LogoutResult{}, domain.ErrInternal(err)
	}

	resp, err := s.sessions.Logout(ctx, instanceID, true)
	if err != nil {
		if errors.Is(err, sessionclient.ErrNoOwner) {
			// Offline logout: no worker holds the session, so the material is
			// dropped here and the device stays listed on the handset.
			if err := s.queries.ClearDeviceIdentity(ctx, instanceID); err != nil {
				return LogoutResult{}, domain.ErrInternal(err)
			}
			_ = s.leases.SetDesired(ctx, instanceID, lease.DesiredStopped)
			return LogoutResult{State: domain.InstanceRegistered, RemoteRemoved: false}, nil
		}
		return LogoutResult{}, translateSessionError(err)
	}

	_ = s.leases.SetDesired(ctx, instanceID, lease.DesiredStopped)
	return LogoutResult{
		State:         domain.InstanceRegistered,
		RemoteRemoved: resp.GetRemoteRemoved(),
	}, nil
}

// PairPhoneResult carries the code to type on the handset.
type PairPhoneResult struct {
	Code      string
	ExpiresAt string
}

// PairPhone starts a phone-code pairing attempt.
func (s *ConnectionService) PairPhone(ctx context.Context, instanceID domain.ID, phoneNumber string, replaceActive bool) (PairPhoneResult, error) {
	if err := s.leases.Ensure(ctx, instanceID); err != nil {
		return PairPhoneResult{}, domain.ErrInternal(err)
	}
	if _, err := s.queries.SetConnectionIntent(ctx, store.SetConnectionIntentParams{
		ID:               instanceID,
		ConnectionIntent: string(domain.IntentActive),
		ClearReason:      true,
	}); err != nil {
		return PairPhoneResult{}, domain.ErrInternal(err)
	}
	if err := s.leases.SetDesired(ctx, instanceID, lease.DesiredRunning); err != nil {
		return PairPhoneResult{}, domain.ErrInternal(err)
	}

	resp, err := s.sessions.PairPhone(ctx, instanceID, phoneNumber, replaceActive)
	if err != nil {
		if errors.Is(err, sessionclient.ErrNoOwner) {
			// Unlike connect, this command cannot answer 202 and finish later:
			// it must return a code the caller types on the handset. So it wakes
			// the fleet and waits for someone to take the session — an instance
			// that was never connected has no owner yet, and giving up here
			// would make the very first pairing attempt fail every time.
			resp, err = s.claimAndRetryPairPhone(ctx, instanceID, phoneNumber, replaceActive)
			if err != nil {
				return PairPhoneResult{}, err
			}
		} else {
			return PairPhoneResult{}, translateSessionError(err)
		}
	}

	return PairPhoneResult{
		Code:      resp.GetPairingCode(),
		ExpiresAt: resp.GetExpiresAt().AsTime().UTC().Format("2006-01-02T15:04:05Z07:00"),
	}, nil
}

// Terminate ends a session because its instance is being removed.
//
// It logs the device out so the companion device disappears from the customer's
// handset, then stops the lease. Both steps are best-effort: the deletion must
// not be held hostage by an unreachable worker or an offline session, or a
// tenant could end up unable to delete an instance at all (FR-007).
func (s *ConnectionService) Terminate(ctx context.Context, instanceID domain.ID) error {
	if _, err := s.sessions.Logout(ctx, instanceID, true); err != nil && !errors.Is(err, sessionclient.ErrNoOwner) {
		return fmt.Errorf("logout before deletion: %w", err)
	}
	if err := s.leases.SetDesired(ctx, instanceID, lease.DesiredStopped); err != nil {
		return fmt.Errorf("stop lease before deletion: %w", err)
	}
	// Whoever is watching the channel learns the instance is gone rather than
	// waiting on a socket that will never speak again.
	if err := s.publisher.Stop(ctx, instanceID); err != nil {
		return fmt.Errorf("signal stop before deletion: %w", err)
	}
	return nil
}

// claimAndRetryConnect wakes the fleet and delivers the command once a worker
// owns the session.
func (s *ConnectionService) claimAndRetryConnect(ctx context.Context, instanceID domain.ID) (*sessionv1.ConnectResponse, error) {
	if err := s.publisher.Claim(ctx, instanceID); err != nil {
		return nil, domain.ErrInternal(err)
	}

	deadline := time.Now().Add(s.claimWait)
	for {
		resp, err := s.sessions.Connect(ctx, instanceID)
		if err == nil {
			return resp, nil
		}
		if !errors.Is(err, sessionclient.ErrNoOwner) {
			return nil, translateSessionError(err)
		}
		if time.Now().After(deadline) {
			return nil, domain.ErrSessionUnavailable()
		}

		select {
		case <-ctx.Done():
			return nil, domain.ErrSessionUnavailable()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// claimAndRetryPairPhone wakes the fleet and retries once a worker has the
// session. Failing to find one within the window is the honest
// SESSION_UNAVAILABLE: there really is no capacity right now.
func (s *ConnectionService) claimAndRetryPairPhone(
	ctx context.Context,
	instanceID domain.ID,
	phoneNumber string,
	replaceActive bool,
) (*sessionv1.PairPhoneResponse, error) {
	if err := s.publisher.Claim(ctx, instanceID); err != nil {
		return nil, domain.ErrInternal(err)
	}

	deadline := time.Now().Add(s.claimWait)
	for {
		resp, err := s.sessions.PairPhone(ctx, instanceID, phoneNumber, replaceActive)
		if err == nil {
			return resp, nil
		}
		if !errors.Is(err, sessionclient.ErrNoOwner) {
			return nil, translateSessionError(err)
		}
		if time.Now().After(deadline) {
			return nil, domain.ErrSessionUnavailable()
		}

		select {
		case <-ctx.Done():
			return nil, domain.ErrSessionUnavailable()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// setStateDirect writes a state the API itself decided, used when no worker is
// involved.
func (s *ConnectionService) setStateDirect(ctx context.Context, instanceID domain.ID, state domain.InstanceState) error {
	err := s.queries.SetConnectionState(ctx, store.SetConnectionStateParams{
		ID:              instanceID,
		ConnectionState: string(state),
	})
	if err != nil {
		return domain.ErrInternal(err)
	}
	return nil
}

// translateSessionError maps the worker's gRPC codes onto domain errors, which
// httperr then renders as problem details.
func translateSessionError(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return domain.ErrInternal(err)
	}

	// The detail string carries the reason; the code alone cannot tell an
	// upstream failure from a platform one.
	switch {
	case strings.HasPrefix(st.Message(), "UPSTREAM_FAILURE"):
		return domain.ErrWhatsAppUnavailable()
	case st.Message() == "UNKNOWN_INSTANCE":
		// The lease says this worker owns the session, but it has none in
		// memory — a stale ownership record, not a missing worker.
		return domain.ErrSessionNotRunning()
	}

	switch st.Message() {
	case "NOT_PAIRED":
		return domain.ErrInstanceNotPaired()
	case "ALREADY_PAIRED":
		return domain.ErrAlreadyPaired()
	case "INVALID_PHONE_NUMBER":
		return domain.ErrInvalidPhoneNumber()
	case "PAIRING_IN_PROGRESS":
		return domain.ErrPairingInProgress()
	}

	switch st.Code() {
	case codes.Unavailable:
		return domain.ErrSessionUnavailable()
	case codes.FailedPrecondition:
		return domain.ErrSessionUnavailable()
	default:
		return domain.ErrInternal(fmt.Errorf("session command failed: %w", err))
	}
}

func stateFromProto(state sessionv1.SessionState) domain.InstanceState {
	switch state {
	case sessionv1.SessionState_SESSION_STATE_REGISTERED:
		return domain.InstanceRegistered
	case sessionv1.SessionState_SESSION_STATE_PAIRING:
		return domain.InstancePairing
	case sessionv1.SessionState_SESSION_STATE_CONNECTING:
		return domain.InstanceConnecting
	case sessionv1.SessionState_SESSION_STATE_CONNECTED:
		return domain.InstanceConnected
	case sessionv1.SessionState_SESSION_STATE_DISCONNECTED:
		return domain.InstanceDisconnected
	case sessionv1.SessionState_SESSION_STATE_LOGGED_OUT:
		return domain.InstanceLoggedOut
	case sessionv1.SessionState_SESSION_STATE_BANNED:
		return domain.InstanceBanned
	default:
		return domain.InstanceConnecting
	}
}

// ConnectionStatus is everything a tenant can ask about a session without
// touching the worker: the database is the authority here.
type ConnectionStatus struct {
	InstanceID       domain.ID
	State            domain.InstanceState
	Intent           domain.ConnectionIntent
	ConnectedAt      *time.Time
	Device           *domain.DeviceIdentity
	LastDisconnectAt *time.Time
	LastReason       domain.DisconnectReason
	BanExpiresAt     *time.Time
	// SharesNumberWith lists sibling instances paired to the same number.
	// Several companion devices of one number is legitimate, so this is
	// context, never a conflict (FR-018).
	SharesNumberWith []domain.ID
}

// Status reads the connection state of an instance.
func (s *ConnectionService) Status(ctx context.Context, tenantID, instanceID domain.ID) (ConnectionStatus, error) {
	row, err := s.queries.GetInstanceConnection(ctx, store.GetInstanceConnectionParams{
		ID:       instanceID,
		TenantID: tenantID,
	})
	if err != nil {
		if store.IsNoRows(err) {
			return ConnectionStatus{}, domain.ErrNotFound()
		}
		return ConnectionStatus{}, domain.ErrInternal(err)
	}

	status := ConnectionStatus{
		InstanceID:       instanceID,
		State:            domain.InstanceState(row.ConnectionState),
		Intent:           domain.ConnectionIntent(row.ConnectionIntent),
		ConnectedAt:      row.ConnectedAt,
		LastDisconnectAt: row.LastDisconnectAt,
		BanExpiresAt:     row.BanExpiresAt,
	}
	if row.LastDisconnectReason != nil {
		status.LastReason = domain.DisconnectReason(*row.LastDisconnectReason)
	}

	if row.WaJid != nil {
		device := &domain.DeviceIdentity{JID: *row.WaJid}
		device.LID = deref(row.WaLid)
		device.PhoneNumber = deref(row.PhoneNumber)
		device.PushName = deref(row.PushName)
		device.Platform = deref(row.Platform)
		device.BusinessName = deref(row.BusinessName)
		if row.PairedAt != nil {
			device.PairedAt = *row.PairedAt
		}
		status.Device = device
	}

	if row.PhoneNumber != nil && *row.PhoneNumber != "" {
		siblings, err := s.queries.ListInstancesSharingNumber(ctx, store.ListInstancesSharingNumberParams{
			TenantID:    tenantID,
			PhoneNumber: row.PhoneNumber,
			ID:          instanceID,
		})
		if err != nil {
			return ConnectionStatus{}, domain.ErrInternal(err)
		}
		status.SharesNumberWith = append(status.SharesNumberWith, siblings...)
	}

	return status, nil
}

// EventPage is one page of the connection trail.
type EventPage struct {
	Events []domain.ConnectionEvent
	// NextBefore is the cursor for the following page, empty when the trail
	// ends here.
	NextBefore string
}

// maxTrailPage bounds a single page, so a client cannot ask for the whole trail
// in one request.
const maxTrailPage = 200

// Events reads the connection trail, newest first.
func (s *ConnectionService) Events(ctx context.Context, tenantID, instanceID domain.ID, limit int32, before *int64, types []string) (EventPage, error) {
	// Ownership is checked before anything is read: another tenant's trail must
	// be as invisible as one that never existed.
	if _, err := s.queries.GetInstanceConnection(ctx, store.GetInstanceConnectionParams{
		ID:       instanceID,
		TenantID: tenantID,
	}); err != nil {
		if store.IsNoRows(err) {
			return EventPage{}, domain.ErrNotFound()
		}
		return EventPage{}, domain.ErrInternal(err)
	}

	if limit < 1 {
		limit = 50
	}
	if limit > maxTrailPage {
		limit = maxTrailPage
	}

	rows, err := s.queries.ListConnectionEvents(ctx, store.ListConnectionEventsParams{
		InstanceID: instanceID,
		BeforeID:   before,
		Types:      types,
		MaxRows:    limit,
	})
	if err != nil {
		return EventPage{}, domain.ErrInternal(err)
	}

	page := EventPage{Events: make([]domain.ConnectionEvent, 0, len(rows))}
	for _, row := range rows {
		event := domain.ConnectionEvent{
			ID:         row.ID,
			InstanceID: row.InstanceID,
			Type:       domain.ConnectionEventType(row.Type),
			OccurredAt: row.OccurredAt,
		}
		if row.Reason != nil {
			event.Reason = domain.DisconnectReason(*row.Reason)
		}
		if len(row.Detail) > 0 {
			detail := map[string]any{}
			if err := json.Unmarshal(row.Detail, &detail); err == nil {
				event.Detail = detail
			}
		}
		page.Events = append(page.Events, event)
	}

	// A full page implies there may be more; the cursor is the last id seen.
	if int32(len(rows)) == limit && len(rows) > 0 {
		page.NextBefore = encodeCursor(rows[len(rows)-1].ID)
	}
	return page, nil
}

// encodeCursor hides the row id behind an opaque token, so pagination stays a
// contract about ordering rather than about our primary keys.
func encodeCursor(id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(id, 10)))
}

// DecodeCursor reverses encodeCursor. A malformed cursor is a client error, not
// a silent restart from the top — that would loop a paginating client forever.
func DecodeCursor(raw string) (int64, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, domain.ErrValidation("query.before", "cursor is not valid")
	}
	id, err := strconv.ParseInt(string(decoded), 10, 64)
	if err != nil {
		return 0, domain.ErrValidation("query.before", "cursor is not valid")
	}
	return id, nil
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
