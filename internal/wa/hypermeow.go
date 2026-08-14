package wa

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/proto/waCompanionReg"
	"github.com/polymorfa/hypermeow/store"
	"github.com/polymorfa/hypermeow/store/sqlstore"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	waLog "github.com/polymorfa/hypermeow/util/log"
	"google.golang.org/protobuf/proto"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/metrics"
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
func NewContainer(ctx context.Context, pool *pgxpool.Pool, deviceName string, logger *slog.Logger) (*Container, error) {
	// These properties are registered with WhatsApp at pairing time and are what
	// the customer sees in "Linked devices" on their phone. The library defaults
	// to an unknown platform, which the handset renders as "Other device" — a
	// number the tenant cannot identify is a number they cannot safely unlink.
	//
	// PlatformType also decides the client type sent in a phone-code request,
	// so it must stay consistent with the display name used there.
	if deviceName != "" {
		store.DeviceProps.Os = proto.String(deviceName)
	}
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_CHROME.Enum()

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

// SessionConfig is the per-instance configuration a session is built with.
// Everything here is fixed for the lifetime of the client: the library applies
// the proxy when the socket is created, so changing it means building a new
// session, not mutating this one (research R1).
type SessionConfig struct {
	// StoredJID is the device material to reconnect with; empty means the
	// instance still has to be paired.
	StoredJID string
	// ProxyURL is the egress proxy for this instance, empty for direct.
	ProxyURL string
}

// NewSession builds the session for an instance. A stored JID means existing
// device material to reconnect with; an empty one means the instance still has
// to be paired, and a fresh device is created for the attempt (research R9).
func (c *Container) NewSession(ctx context.Context, instanceID domain.ID, cfg SessionConfig) (Session, error) {
	var device *store.Device

	storedJID := cfg.StoredJID
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
			// The instance believes it is paired but the store has nothing —
			// a remote logout the platform did not record, or material removed
			// out of band. Refusing here would strand the instance forever;
			// starting a fresh device lets a connect command pair it again,
			// which is the only way back.
			c.logger.Warn("device material is missing; starting a fresh device",
				slog.String("instance_id", instanceID.String()),
				slog.String("stored_jid", storedJID))
			device = c.container.NewDevice()
		}
	} else {
		device = c.container.NewDevice()
	}

	session := &hypermeowSession{
		instanceID:      instanceID,
		logger:          c.logger.With(slog.String("instance_id", instanceID.String())),
		events:          make(chan Event, 32),
		proxyConfigured: cfg.ProxyURL != "",
	}

	client := whatsmeow.NewClient(device, c.waLogger)
	client.EnableAutoReconnect = true
	client.AutoReconnectHook = session.reconnectHook

	// One of these two calls always runs, and that is the point. Left alone the
	// library resolves a proxy from the environment, so a worker started with
	// https_proxy set would route every tenant that configured nothing through
	// a proxy nobody asked for. SetProxy(nil) is what makes "no proxy" mean
	// direct (FR-006, research R1).
	//
	// Both must happen before Connect: for the websocket the proxy is read when
	// the socket is built, so changing it later means building a new session.
	if cfg.ProxyURL != "" {
		if err := client.SetProxyAddress(cfg.ProxyURL); err != nil {
			// The address was validated when it was stored, so reaching here
			// means the stored value and the dialer disagree — worth failing
			// loudly rather than silently connecting direct.
			return nil, fmt.Errorf("apply proxy: %w", err)
		}
	} else {
		client.SetProxy(nil)
	}

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

	// proxyConfigured marks sessions whose traffic must go through a proxy. It
	// is what separates "the network is down" from "the tenant's proxy is
	// down", which are the same error to the dialer and very different
	// diagnoses to the tenant (research R3).
	proxyConfigured bool

	// passkeyPhase tracks where the passkey step of the current attempt is, so
	// commands arriving out of order are refused here with a clear reason
	// instead of reaching a library that is not reentrant (research R7).
	passkeyPhase passkeyPhase
}

