package ws_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zapperhub/zappermeow/internal/api/ws"
	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/events"
	"github.com/zapperhub/zappermeow/internal/store/testutil"
)

type stubAuth struct {
	// valid is the only credential accepted; flipping revoked simulates a key
	// being revoked while the connection is open.
	valid   string
	revoked atomic.Bool
	calls   atomic.Int32
}

func (s *stubAuth) Authorize(_ context.Context, credential string, _ domain.ID) (ws.Principal, error) {
	s.calls.Add(1)
	if s.revoked.Load() || credential != s.valid {
		return ws.Principal{}, ws.ErrForbidden
	}
	return ws.Principal{TenantID: domain.NewID(), RateLimitKey: "rl:conn:key:test"}, nil
}

// stubLimiter counts handshakes and can refuse them, standing in for the shared
// GCRA allowance.
type stubLimiter struct {
	allow atomic.Bool
	calls atomic.Int32
}

func (s *stubLimiter) AllowKey(context.Context, string) bool {
	s.calls.Add(1)
	return s.allow.Load()
}

type stubSnapshot struct {
	envelope events.Envelope
}

func (s *stubSnapshot) Snapshot(_ context.Context, instanceID domain.ID) (events.Envelope, error) {
	envelope := s.envelope
	envelope.InstanceID = instanceID
	return envelope, nil
}

type fixture struct {
	server     *httptest.Server
	publisher  *events.Publisher
	auth       *stubAuth
	snapshot   *stubSnapshot
	limiter    *stubLimiter
	instanceID domain.ID
	ctx        context.Context
}

func setup(t *testing.T) *fixture {
	t.Helper()

	infra := testutil.Shared(t)
	infra.Reset(t)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	instanceID := domain.NewID()

	limiter := &stubLimiter{}
	limiter.allow.Store(true)

	f := &fixture{
		publisher:  events.NewPublisher(infra.Redis),
		auth:       &stubAuth{valid: "zmk_secret"},
		limiter:    limiter,
		snapshot:   &stubSnapshot{envelope: events.Envelope{Type: events.TypeStateSnapshot, Seq: 0}},
		instanceID: instanceID,
		ctx:        context.Background(),
	}

	handler := ws.NewHandler(f.auth, f.limiter, events.NewSubscriber(infra.Redis, logger), f.snapshot, logger)
	handler.RevalidateInterval = 100 * time.Millisecond
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r, instanceID)
	}))
	t.Cleanup(f.server.Close)

	return f
}

func (f *fixture) url() string { return "ws" + strings.TrimPrefix(f.server.URL, "http") }

func (f *fixture) dial(t *testing.T, opts *coderws.DialOptions) (*coderws.Conn, *http.Response, error) {
	t.Helper()
	if opts == nil {
		opts = &coderws.DialOptions{}
	}
	if opts.Subprotocols == nil {
		opts.Subprotocols = []string{ws.Subprotocol}
	}

	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	t.Cleanup(cancel)
	return coderws.Dial(ctx, f.url(), opts)
}

func readFrame(t *testing.T, conn *coderws.Conn) events.Envelope {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, payload, err := conn.Read(ctx)
	require.NoError(t, err)

	var envelope events.Envelope
	require.NoError(t, json.Unmarshal(payload, &envelope))
	return envelope
}

func TestHandshakeRequiresACredential(t *testing.T) {
	f := setup(t)

	_, resp, err := f.dial(t, &coderws.DialOptions{})
	require.Error(t, err, "no credential means no socket")
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"the refusal must be an HTTP status, not a socket that closes right after opening")
}

