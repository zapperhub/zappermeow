package wa

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/store"
	"github.com/polymorfa/hypermeow/store/sqlstore"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	waLog "github.com/polymorfa/hypermeow/util/log"

	"github.com/zapperhub/zappermeow/internal/domain"
)

// reconnectCeiling caps the library's linear backoff, which otherwise grows two
// seconds per attempt without bound. Jitter is applied on top so a thousand
// sessions dropped by the same network event do not reconnect in lockstep.
const reconnectCeiling = 60 * time.Second

// Container owns the HyperMeow device store and builds sessions from it.
type Container struct {
	container *sqlstore.Container
	logger    *slog.Logger
	waLogger  waLog.Logger
}

// NewContainer wires HyperMeow onto the pool the API already uses.
//
// stdlib.OpenDBFromPool wraps the existing pgx pool in a database/sql handle,
// so the library shares our connections instead of opening a second pool
// against the same database (Principle I, research R1). Upgrade applies the
// library's own migrations, versioned in whatsmeow_version and untouched by our
// golang-migrate schema.
func NewContainer(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) (*Container, error) {
	db := stdlib.OpenDBFromPool(pool)

	waLogger := slogAdapter{logger: logger.With(slog.String("component", "hypermeow"))}
	container := sqlstore.NewWithDB(db, "pgx", waLogger)
	if err := container.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("upgrade whatsmeow store: %w", err)
	}

	return &Container{container: container, logger: logger, waLogger: waLogger}, nil
}

// Close releases the container. The pgx pool is owned by the caller and stays
// open.
func (c *Container) Close() error {
	if err := c.container.Close(); err != nil {
		return fmt.Errorf("close whatsmeow container: %w", err)
	}
	return nil
}

// NewSession builds the session for an instance. A stored JID means existing
// device material to reconnect with; an empty one means the instance still has
// to be paired, and a fresh device is created for the attempt (research R9).
func (c *Container) NewSession(ctx context.Context, instanceID domain.ID, storedJID string) (Session, error) {
	var device *store.Device

	if storedJID != "" {
		jid, err := types.ParseJID(storedJID)
		if err != nil {
			return nil, fmt.Errorf("parse stored jid: %w", err)
		}
		device, err = c.container.GetDevice(ctx, jid)
		if err != nil {
			return nil, fmt.Errorf("load device: %w", err)
		}
		if device == nil {
			// The row vanished under us — the instance believes it is paired
			// but the material is gone. Pairing again is the only way out, and
			// pretending otherwise would loop forever on a dead session.
			return nil, fmt.Errorf("%w: no device material for %s", ErrNotPaired, storedJID)
		}
	} else {
		device = c.container.NewDevice()
	}

	session := &hypermeowSession{
		instanceID: instanceID,
		logger:     c.logger.With(slog.String("instance_id", instanceID.String())),
		events:     make(chan Event, 32),
	}

	client := whatsmeow.NewClient(device, c.waLogger)
	client.EnableAutoReconnect = true
	client.AutoReconnectHook = session.reconnectHook
	session.client = client
	session.handlerID = client.AddEventHandler(session.handleEvent)

	return session, nil
}

// hypermeowSession adapts one whatsmeow client to the Session interface.
type hypermeowSession struct {
	instanceID domain.ID
	client     *whatsmeow.Client
	logger     *slog.Logger
	handlerID  uint32

	mu     sync.Mutex
	events chan Event
	closed bool

	// permanent records that the last failure was an invalidation, which is how
	// reconnectHook knows to stop instead of retrying forever.
	permanent bool
}

func (s *hypermeowSession) QRChannel(ctx context.Context) (<-chan Event, error) {
	// GetQRChannel must run before Connect and refuses a store that already has
	// a JID; surfacing those as our own errors keeps callers from having to
	// know the library's rules.
	if s.client.IsConnected() {
		return nil, fmt.Errorf("%w: session is already connected", ErrAlreadyPaired)
	}
	if s.client.Store.ID != nil {
		return nil, ErrAlreadyPaired
	}

	raw, err := s.client.GetQRChannel(ctx)
	if err != nil {
		return nil, fmt.Errorf("open qr channel: %w", err)
	}

	out := make(chan Event, 8)
	go s.pumpQR(raw, out)
	return out, nil
}

