package wa

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
	// passive is the mode currently in force on the server side of this fake.
	passive bool

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

	// --- passkey step state, mirroring the real client's preconditions ---

	// passkeyChallengeOpen is set between the challenge and its assertion;
	// passkeyCodePending between the assertion and the confirmation. The real
	// client refuses both calls out of order, and SendPasskeyConfirmation is not
	// reentrant — a fake that accepted them would hide that (research R7).
	passkeyChallengeOpen bool
	passkeyCodePending   bool

	// calls records every command the worker issued, in order. Ordering is the
	// property under test for passive mode: the platform must reapply after each
	// Connected, and a fake that only counted calls could not tell.
	calls []Call

	// SetPassiveErr, PasskeyResponseErr and PasskeyConfirmErr make the
	// corresponding command fail.
	SetPassiveErr      error
	PasskeyResponseErr error
	PasskeyConfirmErr  error

	// VerificationCodesResult and VerificationCodesErr are what
	// IdentityVerificationCodes answers with.
	VerificationCodesResult *VerificationCodes
	VerificationCodesErr    error
	// LIDMappings resolves a phone number to a LID, standing in for the session
	// store the real client consults. A number missing here is a number this
	// session has never seen mapped.
	LIDMappings map[string]string
	// SelfLID is the instance's own identity, which must never be verifiable
	// against itself.
	SelfLID string
}

// Call is one command the worker issued against the session, recorded in order.
type Call struct {
	Name string
	// Passive is set for SetPassive calls.
	Passive bool
	// Payload is set for SendPasskeyResponse.
	Payload []byte
	// Contact is set for IdentityVerificationCodes.
	Contact string
}

// Command names recorded in Calls.
const (
	CallConnect             = "Connect"
	CallDisconnect          = "Disconnect"
	CallSetPassive          = "SetPassive"
	CallSendPasskeyResponse = "SendPasskeyResponse"
	CallConfirmPasskey      = "ConfirmPasskey"
	CallVerificationCodes   = "IdentityVerificationCodes"
	CallClose               = "Close"
)

// Errors mirroring the failure modes of the real client.
var (
	ErrAlreadyPaired  = errors.New("wa: session already has device material")
	ErrNotPaired      = errors.New("wa: session has no device material")
	ErrSessionClosed  = errors.New("wa: session is closed")
	ErrPairingRunning = errors.New("wa: a pairing attempt is already running")
	ErrNotConnected   = errors.New("wa: session is not connected")

	// ErrNoPasskeyChallenge and ErrNoPasskeyCode mirror the real client
	// refusing a passkey command with nothing pending for it.
	ErrNoPasskeyChallenge = errors.New("wa: no passkey challenge is pending")
	ErrNoPasskeyCode      = errors.New("wa: no passkey confirmation code is pending")

	// ErrIdentityNotResolvable, ErrCannotVerifySelf, ErrContactUnavailable and
	// ErrInvalidContact are the preconditions of the identity verification
	// codes (research R8).
	ErrIdentityNotResolvable = errors.New("wa: no known LID mapping for the phone number")
	ErrCannotVerifySelf      = errors.New("wa: cannot verify the local user")
	ErrContactUnavailable    = errors.New("wa: contact has no devices")
	ErrInvalidContact        = errors.New("wa: contact is neither a LID nor a phone number")

	// ErrInvalidPasskeyResponse is an assertion the authenticator produced in a
	// shape the library cannot read.
	ErrInvalidPasskeyResponse = errors.New("wa: passkey response is not a valid WebAuthn assertion")

	// ErrProxyConnectFailed is a connection that failed with a proxy in force.
	// It never falls back to a direct connection: that would leak the
	// platform's own address (FR-005).
	ErrProxyConnectFailed = errors.New("wa: could not connect through the configured proxy")
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
	f.calls = append(f.calls, Call{Name: CallConnect})

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
	f.calls = append(f.calls, Call{Name: CallDisconnect})
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

func (f *FakeSession) SetPassive(ctx context.Context, passive bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, Call{Name: CallSetPassive, Passive: passive})

	if f.closed {
		return ErrSessionClosed
	}
	// The real call is an IQ: without a socket it cannot be sent at all. A fake
	// that accepted it offline would let the worker "succeed" at something the
	// server never heard (research R6).
	if !f.status.Connected {
		return ErrNotConnected
	}
	if f.SetPassiveErr != nil {
		return f.SetPassiveErr
	}
	f.passive = passive
	return nil
}

