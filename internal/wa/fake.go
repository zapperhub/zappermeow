package wa

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/zapperhub/zappermeow/internal/domain"
)

// FakeSession is a scripted Session for tests.
//
// It replaces only the WhatsApp hop. Everything around it — the state machine,
// the lease, fencing, the event fan-out and the persistence — is still
// exercised against real Postgres and Redis, which is where the constitution
// requires real infrastructure. WhatsApp itself offers no sandbox: a test
// against it needs a physical handset and risks banning the number (R13).
type FakeSession struct {
	mu     sync.Mutex
	events chan Event
	// pairing mirrors the real client, where QR codes arrive on a channel of
	// their own rather than on the main event stream. Sharing one channel would
	// let two consumers split the events between them and hide ordering bugs.
	pairing chan Event
	status  Status

	// device is what a successful pairing reports.
	device domain.DeviceIdentity

	// pairingOpen tracks whether a QR attempt is in flight, so the fake refuses
	// the same combinations the real client refuses.
	pairingOpen bool
	closed      bool

	// ConnectCtx records the context the caller opened the connection with.
	// The real client binds the socket's lifetime to it, so a caller that
	// passes a short-lived context silently kills the session; the only way to
	// catch that in a test is to keep the context and check it later.
	ConnectCtx context.Context

	// ConnectErr, if set, makes Connect fail — used to exercise the retry paths.
	ConnectErr error
	// LogoutRemoteFails simulates a logout that cannot reach the server, which
	// is the case that falls back to deleting the material locally (R10).
	LogoutRemoteFails bool
}

// Errors mirroring the failure modes of the real client.
var (
	ErrAlreadyPaired  = errors.New("wa: session already has device material")
	ErrNotPaired      = errors.New("wa: session has no device material")
	ErrSessionClosed  = errors.New("wa: session is closed")
	ErrPairingRunning = errors.New("wa: a pairing attempt is already running")
)

// NewFakeSession returns an unpaired session, the state of a freshly registered
// instance.
func NewFakeSession() *FakeSession {
	return &FakeSession{events: make(chan Event, 32)}
}

// NewPairedFakeSession returns a session that already holds device material and
// can reconnect without pairing.
func NewPairedFakeSession(device domain.DeviceIdentity) *FakeSession {
	f := NewFakeSession()
	f.device = device
	f.status.LoggedIn = true
	f.status.Device = &device
	return f
}

func (f *FakeSession) QRChannel(ctx context.Context) (<-chan Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return nil, ErrSessionClosed
	}
	// The real GetQRChannel refuses a store that already holds a JID; a test
	// that gets this wrong would pass here and fail in production.
	if f.device.JID != "" {
		return nil, ErrAlreadyPaired
	}
	if f.pairingOpen {
		return nil, ErrPairingRunning
	}
	f.pairingOpen = true
	f.pairing = make(chan Event, 8)
	return f.pairing, nil
}

func (f *FakeSession) PairPhone(ctx context.Context, phoneNumber string) (string, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return "", time.Time{}, ErrSessionClosed
	}
	if f.device.JID != "" {
		return "", time.Time{}, ErrAlreadyPaired
	}
	f.pairingOpen = true

	expires := time.Now().Add(60 * time.Second)
	code := "ABCD-2345"
	f.emitPairingLocked(Event{
		Kind:      KindPairingCode,
		Method:    MethodPhone,
		Code:      code,
		ExpiresAt: expires,
	})
	return code, expires, nil
}

func (f *FakeSession) Connect(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.ConnectCtx = ctx

	if f.closed {
		return ErrSessionClosed
	}
	if f.ConnectErr != nil {
		return f.ConnectErr
	}
	// Connecting without material and without a pairing attempt is the mistake
	// the state machine exists to prevent; the fake surfaces it as an error.
	if f.device.JID == "" && !f.pairingOpen {
		return ErrNotPaired
	}
	if f.device.JID != "" {
		f.status.Connected = true
		f.emitLocked(Event{Kind: KindConnected})
	}
	return nil
}

func (f *FakeSession) Disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status.Connected = false
}