// pumpQR translates the library's QR channel into our events.
//
// It must never block: HyperMeow drops the channel and disconnects the client
// when the consumer does not keep up, so a stall here does not slow pairing
// down — it kills it (research R2).
func (s *hypermeowSession) pumpQR(raw <-chan whatsmeow.QRChannelItem, out chan<- Event) {
	defer close(out)

	for item := range raw {
		var evt Event

		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			evt = Event{
				Kind:      KindPairingCode,
				Method:    MethodQR,
				Code:      item.Code,
				ExpiresAt: time.Now().Add(item.Timeout),
			}

		case "success":
			// The device identity is only complete once the store is updated,
			// which the library does before closing the channel.
			evt = Event{Kind: KindPairingSucceeded, Device: s.deviceIdentity()}

		case "timeout":
			evt = Event{Kind: KindPairingExpired, Method: MethodQR, Expiry: ExpiryWindowExhausted}

		case "err-scanned-without-multidevice":
			evt = Event{Kind: KindPairingFailed, Failure: FailureScannedWithoutMultidevice}

		case "err-client-outdated":
			evt = Event{Kind: KindPairingFailed, Failure: FailureClientOutdated}

		case "err-unexpected-state":
			evt = Event{Kind: KindPairingFailed, Failure: FailureUnexpectedState}

		case whatsmeow.QRChannelEventError:
			evt = Event{Kind: KindPairingFailed, Failure: FailurePairError}

		default:
			// Passkey events belong to a future slice; ignoring them keeps this
			// loop draining, which is what the library requires.
			continue
		}

		evt.OccurredAt = time.Now()
		select {
		case out <- evt:
		default:
			s.logger.Warn("dropping pairing event, consumer is not draining",
				slog.String("kind", string(evt.Kind)))
		}
	}
}

func (s *hypermeowSession) PairPhone(ctx context.Context, phoneNumber string) (string, time.Time, error) {
	if s.client.Store.ID != nil {
		return "", time.Time{}, ErrAlreadyPaired
	}

	// The library requires an open socket before requesting a code, unlike the
	// QR flow where the codes arrive over the pairing stream.
	if !s.client.IsConnected() {
		if err := s.client.Connect(); err != nil {
			return "", time.Time{}, fmt.Errorf("connect before phone pairing: %w", err)
		}
	}

	code, err := s.client.PairPhone(ctx, phoneNumber, true, whatsmeow.PairClientChrome, "ZapperMeow")
	if err != nil {
		if errors.Is(err, whatsmeow.ErrPhoneNumberTooShort) || errors.Is(err, whatsmeow.ErrPhoneNumberIsNotInternational) {
			return "", time.Time{}, fmt.Errorf("%w: %s", ErrInvalidPhoneNumber, err)
		}
		return "", time.Time{}, fmt.Errorf("request pairing code: %w", err)
	}

	expires := time.Now().Add(phonePairingWindow)
	s.emit(Event{
		Kind:      KindPairingCode,
		Method:    MethodPhone,
		Code:      code,
		ExpiresAt: expires,
	})
	return code, expires, nil
}

// phonePairingWindow is how long a phone pairing code stays usable. WhatsApp
// does not report a deadline, so this is the value shown to the tenant.
const phonePairingWindow = 3 * time.Minute

func (s *hypermeowSession) Connect(ctx context.Context) error {
	if s.client.IsConnected() {
		return nil
	}
	if err := s.client.ConnectContext(ctx); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	return nil
}

func (s *hypermeowSession) Disconnect() { s.client.Disconnect() }

func (s *hypermeowSession) Logout(ctx context.Context) (bool, error) {
	if s.client.Store.ID == nil {
		return false, ErrNotPaired
	}

	// Client.Logout sends an IQ and aborts without deleting anything when the
	// socket is down. Falling back to a local wipe keeps the platform's view
	// honest — the caller reports local_only so the tenant knows the device may
	// still be listed on the handset (research R10).
	err := s.client.Logout(ctx)
	if err == nil {
		return true, nil
	}

	s.logger.Warn("remote logout failed, deleting local session material",
		slog.String("error", err.Error()))
	s.client.Disconnect()
	if delErr := s.client.Store.Delete(ctx); delErr != nil {
		return false, fmt.Errorf("delete local session material: %w", delErr)
	}
	return false, nil
}

func (s *hypermeowSession) Events() <-chan Event { return s.events }

func (s *hypermeowSession) Status() Status {
	return Status{
		Connected: s.client.IsConnected(),
		LoggedIn:  s.client.IsLoggedIn(),
		Device:    s.deviceIdentity(),
	}
}

func (s *hypermeowSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}
	s.closed = true
	s.client.RemoveEventHandler(s.handlerID)
	s.client.Disconnect()
	close(s.events)
}

