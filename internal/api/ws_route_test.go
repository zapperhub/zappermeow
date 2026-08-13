package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zapperhub/zappermeow/internal/api/ws"
	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/events"
)

// wsServer exposes the real application over a real socket, so these tests
// exercise the same path a tenant's browser takes.
func (f *fixture) wsServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(f.handler)
	t.Cleanup(server.Close)
	return server
}

func dialWS(t *testing.T, server *httptest.Server, instanceID string, opts *coderws.DialOptions) (*coderws.Conn, *http.Response, error) {
	t.Helper()

	if opts.Subprotocols == nil {
		opts.Subprotocols = []string{ws.Subprotocol}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)

	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/instances/" + instanceID + "/ws"
	return coderws.Dial(ctx, url, opts)
}

func readEnvelope(t *testing.T, conn *coderws.Conn) events.Envelope {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, payload, err := conn.Read(ctx)
	require.NoError(t, err)

	var envelope events.Envelope
	require.NoError(t, json.Unmarshal(payload, &envelope))
	return envelope
}

func TestWebSocketDeliversASnapshotToAnAPIKey(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")
	server := f.wsServer(t)

	conn, _, err := dialWS(t, server, setup.instanceID, &coderws.DialOptions{
		HTTPHeader: http.Header{"X-Api-Key": {setup.key}},
	})
	require.NoError(t, err)
	defer func() { _ = conn.Close(coderws.StatusNormalClosure, "") }()

	frame := readEnvelope(t, conn)
	assert.Equal(t, events.TypeStateSnapshot, frame.Type, "the first frame is always the snapshot")
	assert.Equal(t, "registered", frame.Data["state"])
	assert.Nil(t, frame.Data["device"], "an instance that never paired has no device")
	assert.Nil(t, frame.Data["pairing"], "no attempt in flight means no code")
}

func TestWebSocketAcceptsATenantToken(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")
	server := f.wsServer(t)

	conn, _, err := dialWS(t, server, setup.instanceID, &coderws.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + setup.tenant.token}},
	})
	require.NoError(t, err)
	defer func() { _ = conn.Close(coderws.StatusNormalClosure, "") }()

	assert.Equal(t, events.TypeStateSnapshot, readEnvelope(t, conn).Type)
}

// The browser path: the WebSocket API cannot set headers, so the credential
// rides in the subprotocol list rather than in the query string.
func TestWebSocketAcceptsTheSubprotocolCredential(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")
	server := f.wsServer(t)

	conn, _, err := dialWS(t, server, setup.instanceID, &coderws.DialOptions{
		Subprotocols: []string{ws.Subprotocol, "bearer." + setup.key},
	})
	require.NoError(t, err)
	defer func() { _ = conn.Close(coderws.StatusNormalClosure, "") }()

	assert.Equal(t, events.TypeStateSnapshot, readEnvelope(t, conn).Type)
}

func TestWebSocketRefusesAKeyFromAnotherInstance(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")
	sibling := f.createInstance(setup.tenant.token, "vendas-02")
	server := f.wsServer(t)

	_, resp, err := dialWS(t, server, sibling.ID, &coderws.DialOptions{
		HTTPHeader: http.Header{"X-Api-Key": {setup.key}},
	})
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestWebSocketRefusesAnotherTenant(t *testing.T) {
	f := newFixture(t)
	owner := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")
	intruder := f.newTenant(f.platformToken(), "Globex", "bob@globex.com", "senhaBob123")
	server := f.wsServer(t)

	_, resp, err := dialWS(t, server, owner.instanceID, &coderws.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + intruder.token}},
	})
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestWebSocketRefusesWithoutACredential(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")
	server := f.wsServer(t)

	_, resp, err := dialWS(t, server, setup.instanceID, &coderws.DialOptions{})
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// US1 scenario 4: a client that opens the channel mid-pairing must see the
// current code immediately instead of waiting for the next rotation.
func TestWebSocketSnapshotCarriesTheCurrentPairingCode(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")
	server := f.wsServer(t)

	instanceID, err := domain.ParseID("instance", setup.instanceID)
	require.NoError(t, err)

	publisher := events.NewPublisher(f.infra.Redis)
	require.NoError(t, publisher.SetPairing(context.Background(), instanceID, events.PairingSnapshot{
		Method:    "qr",
		Code:      "2@InFlight",
		ExpiresAt: time.Now().Add(20 * time.Second),
	}))

	conn, _, err := dialWS(t, server, setup.instanceID, &coderws.DialOptions{
		HTTPHeader: http.Header{"X-Api-Key": {setup.key}},
	})
	require.NoError(t, err)
	defer func() { _ = conn.Close(coderws.StatusNormalClosure, "") }()

	frame := readEnvelope(t, conn)
	pairing, ok := frame.Data["pairing"].(map[string]any)
	require.True(t, ok, "a pairing in flight must appear in the snapshot")
	assert.Equal(t, "2@InFlight", pairing["code"])
}

// Several listeners per instance is a requirement, not an accident: a dashboard
// and an integration may watch the same number at once (FR-034).
func TestWebSocketFansOutToEveryListener(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")
	server := f.wsServer(t)

	header := http.Header{"X-Api-Key": {setup.key}}
	first, _, err := dialWS(t, server, setup.instanceID, &coderws.DialOptions{HTTPHeader: header})
	require.NoError(t, err)
	defer func() { _ = first.Close(coderws.StatusNormalClosure, "") }()

	second, _, err := dialWS(t, server, setup.instanceID, &coderws.DialOptions{HTTPHeader: header})
	require.NoError(t, err)
	defer func() { _ = second.Close(coderws.StatusNormalClosure, "") }()

	require.Equal(t, events.TypeStateSnapshot, readEnvelope(t, first).Type)
	require.Equal(t, events.TypeStateSnapshot, readEnvelope(t, second).Type)

	instanceID, err := domain.ParseID("instance", setup.instanceID)
	require.NoError(t, err)

	_, err = events.NewPublisher(f.infra.Redis).Publish(context.Background(), events.Envelope{
		Type:       events.TypePairingCode,
		InstanceID: instanceID,
		Generation: 4,
		Data:       map[string]any{"code": "2@broadcast"},
	})
	require.NoError(t, err)

	for _, conn := range []*coderws.Conn{first, second} {
		frame := readEnvelope(t, conn)
		assert.Equal(t, events.TypePairingCode, frame.Type)
		assert.Equal(t, "2@broadcast", frame.Data["code"])
		assert.Equal(t, int64(4), frame.Generation)
	}
}