func TestHandshakeRejectsAnInvalidCredential(t *testing.T) {
	f := setup(t)

	_, resp, err := f.dial(t, &coderws.DialOptions{
		HTTPHeader: http.Header{"X-Api-Key": {"zmk_wrong"}},
	})
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// A token in the query string would be recorded by every proxy in the path, so
// it must not work even when it is otherwise valid.
func TestTokenInQueryStringIsRefused(t *testing.T) {
	f := setup(t)

	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()

	_, resp, err := coderws.Dial(ctx, f.url()+"?token=zmk_secret", &coderws.DialOptions{
		Subprotocols: []string{ws.Subprotocol},
	})
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestApiKeyHeaderIsAccepted(t *testing.T) {
	f := setup(t)

	conn, _, err := f.dial(t, &coderws.DialOptions{
		HTTPHeader: http.Header{"X-Api-Key": {"zmk_secret"}},
	})
	require.NoError(t, err)
	defer func() { _ = conn.Close(coderws.StatusNormalClosure, "") }()

	assert.Equal(t, events.TypeStateSnapshot, readFrame(t, conn).Type)
}

func TestBearerHeaderIsAccepted(t *testing.T) {
	f := setup(t)

	conn, _, err := f.dial(t, &coderws.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer zmk_secret"}},
	})
	require.NoError(t, err)
	defer func() { _ = conn.Close(coderws.StatusNormalClosure, "") }()

	assert.Equal(t, events.TypeStateSnapshot, readFrame(t, conn).Type)
}

// The browser path: the WebSocket API cannot set headers, so the credential
// rides in the subprotocol list.
func TestSubprotocolCredentialIsAccepted(t *testing.T) {
	f := setup(t)

	conn, _, err := f.dial(t, &coderws.DialOptions{
		Subprotocols: []string{ws.Subprotocol, "bearer.zmk_secret"},
	})
	require.NoError(t, err)
	defer func() { _ = conn.Close(coderws.StatusNormalClosure, "") }()

	assert.Equal(t, ws.Subprotocol, conn.Subprotocol(), "the server echoes the versioned subprotocol")
	assert.Equal(t, events.TypeStateSnapshot, readFrame(t, conn).Type)
}

func TestSnapshotIsAlwaysTheFirstFrame(t *testing.T) {
	f := setup(t)
	f.snapshot.envelope = events.Envelope{
		Type: events.TypeStateSnapshot,
		Seq:  7,
		Data: map[string]any{"state": "pairing", "pairing": map[string]any{"code": "2@AbC"}},
	}

	conn, _, err := f.dial(t, &coderws.DialOptions{
		HTTPHeader: http.Header{"X-Api-Key": {"zmk_secret"}},
	})
	require.NoError(t, err)
	defer func() { _ = conn.Close(coderws.StatusNormalClosure, "") }()

	frame := readFrame(t, conn)
	assert.Equal(t, events.TypeStateSnapshot, frame.Type)
	assert.Equal(t, int64(7), frame.Seq)

	// A client arriving mid-pairing sees the current code immediately instead
	// of waiting for the next rotation.
	pairing, ok := frame.Data["pairing"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "2@AbC", pairing["code"])
}

func TestLiveEventsFollowTheSnapshot(t *testing.T) {
	f := setup(t)

	conn, _, err := f.dial(t, &coderws.DialOptions{
		HTTPHeader: http.Header{"X-Api-Key": {"zmk_secret"}},
	})
	require.NoError(t, err)
	defer func() { _ = conn.Close(coderws.StatusNormalClosure, "") }()

	require.Equal(t, events.TypeStateSnapshot, readFrame(t, conn).Type)

	_, err = f.publisher.Publish(f.ctx, events.Envelope{
		Type:       events.TypePairingCode,
		InstanceID: f.instanceID,
		Generation: 3,
		Data:       map[string]any{"code": "2@live"},
	})
	require.NoError(t, err)

	frame := readFrame(t, conn)
	assert.Equal(t, events.TypePairingCode, frame.Type)
	assert.Equal(t, "2@live", frame.Data["code"])
	assert.Equal(t, int64(3), frame.Generation,
		"the generation travels with the frame so a client can ignore a former owner")
}

// Subscribing before reading the snapshot duplicates rather than loses; the
// sequence number is what turns that duplicate back into a single event.
func TestEventsAlreadyInTheSnapshotAreNotResent(t *testing.T) {
	f := setup(t)

	// Publish first so the counter advances, then claim the snapshot already
	// reflects it.
	published, err := f.publisher.Publish(f.ctx, events.Envelope{
		Type:       events.TypeConnected,
		InstanceID: f.instanceID,
	})
	require.NoError(t, err)
	f.snapshot.envelope = events.Envelope{Type: events.TypeStateSnapshot, Seq: published.Seq}

	conn, _, err := f.dial(t, &coderws.DialOptions{
		HTTPHeader: http.Header{"X-Api-Key": {"zmk_secret"}},
	})
	require.NoError(t, err)
	defer func() { _ = conn.Close(coderws.StatusNormalClosure, "") }()

	require.Equal(t, events.TypeStateSnapshot, readFrame(t, conn).Type)

	// An event with a higher sequence still arrives.
	_, err = f.publisher.Publish(f.ctx, events.Envelope{
		Type:       events.TypeDisconnected,
		InstanceID: f.instanceID,
	})
	require.NoError(t, err)

	frame := readFrame(t, conn)
	assert.Equal(t, events.TypeDisconnected, frame.Type,
		"the stale duplicate must be dropped, the newer event delivered")
	assert.Greater(t, frame.Seq, published.Seq)
}

func TestSeveralListenersReceiveTheSameEvents(t *testing.T) {
	f := setup(t)

	header := http.Header{"X-Api-Key": {"zmk_secret"}}
	first, _, err := f.dial(t, &coderws.DialOptions{HTTPHeader: header})
	require.NoError(t, err)
	defer func() { _ = first.Close(coderws.StatusNormalClosure, "") }()

	second, _, err := f.dial(t, &coderws.DialOptions{HTTPHeader: header})
	require.NoError(t, err)
	defer func() { _ = second.Close(coderws.StatusNormalClosure, "") }()

	require.Equal(t, events.TypeStateSnapshot, readFrame(t, first).Type)
	require.Equal(t, events.TypeStateSnapshot, readFrame(t, second).Type)

	_, err = f.publisher.Publish(f.ctx, events.Envelope{
		Type:       events.TypeConnected,
		InstanceID: f.instanceID,
	})
	require.NoError(t, err)

	assert.Equal(t, events.TypeConnected, readFrame(t, first).Type)
	assert.Equal(t, events.TypeConnected, readFrame(t, second).Type)
}

// Authorisation is not only checked at the door: a key revoked mid-session must
// stop receiving events (FR-042).
func TestRevokedCredentialClosesTheConnection(t *testing.T) {
	f := setup(t)

	conn, _, err := f.dial(t, &coderws.DialOptions{
		HTTPHeader: http.Header{"X-Api-Key": {"zmk_secret"}},
	})
	require.NoError(t, err)
	require.Equal(t, events.TypeStateSnapshot, readFrame(t, conn).Type)

	f.auth.revoked.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, _, err = conn.Read(ctx)
	require.Error(t, err, "a revoked credential must not keep receiving events")

	var closeErr coderws.CloseError
	require.ErrorAs(t, err, &closeErr)
	assert.Equal(t, ws.CloseRevoked, closeErr.Code,
		"the client must learn why it was disconnected, not just that it was")
}

// The upgrade is a chi handler outside huma's middleware chain, so the shared
// allowance has to be enforced here explicitly (constitution, principle II).
func TestHandshakeIsRateLimited(t *testing.T) {
	f := setup(t)
	f.limiter.allow.Store(false)

	_, resp, err := f.dial(t, &coderws.DialOptions{
		HTTPHeader: http.Header{"X-Api-Key": {"zmk_secret"}},
	})
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode,
		"refusing before the upgrade costs less than opening a socket to close it")
	assert.Equal(t, int32(1), f.limiter.calls.Load())
}

// An unauthenticated handshake must not even reach the limiter: it would let an
// anonymous caller drain someone else's allowance.
func TestRateLimitIsCheckedAfterAuthentication(t *testing.T) {
	f := setup(t)

	_, _, err := f.dial(t, &coderws.DialOptions{
		HTTPHeader: http.Header{"X-Api-Key": {"zmk_wrong"}},
	})
	require.Error(t, err)
	assert.Equal(t, int32(0), f.limiter.calls.Load())
}