func (f *FakeSession) Logout(ctx context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.device.JID == "" {
		return false, ErrNotPaired
	}
	f.device = domain.DeviceIdentity{}
	f.status = Status{}
	f.pairingOpen = false

	// A remote failure still clears the local material: the caller reports
	// local_only so the tenant knows the handset may still list the device.
	return !f.LogoutRemoteFails, nil
}

func (f *FakeSession) Events() <-chan Event { return f.events }

func (f *FakeSession) Status() Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *FakeSession) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.events)
	}
}

// --- scripting API: what the test makes the "WhatsApp side" do ---

// EmitPairingCode plays one QR code with the timings of the real client: the
// first code lives 60s, the following ones 20s (research R2).
func (f *FakeSession) EmitPairingCode(code string, first bool) {
	ttl := 20 * time.Second
	if first {
		ttl = 60 * time.Second
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emitPairingLocked(Event{
		Kind:      KindPairingCode,
		Method:    MethodQR,
		Code:      code,
		ExpiresAt: time.Now().Add(ttl),
	})
}

// EmitPairSuccess completes a pairing attempt with the given device.
func (f *FakeSession) EmitPairSuccess(device domain.DeviceIdentity) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.device = device
	f.pairingOpen = false
	f.status.LoggedIn = true
	f.status.Connected = true
	f.status.Device = &device

	// Pairing success arrives on the pairing channel; the connection event that
	// follows it belongs to the main stream, exactly as the real client does it.
	f.emitPairingLocked(Event{Kind: KindPairingSucceeded, Device: &device})
	f.emitLocked(Event{Kind: KindConnected})
}

// EmitPairingExpired ends the attempt without pairing.
func (f *FakeSession) EmitPairingExpired(reason PairingExpiry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pairingOpen = false
	f.emitPairingLocked(Event{Kind: KindPairingExpired, Method: MethodQR, Expiry: reason})
}

// EmitPairingFailed ends the attempt with a rejection from WhatsApp.
func (f *FakeSession) EmitPairingFailed(reason PairingFailure) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pairingOpen = false
	f.emitPairingLocked(Event{Kind: KindPairingFailed, Failure: reason})
}

// EmitDisconnect plays a drop with the given reason. Permanence comes from the
// reason vocabulary, so a test cannot script an inconsistent pair.
func (f *FakeSession) EmitDisconnect(reason domain.DisconnectReason) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.status.Connected = false
	if reason == domain.ReasonLoggedOutFromPhone {
		f.device = domain.DeviceIdentity{}
		f.status = Status{}
	}
	f.emitLocked(Event{
		Kind:      KindDisconnected,
		Reason:    reason,
		Permanent: reason.Permanent(),
	})
}

// EmitBan plays a temporary ban, optionally with a deadline.
func (f *FakeSession) EmitBan(expiresIn time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.status.Connected = false
	evt := Event{
		Kind:      KindDisconnected,
		Reason:    domain.ReasonTemporaryBan,
		Permanent: true,
	}
	if expiresIn > 0 {
		expires := time.Now().Add(expiresIn)
		evt.BanExpiresAt = &expires
	}
	f.emitLocked(evt)
}

// emitPairingLocked publishes on the pairing channel when an attempt is open,
// falling back to the main stream so a test that scripts a pairing event
// without opening a channel still observes it instead of losing it silently.
func (f *FakeSession) emitPairingLocked(evt Event) {
	if f.closed {
		return
	}
	if evt.OccurredAt.IsZero() {
		evt.OccurredAt = time.Now()
	}
	target := f.pairing
	if target == nil {
		target = f.events
	}
	select {
	case target <- evt:
	default:
	}
}

// emitLocked publishes without blocking. Dropping on a full buffer mirrors the
// real client, which closes the channel when the consumer falls behind — a test
// that trips this has a consumer bug worth seeing.
func (f *FakeSession) emitLocked(evt Event) {
	if f.closed {
		return
	}
	if evt.OccurredAt.IsZero() {
		evt.OccurredAt = time.Now()
	}
	select {
	case f.events <- evt:
	default:
	}
}

// compile-time check: the fake must keep satisfying the real interface.
var _ Session = (*FakeSession)(nil)