// passkeyPhase is the state of the passkey step within a pairing attempt.
type passkeyPhase int

const (
	passkeyNone passkeyPhase = iota
	// passkeyAwaitingResponse: a challenge was published, no assertion yet.
	passkeyAwaitingResponse
	// passkeyAwaitingConfirm: a handoff code was published, no confirmation yet.
	passkeyAwaitingConfirm
)

func (s *hypermeowSession) setPasskeyPhase(phase passkeyPhase) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.passkeyPhase = phase
}

func (s *hypermeowSession) passkeyInFlight() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.passkeyPhase != passkeyNone
}

// marshalPasskeyChallenge encodes the library's WebAuthn public key options for
// the tenant's authenticator. The platform passes it through untouched: only
// the authenticator can answer it, and parsing it here would buy nothing while
// coupling the contract to the library's struct.
func marshalPasskeyChallenge(req *events.PairPasskeyRequest) (json.RawMessage, error) {
	if req == nil || req.PublicKey == nil {
		return nil, errors.New("passkey request carries no public key")
	}
	raw, err := json.Marshal(req.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("marshal webauthn public key: %w", err)
	}
	return raw, nil
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

		case whatsmeow.QRChannelEventPasskeyRequest:
			// WhatsApp requires the passkey step. Entering it rotates the secret
			// that validates the QR codes already on screen, so no further code
			// arrives for this attempt and the ones displayed are dead — the
			// contract tells the client to stop showing them (research R7).
			challenge, err := marshalPasskeyChallenge(item.PasskeyRequest)
			if err != nil {
				s.logger.Error("passkey challenge could not be encoded",
					slog.String("error", err.Error()))
				evt = Event{Kind: KindPairingFailed, Failure: FailurePasskeyError}
				break
			}
			s.setPasskeyPhase(passkeyAwaitingResponse)
			evt = Event{Kind: KindPasskeyChallenge, Challenge: challenge}

		case whatsmeow.QRChannelEventPasskeyResponse:
			// Only reaches us when the confirmation is not automatic: with a
			// valid handoff proof the library confirms on its own and emits
			// nothing, which is why the platform has no auto-confirm path of
			// its own (research R7).
			var code string
			if item.PasskeyConfirmation != nil {
				code = item.PasskeyConfirmation.Code
			}
			s.setPasskeyPhase(passkeyAwaitingConfirm)
			evt = Event{Kind: KindPasskeyCode, Code: code}

		case whatsmeow.QRChannelEventError:
			// Passkey failures arrive on this same item. Telling them apart
			// matters to the tenant: "the code did not match" and "the QR was
			// refused" call for different next steps.
			failure := FailurePairError
			if s.passkeyInFlight() {
				failure = FailurePasskeyError
			}
			evt = Event{Kind: KindPairingFailed, Failure: failure}

		default:
			// Unknown item types must still leave the loop draining: the
			// library closes the channel and drops the client when the consumer
			// falls behind (research R2).
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
		if !s.client.WaitForConnection(connectWait) {
			return "", time.Time{}, fmt.Errorf("connect before phone pairing: socket did not come up in %s", connectWait)
		}
		// Connect returns once the handshake is done, but the server only
		// accepts a pairing request after the login socket is fully
		// established — which it signals by sending the first QR ref. The
		// library's own guidance is to wait for that event or to pause briefly;
		// asking too early is answered with a 400.
		time.Sleep(pairingSettle)
	}

	code, err := s.client.PairPhone(ctx, phoneNumber, true, whatsmeow.PairClientChrome, pairDisplayName)
	if err != nil {
		if errors.Is(err, whatsmeow.ErrPhoneNumberTooShort) || errors.Is(err, whatsmeow.ErrPhoneNumberIsNotInternational) {
			// Both errors matter: the sentinel drives the HTTP code, the
			// library's message says which rule the number broke.
			return "", time.Time{}, errors.Join(ErrInvalidPhoneNumber, err)
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

// pairDisplayName is what the handset shows next to the linked device.
//
// It is NOT free text: WhatsApp validates it against a list of common
// browser/OS pairs and answers 400 to anything else — a product name here is
// rejected outright. It must stay consistent with the PairClient* constant sent
// alongside it, which is why both say Chrome.
const pairDisplayName = "Chrome (Linux)"

// connectWait bounds how long we wait for the login socket, and pairingSettle
// is the pause the library recommends before requesting a code: the server
// rejects a request that arrives before the session is fully established.
const (
	connectWait   = 10 * time.Second
	pairingSettle = 1500 * time.Millisecond
)

func (s *hypermeowSession) Connect(ctx context.Context) error {
	if s.client.IsConnected() {
		return nil
	}
	if err := s.client.ConnectContext(ctx); err != nil {
		if s.proxyConfigured {
			// The dialer reports the same error whether the proxy or the
			// destination is unreachable, and the platform cannot tell them
			// apart from here. Naming it as a proxy failure is the honest
			// reading: with a proxy configured, every byte goes through it, so
			// it is the first thing to check — and there is deliberately no
			// direct fallback to mask the problem (FR-005).
			metrics.ProxyConnectFailures.Inc()
			return fmt.Errorf("%w: %w", ErrProxyConnectFailed, err)
		}
		return fmt.Errorf("connect: %w", err)
	}
	return nil
}

// SetPassive tells the server whether this device announces itself as active.
//
// The library restores active mode on every connection, in the goroutine that
// runs after authentication and before it dispatches Connected, so the caller
// must reapply after each Connected rather than once. The ordering also removes
// the race: our call is dispatched from an event the library only emits after
// its own SetPassive(false) has run (research R6).
func (s *hypermeowSession) SetPassive(ctx context.Context, passive bool) error {
	if !s.client.IsConnected() {
		return ErrNotConnected
	}
	if err := s.client.SetPassive(ctx, passive); err != nil {
		return fmt.Errorf("set passive mode: %w", err)
	}
	return nil
}

func (s *hypermeowSession) SendPasskeyResponse(ctx context.Context, webauthnResponseJSON []byte) error {
	s.mu.Lock()
	phase := s.passkeyPhase
	s.mu.Unlock()

	if phase != passkeyAwaitingResponse {
		return ErrNoPasskeyChallenge
	}

	var response types.WebAuthnResponse
	if err := json.Unmarshal(webauthnResponseJSON, &response); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidPasskeyResponse, err)
	}

	if err := s.client.SendPasskeyResponse(ctx, &response); err != nil {
		return fmt.Errorf("send passkey response: %w", err)
	}
	// The challenge is spent either way; what happens next arrives as an event
	// (a handoff code, or the pairing simply succeeding).
	s.setPasskeyPhase(passkeyNone)
	return nil
}