func (f *FakeSession) SendPasskeyResponse(ctx context.Context, webauthnResponseJSON []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, Call{Name: CallSendPasskeyResponse, Payload: webauthnResponseJSON})

	if f.closed {
		return ErrSessionClosed
	}
	if !f.passkeyChallengeOpen {
		return ErrNoPasskeyChallenge
	}
	if f.PasskeyResponseErr != nil {
		return f.PasskeyResponseErr
	}
	// The challenge is consumed: answering twice is the out-of-order case the
	// worker must turn into a clear error.
	f.passkeyChallengeOpen = false
	return nil
}

func (f *FakeSession) ConfirmPasskey(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, Call{Name: CallConfirmPasskey})

	if f.closed {
		return ErrSessionClosed
	}
	if !f.passkeyCodePending {
		return ErrNoPasskeyCode
	}
	if f.PasskeyConfirmErr != nil {
		return f.PasskeyConfirmErr
	}
	// Not reentrant, exactly like the real client, which clears its linking
	// cache on success.
	f.passkeyCodePending = false
	return nil
}

func (f *FakeSession) IdentityVerificationCodes(ctx context.Context, contact string) (*VerificationCodes, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, Call{Name: CallVerificationCodes, Contact: contact})

	if f.closed {
		return nil, ErrSessionClosed
	}
	if !f.status.Connected {
		return nil, ErrNotConnected
	}
	if f.VerificationCodesErr != nil {
		return nil, f.VerificationCodesErr
	}

	lid := contact
	if !strings.HasSuffix(contact, "@lid") {
		mapped, ok := f.LIDMappings[contact]
		if !ok {
			return nil, ErrIdentityNotResolvable
		}
		lid = mapped
	}
	if f.SelfLID != "" && lid == f.SelfLID {
		return nil, ErrCannotVerifySelf
	}
	if f.VerificationCodesResult == nil {
		return nil, ErrContactUnavailable
	}

	result := *f.VerificationCodesResult
	result.LID = lid
	return &result, nil
}

func (f *FakeSession) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Name: CallClose})
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

// EmitPasskeyChallenge plays WhatsApp requiring the passkey step mid-attempt.
// It arrives on the pairing channel because that is where the real client
// surfaces it, and the attempt stays open waiting for the assertion.
func (f *FakeSession) EmitPasskeyChallenge(challenge json.RawMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.passkeyChallengeOpen = true
	f.emitPairingLocked(Event{Kind: KindPasskeyChallenge, Challenge: challenge})
}

// EmitPasskeyCode plays the handoff code that must be compared against the
// handset. The real client only emits this when the confirmation is not
// automatic; EmitPasskeyAutoConfirmed covers the other branch.
func (f *FakeSession) EmitPasskeyCode(code string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.passkeyCodePending = true
	f.emitPairingLocked(Event{Kind: KindPasskeyCode, Code: code})
}

// EmitPasskeyAutoConfirmed plays the branch where a valid handoff proof lets
// the library confirm on its own: no code reaches the tenant, and the next
// thing that happens is the pairing succeeding.
func (f *FakeSession) EmitPasskeyAutoConfirmed() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.passkeyCodePending = false
}

// EmitPasskeyFailure ends the attempt with a failing passkey step.
func (f *FakeSession) EmitPasskeyFailure() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.passkeyChallengeOpen = false
	f.passkeyCodePending = false
	f.pairingOpen = false
	f.emitPairingLocked(Event{Kind: KindPairingFailed, Failure: FailurePasskeyError})
}

// EmitStreamError plays a stream closed with a code the library does not know.
// It is transient: the material survives and reconnecting is the right move.
func (f *FakeSession) EmitStreamError(code string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.status.Connected = false
	f.emitLocked(Event{
		Kind:            KindDisconnected,
		Reason:          domain.ReasonStreamError,
		Permanent:       false,
		StreamErrorCode: code,
	})
}

// EmitManualLoginReconnect plays the server asking the client to reconnect on
// its own after pairing.
func (f *FakeSession) EmitManualLoginReconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.status.Connected = false
	f.emitLocked(Event{Kind: KindManualLoginReconnect})
}

// Calls returns every command issued so far, in order.
func (f *FakeSession) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// CallNames returns just the command names, which is what an ordering assertion
// usually needs.
func (f *FakeSession) CallNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	names := make([]string, 0, len(f.calls))
	for _, call := range f.calls {
		names = append(names, call.Name)
	}
	return names
}

// Passive reports the mode currently in force on the session.
func (f *FakeSession) Passive() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.passive
}

// IsClosed reports whether the session was closed. A factory uses it to hand
// out a fresh session instead of a dead one, which is what the real container
// does: every build creates a new client.
func (f *FakeSession) IsClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
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