// handleEvent translates the library's events into ours.
func (s *hypermeowSession) handleEvent(raw any) {
	switch raw.(type) {
	case *events.Connected:
		s.setPermanent(false)
		s.emit(Event{Kind: KindConnected})

	case *events.PairSuccess:
		s.emit(Event{Kind: KindPairingSucceeded, Device: s.deviceIdentity()})

	case *events.PairError:
		s.emit(Event{Kind: KindPairingFailed, Failure: FailurePairError})

	case *events.QRScannedWithoutMultidevice:
		s.emit(Event{Kind: KindPairingFailed, Failure: FailureScannedWithoutMultidevice})

	default:
		classification, ok := ClassifyDisconnect(raw)
		if !ok {
			return
		}
		if classification.IsAlarm() {
			// Two owners of one session: the lease failed, or something is
			// running this device outside the platform. Either way it is an
			// incident, not telemetry (Principle III).
			s.logger.Error("session replaced elsewhere: exclusive ownership violated",
				slog.String("reason", string(classification.Reason)))
		}
		s.setPermanent(classification.Permanent)
		s.emit(Event{
			Kind:         KindDisconnected,
			Reason:       classification.Reason,
			Permanent:    classification.Permanent,
			BanExpiresAt: classification.BanExpiresAt,
		})
	}
}

// reconnectHook runs when the library's automatic reconnection fails. Returning
// false stops it for good.
func (s *hypermeowSession) reconnectHook(err error) bool {
	s.mu.Lock()
	permanent := s.permanent
	s.mu.Unlock()

	if permanent {
		s.logger.Info("not reconnecting: last failure was permanent")
		return false
	}

	// The library's delay is AutoReconnectErrors * 2s and grows without bound.
	// Capping it keeps a long outage from stretching to hours between attempts,
	// and the jitter spreads a fleet-wide reconnection over the window instead
	// of stacking every session on the same instant.
	attempts := s.client.AutoReconnectErrors
	delay := min(time.Duration(attempts)*2*time.Second, reconnectCeiling)
	jitter := time.Duration(rand.Int64N(int64(delay/4 + 1)))

	s.logger.Debug("scheduling reconnect",
		slog.Int("attempts", attempts),
		slog.Duration("delay", delay+jitter),
		slog.String("error", err.Error()))

	time.Sleep(jitter)
	return true
}

func (s *hypermeowSession) setPermanent(permanent bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.permanent = permanent
}

func (s *hypermeowSession) emit(evt Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}
	if evt.OccurredAt.IsZero() {
		evt.OccurredAt = time.Now()
	}
	select {
	case s.events <- evt:
	default:
		s.logger.Warn("dropping session event, consumer is not draining",
			slog.String("kind", string(evt.Kind)))
	}
}

// deviceIdentity reads the paired device from the store, keeping the device
// suffix in the JID: that suffix is what distinguishes two instances of the
// same phone number.
func (s *hypermeowSession) deviceIdentity() *domain.DeviceIdentity {
	device := s.client.Store
	if device == nil || device.ID == nil {
		return nil
	}

	identity := &domain.DeviceIdentity{
		JID:          device.ID.String(),
		PhoneNumber:  device.ID.User,
		PushName:     device.PushName,
		Platform:     device.Platform,
		BusinessName: device.BusinessName,
		PairedAt:     time.Now(),
	}
	if !device.LID.IsEmpty() {
		identity.LID = device.LID.String()
	}
	return identity
}

// ErrInvalidPhoneNumber is returned for a number the library refuses before
// contacting WhatsApp.
var ErrInvalidPhoneNumber = errors.New("wa: invalid phone number")

// slogAdapter bridges HyperMeow's logger onto slog, so library output lands in
// the same structured stream as everything else (Principle VI).
type slogAdapter struct {
	logger *slog.Logger
	module string
}

func (a slogAdapter) Warnf(msg string, args ...any)  { a.logger.Warn(fmt.Sprintf(msg, args...)) }
func (a slogAdapter) Errorf(msg string, args ...any) { a.logger.Error(fmt.Sprintf(msg, args...)) }
func (a slogAdapter) Infof(msg string, args ...any)  { a.logger.Info(fmt.Sprintf(msg, args...)) }
func (a slogAdapter) Debugf(msg string, args ...any) { a.logger.Debug(fmt.Sprintf(msg, args...)) }

func (a slogAdapter) Sub(module string) waLog.Logger {
	return slogAdapter{logger: a.logger.With(slog.String("module", module)), module: module}
}

var (
	_ Session      = (*hypermeowSession)(nil)
	_ waLog.Logger = slogAdapter{}
)