func (s *hypermeowSession) ConfirmPasskey(ctx context.Context) error {
	s.mu.Lock()
	phase := s.passkeyPhase
	s.mu.Unlock()

	// The library clears its linking cache on success, so a second call fails
	// with an opaque message. Checking here turns a double submit into a clear
	// answer and keeps the attempt intact (research R7).
	if phase != passkeyAwaitingConfirm {
		return ErrNoPasskeyCode
	}

	if err := s.client.SendPasskeyConfirmation(ctx); err != nil {
		return fmt.Errorf("send passkey confirmation: %w", err)
	}
	s.setPasskeyPhase(passkeyNone)
	return nil
}

func (s *hypermeowSession) IdentityVerificationCodes(ctx context.Context, contact string) (*VerificationCodes, error) {
	if !s.client.IsConnected() {
		return nil, ErrNotConnected
	}

	target, err := s.resolveContactLID(ctx, contact)
	if err != nil {
		return nil, err
	}

	codes, err := s.client.GetIdentityVerificationCodes(ctx, target)
	if err != nil {
		return nil, translateVerificationError(err)
	}

	return &VerificationCodes{
		LID:            codes.UserID.String(),
		PhoneNumber:    phoneUserOrEmpty(codes.PhoneNumber),
		Username:       codes.Username,
		NumericCode:    codes.NumericCode,
		DisplayQR:      codes.DisplayQRCode,
		VerificationQR: codes.VerificationQRCode,
	}, nil
}

