package worker_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/events"
	"github.com/zapperhub/zappermeow/internal/lease"
	"github.com/zapperhub/zappermeow/internal/store"
	"github.com/zapperhub/zappermeow/internal/store/testutil"
	"github.com/zapperhub/zappermeow/internal/wa"
	"github.com/zapperhub/zappermeow/internal/worker"
)

// fakeFactory hands out sessions the test scripts. Postgres and Redis stay
// real; only the WhatsApp hop is replaced (research R13).
type fakeFactory struct {
	sessions map[domain.ID]*wa.FakeSession
}

func (f *fakeFactory) NewSession(_ context.Context, instanceID domain.ID, _ string) (wa.Session, error) {
	if session, ok := f.sessions[instanceID]; ok {
		return session, nil
	}
	session := wa.NewFakeSession()
	f.sessions[instanceID] = session
	return session, nil
}

type harness struct {
	infra      *testutil.Infra
	supervisor *worker.Supervisor
	leases     *lease.Manager
	publisher  *events.Publisher
	subscriber *events.Subscriber
	factory    *fakeFactory
	instanceID domain.ID
	ctx        context.Context
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	infra := testutil.Shared(t)
	infra.Reset(t)
	ctx := context.Background()

	tenantID := uuid.New()
	_, err := infra.Pool.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ($1, 'acme')`, tenantID)
	require.NoError(t, err)

	instanceID := uuid.New()
	_, err = infra.Pool.Exec(ctx,
		`INSERT INTO instances (id, tenant_id, name) VALUES ($1, $2, 'vendas-01')`, instanceID, tenantID)
	require.NoError(t, err)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	leases := lease.New(infra.Queries, lease.Options{
		WorkerID: "worker-test",
		GRPCAddr: "127.0.0.1:9090",
		Expiry:   30 * time.Second,
	})
	factory := &fakeFactory{sessions: map[domain.ID]*wa.FakeSession{}}

	h := &harness{
		infra:      infra,
		leases:     leases,
		publisher:  events.NewPublisher(infra.Redis),
		subscriber: events.NewSubscriber(infra.Redis, logger),
		factory:    factory,
		instanceID: domain.ID(instanceID),
		ctx:        ctx,
	}
	h.supervisor = worker.NewSupervisor(worker.Options{
		Queries:       infra.Queries,
		Leases:        leases,
		Publisher:     h.publisher,
		Factory:       factory,
		Logger:        logger,
		PairingWindow: 3 * time.Second,
		MaxSessions:   10,
	})

	// Every test drains its own supervisor: leaving pumps alive would let one
	// test's sessions write into the next test's freshly reset database.
	t.Cleanup(func() { _ = h.supervisor.Shutdown(context.Background()) })

	require.NoError(t, leases.Ensure(ctx, h.instanceID))
	require.NoError(t, leases.SetDesired(ctx, h.instanceID, lease.DesiredRunning))
	return h
}

func (h *harness) adopt(t *testing.T) *wa.FakeSession {
	t.Helper()
	require.NoError(t, h.supervisor.Adopt(h.ctx, h.instanceID))
	return h.factory.sessions[h.instanceID]
}

func (h *harness) instance(t *testing.T) store.Instance {
	t.Helper()
	row, err := h.infra.Queries.GetInstanceConnectionByID(h.ctx, uuid.UUID(h.instanceID))
	require.NoError(t, err)
	return row
}

// eventually polls the database because state changes travel through the
// session's event pump, which is asynchronous by design.
func (h *harness) waitForState(t *testing.T, want domain.InstanceState) store.Instance {
	t.Helper()

	// The window is generous on purpose: the whole suite starts a Postgres and a
	// Redis per package, and on a loaded machine a correct transition can take
	// seconds to land. A tight deadline here fails the build for being busy,
	// not for being wrong — and the assertion is just as strict either way.
	var last store.Instance
	require.Eventually(t, func() bool {
		last = h.instance(t)
		return last.ConnectionState == string(want)
	}, 20*time.Second, 25*time.Millisecond, "state never reached %q (last: %q)", want, last.ConnectionState)
	return last
}

func (h *harness) trail(t *testing.T) []string {
	t.Helper()

	rows, err := h.infra.Pool.Query(h.ctx,
		`SELECT type FROM connection_events WHERE instance_id = $1 ORDER BY id`, uuid.UUID(h.instanceID))
	require.NoError(t, err)
	defer rows.Close()

	var types []string
	for rows.Next() {
		var t2 string
		require.NoError(t, rows.Scan(&t2))
		types = append(types, t2)
	}
	return types
}

func TestAdoptStartsASessionAndRecordsTheLease(t *testing.T) {
	h := newHarness(t)
	h.adopt(t)

	assert.True(t, h.supervisor.Owns(h.instanceID))
	assert.Equal(t, 1, h.supervisor.Count())
	assert.Contains(t, h.trail(t), "lease_acquired")
}

func TestAdoptIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.adopt(t)

	require.NoError(t, h.supervisor.Adopt(h.ctx, h.instanceID))
	assert.Equal(t, 1, h.supervisor.Count(), "adopting twice must not start a second session")
}

// The full US1 path: connect with no device material starts pairing, codes
// reach the bus, and a successful scan persists the companion device.
func TestConnectStartsPairingAndPersistsTheDevice(t *testing.T) {
	h := newHarness(t)
	session := h.adopt(t)

	stream, err := h.subscriber.Subscribe(h.ctx, h.instanceID)
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()

	result, err := h.supervisor.Connect(h.ctx, h.instanceID)
	require.NoError(t, err)
	assert.True(t, result.PairingStarted)
	assert.Equal(t, domain.InstancePairing, result.State)
	h.waitForState(t, domain.InstancePairing)

	session.EmitPairingCode("2@first", true)

	frame := receive(t, stream)
	assert.Equal(t, events.TypePairingCode, frame.Type)
	assert.Equal(t, "2@first", frame.Data["code"])

	// The code is also parked in Redis, so a client arriving late sees it
	// instead of waiting for the next rotation.
	snapshot, found, err := h.publisher.Pairing(h.ctx, h.instanceID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "2@first", snapshot.Code)

	session.EmitPairSuccess(domain.DeviceIdentity{
		JID:         "5511999999999:11@s.whatsapp.net",
		PhoneNumber: "5511999999999",
		PushName:    "Suporte ACME",
		Platform:    "android",
	})

	row := h.waitForState(t, domain.InstanceConnected)
	require.NotNil(t, row.WaJid)
	assert.Equal(t, "5511999999999:11@s.whatsapp.net", *row.WaJid,
		"the companion device suffix must be persisted, not stripped")
	require.NotNil(t, row.PhoneNumber)
	assert.Equal(t, "5511999999999", *row.PhoneNumber)
	assert.NotNil(t, row.PairedAt)

	assert.Subset(t, h.trail(t), []string{"pairing_started", "pairing_succeeded", "connected"})

	// The stored code is cleared once pairing ends: serving it afterwards would
	// render a QR nobody can scan.
	_, found, err = h.publisher.Pairing(h.ctx, h.instanceID)
	require.NoError(t, err)
	assert.False(t, found)
}

func TestConnectOnConnectedSessionIsIdempotent(t *testing.T) {
	h := newHarness(t)
	session := h.adopt(t)

	_, err := h.supervisor.Connect(h.ctx, h.instanceID)
	require.NoError(t, err)
	session.EmitPairSuccess(domain.DeviceIdentity{
		JID: "5511999999999:11@s.whatsapp.net", PhoneNumber: "5511999999999",
	})
	h.waitForState(t, domain.InstanceConnected)

	result, err := h.supervisor.Connect(h.ctx, h.instanceID)
	require.NoError(t, err)
	assert.Equal(t, domain.InstanceConnected, result.State)
	assert.False(t, result.PairingStarted, "an already connected session must not generate a new QR")
}

func TestPairingExpiryReturnsToThePreviousState(t *testing.T) {
	h := newHarness(t)
	session := h.adopt(t)

	stream, err := h.subscriber.Subscribe(h.ctx, h.instanceID)
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()

	_, err = h.supervisor.Connect(h.ctx, h.instanceID)
	require.NoError(t, err)
	h.waitForState(t, domain.InstancePairing)

	session.EmitPairingExpired(wa.ExpiryWindowExhausted)

	h.waitForState(t, domain.InstanceRegistered)
	assert.Contains(t, h.trail(t), "pairing_expired")

	var expired bool
	for range 3 {
		if receive(t, stream).Type == events.TypePairingExpired {
			expired = true
			break
		}
	}
	assert.True(t, expired, "the client must be told the attempt ended")
}

// US5: an invalidation stops the session for good, and the permanent reason is
// what keeps reconciliation from picking it up again.
func TestLogoutFromPhoneStopsTheSessionForGood(t *testing.T) {
	h := newHarness(t)
	session := h.adopt(t)

	_, err := h.supervisor.Connect(h.ctx, h.instanceID)
	require.NoError(t, err)
	session.EmitPairSuccess(domain.DeviceIdentity{
		JID: "5511999999999:11@s.whatsapp.net", PhoneNumber: "5511999999999",
	})
	h.waitForState(t, domain.InstanceConnected)

	session.EmitDisconnect(domain.ReasonLoggedOutFromPhone)

	row := h.waitForState(t, domain.InstanceLoggedOut)
	require.NotNil(t, row.LastDisconnectReason)
	assert.Equal(t, string(domain.ReasonLoggedOutFromPhone), *row.LastDisconnectReason)
	assert.Contains(t, h.trail(t), "logged_out")

	assert.Eventually(t, func() bool { return !h.supervisor.Owns(h.instanceID) },
		10*time.Second, 25*time.Millisecond, "a dead session must not stay resident")

	adoptable, err := h.leases.Adoptable(h.ctx, 10)
	require.NoError(t, err)
	assert.NotContains(t, adoptable, h.instanceID,
		"reconciliation must not keep retrying a logged-out session")
}

func TestTemporaryBanRecordsTheDeadline(t *testing.T) {
	h := newHarness(t)
	session := h.adopt(t)

	_, err := h.supervisor.Connect(h.ctx, h.instanceID)
	require.NoError(t, err)
	session.EmitPairSuccess(domain.DeviceIdentity{JID: "5511999999999:11@s.whatsapp.net", PhoneNumber: "5511999999999"})
	h.waitForState(t, domain.InstanceConnected)

	session.EmitBan(24 * time.Hour)

	row := h.waitForState(t, domain.InstanceBanned)
	require.NotNil(t, row.BanExpiresAt)
	assert.WithinDuration(t, time.Now().Add(24*time.Hour), *row.BanExpiresAt, time.Minute)
	assert.Contains(t, h.trail(t), "banned")
}

// A network drop is not an invalidation: the instance stays adoptable so the
// system keeps trying.
func TestNetworkDropKeepsTheSessionRetryable(t *testing.T) {
	h := newHarness(t)
	session := h.adopt(t)

	_, err := h.supervisor.Connect(h.ctx, h.instanceID)
	require.NoError(t, err)
	session.EmitPairSuccess(domain.DeviceIdentity{JID: "5511999999999:11@s.whatsapp.net", PhoneNumber: "5511999999999"})
	h.waitForState(t, domain.InstanceConnected)

	session.EmitDisconnect(domain.ReasonNetwork)

	row := h.waitForState(t, domain.InstanceConnecting)
	require.NotNil(t, row.LastDisconnectReason)
	assert.Equal(t, string(domain.ReasonNetwork), *row.LastDisconnectReason)
	assert.True(t, h.supervisor.Owns(h.instanceID), "a transient drop must not release the session")
}

func TestDisconnectKeepsTheDeviceMaterial(t *testing.T) {
	h := newHarness(t)
	session := h.adopt(t)

	_, err := h.supervisor.Connect(h.ctx, h.instanceID)
	require.NoError(t, err)
	session.EmitPairSuccess(domain.DeviceIdentity{JID: "5511999999999:11@s.whatsapp.net", PhoneNumber: "5511999999999"})
	h.waitForState(t, domain.InstanceConnected)

	state, err := h.supervisor.Disconnect(h.ctx, h.instanceID)
	require.NoError(t, err)
	assert.Equal(t, domain.InstanceDisconnected, state)

	row := h.instance(t)
	assert.Equal(t, string(domain.InstanceDisconnected), row.ConnectionState)
	require.NotNil(t, row.WaJid, "disconnect must not delete the pairing")
	assert.False(t, h.supervisor.Owns(h.instanceID))
}

func TestDisconnectOnUnheldSessionIsAccepted(t *testing.T) {
	h := newHarness(t)

	// Nothing running here is already the requested outcome; a repeat must not
	// look like a failure (FR-008).
	state, err := h.supervisor.Disconnect(h.ctx, h.instanceID)
	require.NoError(t, err)
	assert.Equal(t, domain.InstanceDisconnected, state)
}

func TestLogoutClearsTheDeviceIdentity(t *testing.T) {
	h := newHarness(t)
	session := h.adopt(t)

	_, err := h.supervisor.Connect(h.ctx, h.instanceID)
	require.NoError(t, err)
	session.EmitPairSuccess(domain.DeviceIdentity{JID: "5511999999999:11@s.whatsapp.net", PhoneNumber: "5511999999999"})
	h.waitForState(t, domain.InstanceConnected)

	remoteRemoved, err := h.supervisor.Logout(h.ctx, h.instanceID, false)
	require.NoError(t, err)
	assert.True(t, remoteRemoved)

	row := h.instance(t)
	assert.Equal(t, string(domain.InstanceRegistered), row.ConnectionState)
	assert.Nil(t, row.WaJid, "logout must delete the session material")
	assert.Nil(t, row.PhoneNumber)
	assert.Contains(t, h.trail(t), "logged_out")
}

// When the server cannot be reached the material is still dropped locally, and
// the caller learns the device may remain listed on the handset (research R10).
func TestLogoutFallsBackToLocalWipe(t *testing.T) {
	h := newHarness(t)
	session := h.adopt(t)
	session.LogoutRemoteFails = true

	_, err := h.supervisor.Connect(h.ctx, h.instanceID)
	require.NoError(t, err)
	session.EmitPairSuccess(domain.DeviceIdentity{JID: "5511999999999:11@s.whatsapp.net", PhoneNumber: "5511999999999"})
	h.waitForState(t, domain.InstanceConnected)

	remoteRemoved, err := h.supervisor.Logout(h.ctx, h.instanceID, true)
	require.NoError(t, err)
	assert.False(t, remoteRemoved, "the caller must be told the device was not removed remotely")

	row := h.instance(t)
	assert.Equal(t, string(domain.InstanceRegistered), row.ConnectionState)
	assert.Nil(t, row.WaJid)
}

// Re-pairing with a different number is allowed — instances are companion
// devices, not numbers — but the swap has to show up in the trail.
//
// The realistic path goes through a logout, which clears the identity from the
// instance row; the detection therefore has to come from the trail, not from
// the row (FR-016).
func TestNumberChangeIsRecorded(t *testing.T) {
	h := newHarness(t)
	session := h.adopt(t)

	_, err := h.supervisor.Connect(h.ctx, h.instanceID)
	require.NoError(t, err)
	session.EmitPairSuccess(domain.DeviceIdentity{
		JID: "5511888888888:11@s.whatsapp.net", PhoneNumber: "5511888888888",
	})
	h.waitForState(t, domain.InstanceConnected)

	_, err = h.supervisor.Logout(h.ctx, h.instanceID, false)
	require.NoError(t, err)

	// Logout releases the session, so the worker adopts the instance again
	// before the second pairing — exactly what happens in production.
	require.NoError(t, h.leases.SetDesired(h.ctx, h.instanceID, lease.DesiredRunning))
	delete(h.factory.sessions, h.instanceID)
	second := h.adopt(t)

	_, err = h.supervisor.Connect(h.ctx, h.instanceID)
	require.NoError(t, err)
	second.EmitPairSuccess(domain.DeviceIdentity{
		JID: "5511999999999:12@s.whatsapp.net", PhoneNumber: "5511999999999",
	})
	h.waitForState(t, domain.InstanceConnected)

	require.Eventually(t, func() bool {
		for _, entry := range h.trail(t) {
			if entry == "number_changed" {
				return true
			}
		}
		return false
	}, 20*time.Second, 25*time.Millisecond, "a number swap must be visible in the trail")

	row := h.instance(t)
	require.NotNil(t, row.PhoneNumber)
	assert.Equal(t, "5511999999999", *row.PhoneNumber)
}

// A first pairing has nothing before it, so it must not be reported as a swap.
func TestFirstPairingIsNotANumberChange(t *testing.T) {
	h := newHarness(t)
	session := h.adopt(t)

	_, err := h.supervisor.Connect(h.ctx, h.instanceID)
	require.NoError(t, err)
	session.EmitPairSuccess(domain.DeviceIdentity{
		JID: "5511999999999:11@s.whatsapp.net", PhoneNumber: "5511999999999",
	})
	h.waitForState(t, domain.InstanceConnected)

	assert.NotContains(t, h.trail(t), "number_changed")
}

func TestStopOnRequestReleasesTheSession(t *testing.T) {
	h := newHarness(t)
	session := h.adopt(t)

	_, err := h.supervisor.Connect(h.ctx, h.instanceID)
	require.NoError(t, err)
	session.EmitPairSuccess(domain.DeviceIdentity{JID: "5511999999999:11@s.whatsapp.net", PhoneNumber: "5511999999999"})
	h.waitForState(t, domain.InstanceConnected)

	h.supervisor.StopOnRequest(h.ctx, h.instanceID, domain.ReasonTenantSuspended)

	row := h.instance(t)
	assert.Equal(t, string(domain.InstanceDisconnected), row.ConnectionState)
	require.NotNil(t, row.LastDisconnectReason)
	assert.Equal(t, string(domain.ReasonTenantSuspended), *row.LastDisconnectReason)
	assert.False(t, h.supervisor.Owns(h.instanceID))
}

// Draining must hand every lease back so another worker adopts in seconds
// rather than waiting out the expiry — that is what keeps a deploy cheap.
func TestShutdownReleasesEverySession(t *testing.T) {
	h := newHarness(t)
	h.adopt(t)

	require.NoError(t, h.supervisor.Shutdown(h.ctx))

	assert.True(t, h.supervisor.Draining())
	assert.Equal(t, 0, h.supervisor.Count())
	assert.False(t, h.supervisor.HasCapacity(), "a draining worker must refuse new work")

	other := lease.New(h.infra.Queries, lease.Options{
		WorkerID: "worker-other", GRPCAddr: "127.0.0.1:9091", Expiry: 30 * time.Second,
	})
	adoptable, err := other.Adoptable(h.ctx, 10)
	require.NoError(t, err)
	assert.Contains(t, adoptable, h.instanceID)
}

func TestCapacityIsRespected(t *testing.T) {
	h := newHarness(t)
	assert.Equal(t, 10, h.supervisor.Capacity())

	h.adopt(t)
	assert.Equal(t, 9, h.supervisor.Capacity())
	assert.True(t, h.supervisor.HasCapacity())
}

func receive(t *testing.T, stream *events.Stream) events.Envelope {
	t.Helper()
	select {
	case envelope, ok := <-stream.Events():
		require.True(t, ok, "stream closed before delivering an event")
		return envelope
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for an event")
		return events.Envelope{}
	}
}

// FR-019: a pairing WhatsApp rejects is a normal outcome with a reason of its
// own, not an internal error and not silence.
func TestPairingFailureIsReportedAndReversed(t *testing.T) {
	h := newHarness(t)
	session := h.adopt(t)

	stream, err := h.subscriber.Subscribe(h.ctx, h.instanceID)
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()

	_, err = h.supervisor.Connect(h.ctx, h.instanceID)
	require.NoError(t, err)
	h.waitForState(t, domain.InstancePairing)

	session.EmitPairingFailed(wa.FailureScannedWithoutMultidevice)

	h.waitForState(t, domain.InstanceRegistered)
	assert.Contains(t, h.trail(t), "pairing_failed")

	var failed bool
	for range 4 {
		frame := receive(t, stream)
		if frame.Type == events.TypePairingFailed {
			failed = true
			assert.Equal(t, string(wa.FailureScannedWithoutMultidevice), frame.Data["reason"],
				"the tenant must learn which failure it was, not just that one happened")
			break
		}
	}
	assert.True(t, failed, "the client must be told the attempt failed")
}

// The code rotates while the attempt is open, and every rotation reaches both
// the bus and the snapshot key — a client arriving late sees the current one.
func TestPairingCodesRotate(t *testing.T) {
	h := newHarness(t)
	session := h.adopt(t)

	stream, err := h.subscriber.Subscribe(h.ctx, h.instanceID)
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()

	_, err = h.supervisor.Connect(h.ctx, h.instanceID)
	require.NoError(t, err)
	h.waitForState(t, domain.InstancePairing)

	session.EmitPairingCode("2@first", true)
	first := receive(t, stream)
	require.Equal(t, events.TypePairingCode, first.Type)
	assert.Equal(t, "2@first", first.Data["code"])

	session.EmitPairingCode("2@second", false)
	second := receive(t, stream)
	require.Equal(t, events.TypePairingCode, second.Type)
	assert.Equal(t, "2@second", second.Data["code"])

	assert.Greater(t, second.Seq, first.Seq, "the sequence must advance so clients can spot a gap")

	snapshot, found, err := h.publisher.Pairing(h.ctx, h.instanceID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "2@second", snapshot.Code, "the snapshot must hold the newest code, not the first")
}

// A pairing attempt on an instance that already holds device material is
// refused by the library; the worker must surface that rather than hang.
func TestPairingIsRefusedWhenAlreadyPaired(t *testing.T) {
	h := newHarness(t)
	session := h.adopt(t)

	_, err := h.supervisor.Connect(h.ctx, h.instanceID)
	require.NoError(t, err)
	session.EmitPairSuccess(domain.DeviceIdentity{
		JID: "5511999999999:11@s.whatsapp.net", PhoneNumber: "5511999999999",
	})
	h.waitForState(t, domain.InstanceConnected)

	_, _, err = h.supervisor.PairPhone(h.ctx, h.instanceID, "5511888888888", true)
	require.Error(t, err)
	assert.ErrorIs(t, err, wa.ErrAlreadyPaired)
}
