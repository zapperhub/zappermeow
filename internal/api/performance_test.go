package api_test

import (
	"context"
	"net/http"
	"sort"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/events"
)

// The measurable half of the success criteria.
//
// SC-001, SC-003, SC-013 and SC-014 time a human scanning a QR code against the
// real WhatsApp servers; they belong to the manual run in quickstart.md and are
// deliberately absent here rather than faked with a stand-in that would measure
// nothing. What follows is what the implementation can be held to on its own.

// SC-008: a transition must reach the event channel within two seconds. The
// budget covers the publish, the fan-out through Redis and the WebSocket write.
func TestTransitionReachesTheClientWithinTwoSeconds(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")
	server := f.wsServer(t)

	instanceID, err := domain.ParseID("instance", setup.instanceID)
	require.NoError(t, err)

	conn, err := dialWS(t, server, setup.instanceID, &coderws.DialOptions{
		HTTPHeader: http.Header{"X-Api-Key": {setup.key}},
	})
	require.NoError(t, err)
	defer func() { _ = conn.Close(coderws.StatusNormalClosure, "") }()

	require.Equal(t, events.TypeStateSnapshot, readEnvelope(t, conn).Type)

	publisher := events.NewPublisher(f.infra.Redis)
	var worst time.Duration

	for range 20 {
		start := time.Now()
		_, err := publisher.Publish(context.Background(), events.Envelope{
			Type:       events.TypeConnected,
			InstanceID: instanceID,
		})
		require.NoError(t, err)

		require.Equal(t, events.TypeConnected, readEnvelope(t, conn).Type)
		if elapsed := time.Since(start); elapsed > worst {
			worst = elapsed
		}
	}

	assert.Less(t, worst, 2*time.Second,
		"SC-008: worst transition took %s; a tenant watching a pairing would see it stall", worst)
	t.Logf("SC-008 worst case: %s", worst)
}

// SC-010: the connection status must answer under 300ms at p95. It is the call
// a dashboard polls, so its latency is what a tenant perceives as the API's
// responsiveness.
func TestConnectionStatusStaysUnder300msAtP95(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	// A paired instance with siblings: the heaviest shape this endpoint has,
	// since it also resolves which instances share the number.
	sibling := f.createInstance(setup.tenant.token, "vendas-02")
	for id, jid := range map[string]string{
		setup.instanceID: "5511999999999:11@s.whatsapp.net",
		sibling.ID:       "5511999999999:12@s.whatsapp.net",
	} {
		_, err := f.infra.Pool.Exec(t.Context(),
			`UPDATE instances SET wa_jid = $2, phone_number = '5511999999999', paired_at = now() WHERE id = $1`,
			id, jid)
		require.NoError(t, err)
	}

	const samples = 60
	latencies := make([]time.Duration, 0, samples)

	for range samples {
		start := time.Now()
		resp := f.do(request{
			method: http.MethodGet,
			path:   "/instances/" + setup.instanceID + "/connection",
			apiKey: setup.key,
		})
		require.Equal(t, http.StatusOK, resp.Status)
		latencies = append(latencies, time.Since(start))
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p95 := latencies[int(float64(len(latencies))*0.95)-1]

	assert.Less(t, p95, 300*time.Millisecond, "SC-010: p95 was %s", p95)
	t.Logf("SC-010 p95: %s (median %s, worst %s)", p95, latencies[len(latencies)/2], latencies[len(latencies)-1])
}

// SC-002: during a pairing attempt there must always be a usable code. The
// worker stores each code with the validity WhatsApp gave it, so the guarantee
// reduces to "the stored code never expires before the next one replaces it".
func TestPairingCodeIsAlwaysAvailableDuringAnAttempt(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	instanceID, err := domain.ParseID("instance", setup.instanceID)
	require.NoError(t, err)
	publisher := events.NewPublisher(f.infra.Redis)

	// The real rotation: the first code lives 60s, the ones after it 20s.
	require.NoError(t, publisher.SetPairing(t.Context(), instanceID, events.PairingSnapshot{
		Method: "qr", Code: "2@first", ExpiresAt: time.Now().Add(60 * time.Second),
	}))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, found, err := publisher.Pairing(t.Context(), instanceID)
		require.NoError(t, err)
		require.True(t, found, "SC-002: a client arriving now would find no code to scan")
		time.Sleep(50 * time.Millisecond)
	}

	// A rotation replaces the code without ever leaving a gap.
	require.NoError(t, publisher.SetPairing(t.Context(), instanceID, events.PairingSnapshot{
		Method: "qr", Code: "2@second", ExpiresAt: time.Now().Add(20 * time.Second),
	}))

	snapshot, found, err := publisher.Pairing(t.Context(), instanceID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "2@second", snapshot.Code)
}

// A client that opens the channel mid-attempt must get the code in the very
// first frame — this is the part of SC-001 that does not depend on WhatsApp.
func TestSnapshotDeliversTheCodeImmediately(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")
	server := f.wsServer(t)

	instanceID, err := domain.ParseID("instance", setup.instanceID)
	require.NoError(t, err)
	require.NoError(t, events.NewPublisher(f.infra.Redis).SetPairing(t.Context(), instanceID, events.PairingSnapshot{
		Method: "qr", Code: "2@waiting", ExpiresAt: time.Now().Add(20 * time.Second),
	}))

	start := time.Now()
	conn, err := dialWS(t, server, setup.instanceID, &coderws.DialOptions{
		HTTPHeader: http.Header{"X-Api-Key": {setup.key}},
	})
	require.NoError(t, err)
	defer func() { _ = conn.Close(coderws.StatusNormalClosure, "") }()

	frame := readEnvelope(t, conn)
	elapsed := time.Since(start)

	pairing, ok := frame.Data["pairing"].(map[string]any)
	require.True(t, ok, "the snapshot must carry the attempt in flight")
	assert.Equal(t, "2@waiting", pairing["code"])
	assert.Less(t, elapsed, 5*time.Second, "handshake to first code took %s", elapsed)
	t.Logf("handshake to first code: %s", elapsed)
}