// resolveContactLID turns what the tenant sent into the LID the library
// requires. Phone numbers are resolved through mappings this session already
// learned; discovering an identity over the network belongs to the contacts
// slice, and guessing one here would produce a verification code for the wrong
// person (research R8).
func (s *hypermeowSession) resolveContactLID(ctx context.Context, contact string) (types.JID, error) {
	if contact == "" {
		return types.EmptyJID, ErrInvalidContact
	}

	if strings.HasSuffix(contact, "@"+types.HiddenUserServer) {
		jid, err := types.ParseJID(contact)
		if err != nil {
			return types.EmptyJID, ErrInvalidContact
		}
		return jid.ToNonAD(), nil
	}

	// Anything else is treated as a phone number in the same format the pairing
	// flow accepts.
	if !isPlainPhoneNumber(contact) {
		return types.EmptyJID, ErrInvalidContact
	}
	pn := types.NewJID(contact, types.DefaultUserServer)

	lidStore := s.client.Store.LIDs
	if lidStore == nil {
		return types.EmptyJID, ErrIdentityNotResolvable
	}
	lid, err := lidStore.GetLIDForPN(ctx, pn)
	if err != nil {
		return types.EmptyJID, fmt.Errorf("resolve lid for phone number: %w", err)
	}
	if lid.IsEmpty() {
		return types.EmptyJID, ErrIdentityNotResolvable
	}
	return lid.ToNonAD(), nil
}

// translateVerificationError maps the library's preconditions onto errors the
// API can turn into a specific answer, rather than a generic failure.
func translateVerificationError(err error) error {
	switch {
	case errors.Is(err, whatsmeow.ErrIdentityVerificationRequiresLID):
		return ErrInvalidContact
	case strings.Contains(err.Error(), "local user"):
		return ErrCannotVerifySelf
	case strings.Contains(err.Error(), "no devices found"):
		return ErrContactUnavailable
	default:
		return fmt.Errorf("get identity verification codes: %w", err)
	}
}

// isPlainPhoneNumber accepts digits only, the same shape the phone pairing flow
// takes: E.164 without the leading plus.
func isPlainPhoneNumber(value string) bool {
	if len(value) < 8 || len(value) > 20 {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func phoneUserOrEmpty(jid types.JID) string {
	if jid.IsEmpty() {
		return ""
	}
	return jid.User
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
		if classification.Reason == domain.ReasonStreamError {
			metrics.StreamErrors.WithLabelValues(classification.StreamErrorCode).Inc()
			s.logger.Warn("stream closed with an unknown code",
				slog.String("stream_error_code", classification.StreamErrorCode))
		}

		s.setPermanent(classification.Permanent)

		// The server asking the client to reconnect is not a disconnection to
		// report — it is an instruction to act on. Emitting it as its own kind
		// keeps the trail honest about which of the two happened (research R5).
		if classification.ManualReconnect {
			s.emit(Event{Kind: KindManualLoginReconnect})
			return
		}

		s.emit(Event{
			Kind:            KindDisconnected,
			Reason:          classification.Reason,
			Permanent:       classification.Permanent,
			BanExpiresAt:    classification.BanExpiresAt,
			StreamErrorCode: classification.StreamErrorCode,
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
	// Spreading reconnections in time is scheduling, not secrecy: predicting
	// this jitter gains an attacker nothing.
	jitter := time.Duration(rand.Int64N(int64(delay/4 + 1))) //nolint:gosec // not security-sensitive

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
