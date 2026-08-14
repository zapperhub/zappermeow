package worker_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/events"
	"github.com/zapperhub/zappermeow/internal/lease"
	"github.com/zapperhub/zappermeow/internal/wa"
	"github.com/zapperhub/zappermeow/internal/worker"
)

// fleet is two workers over the same database, which is what a deploy looks
// like from the leases' point of view.
type fleet struct {
	harness *harness
	first   *worker.Supervisor
	second  *worker.Supervisor
	leasesA *lease.Manager
	leasesB *lease.Manager
	factory *fakeFactory
}

func newFleet(t *testing.T) *fleet {
	t.Helper()

	h := newHarness(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	factory := &fakeFactory{sessions: map[domain.ID]*wa.FakeSession{}}

	build := func(workerID, addr string) (*worker.Supervisor, *lease.Manager) {
		leases := lease.New(h.infra.Queries, lease.Options{
			WorkerID: workerID,
			GRPCAddr: addr,
			Expiry:   30 * time.Second,
		})
		supervisor := worker.NewSupervisor(worker.Options{
			Queries:       h.infra.Queries,
			Leases:        leases,
			Publisher:     events.NewPublisher(h.infra.Redis),
			Factory:       factory,
			Logger:        logger,
			PairingWindow: 3 * time.Second,
			MaxSessions:   10,
		})
		t.Cleanup(func() { _ = supervisor.Shutdown(context.Background()) })
		return supervisor, leases
	}

	first, leasesA := build("worker-a", "10.0.0.1:9090")
	second, leasesB := build("worker-b", "10.0.0.2:9090")

	return &fleet{harness: h, first: first, second: second, leasesA: leasesA, leasesB: leasesB, factory: factory}
}

// killOwner simulates a process that died: the heartbeat simply stops, which is
// all a crashed worker leaves behind.
func (f *fleet) expireHeartbeat(t *testing.T) {
	t.Helper()
	_, err := f.harness.infra.Pool.Exec(f.harness.ctx,
		`UPDATE session_leases SET heartbeat_at = now() - interval '2 minutes' WHERE instance_id = $1`,
		f.harness.instanceID)
	require.NoError(t, err)
}

// US4 scenario 1: a session whose owner died is picked up by another worker,
// with no human involved and no new pairing.
func TestSessionIsAdoptedAfterTheOwnerDies(t *testing.T) {
	f := newFleet(t)
	h := f.harness

	require.NoError(t, f.first.Adopt(h.ctx, h.instanceID))
	require.True(t, f.first.Owns(h.instanceID))

	// While the first worker is alive its session cannot be taken.
	require.Error(t, f.second.Adopt(h.ctx, h.instanceID))

	f.expireHeartbeat(t)

	require.NoError(t, f.second.Adopt(h.ctx, h.instanceID))
	assert.True(t, f.second.Owns(h.instanceID))

	owner, err := f.leasesB.Owner(h.ctx, h.instanceID)
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.2:9090", owner.GRPCAddr)
	assert.Equal(t, int64(2), owner.Generation, "each acquisition bumps the fencing token")
}

// Fencing: the previous owner may still be running — a long GC pause, a
// partition — and must not be able to act on a session it no longer holds.
func TestFormerOwnerIsFencedOut(t *testing.T) {
	f := newFleet(t)
	h := f.harness

	require.NoError(t, f.first.Adopt(h.ctx, h.instanceID))
	firstGen, err := f.leasesA.Owner(h.ctx, h.instanceID)
	require.NoError(t, err)

	f.expireHeartbeat(t)
	require.NoError(t, f.second.Adopt(h.ctx, h.instanceID))

	// The old generation is refused, and so is the new one in the old worker's
	// hands: holding the right number is not enough, ownership is checked too.
	assert.ErrorIs(t, f.leasesA.CheckGeneration(h.ctx, h.instanceID, firstGen.Generation), lease.ErrWrongGeneration)
	assert.ErrorIs(t, f.leasesA.CheckGeneration(h.ctx, h.instanceID, firstGen.Generation+1), lease.ErrWrongGeneration)
	require.NoError(t, f.leasesB.CheckGeneration(h.ctx, h.instanceID, firstGen.Generation+1))
}

// A worker that lost its lease learns it from the heartbeat and must let go
// before the new owner connects — two live sessions is the one thing the lease
// exists to prevent.
func TestHeartbeatLossDropsTheSession(t *testing.T) {
	f := newFleet(t)
	h := f.harness

	require.NoError(t, f.first.Adopt(h.ctx, h.instanceID))
	f.expireHeartbeat(t)
	require.NoError(t, f.second.Adopt(h.ctx, h.instanceID))

	held, err := f.leasesA.Heartbeat(h.ctx)
	require.NoError(t, err)
	require.NotContains(t, held, h.instanceID, "the stolen lease must not be renewed")

	f.first.Drop(h.ctx, h.instanceID)
	assert.False(t, f.first.Owns(h.instanceID))
	assert.True(t, f.second.Owns(h.instanceID), "the new owner keeps the session")
	h.waitForTrail(t, "lease_lost")
}

// US4 scenario 2: a planned shutdown hands the leases back, so the other worker
// adopts in seconds instead of waiting out the expiry.
func TestDrainHandsSessionsOverImmediately(t *testing.T) {
	f := newFleet(t)
	h := f.harness

	require.NoError(t, f.first.Adopt(h.ctx, h.instanceID))
	require.NoError(t, f.first.Shutdown(h.ctx))

	// No expiry was faked here: the drain alone makes the session adoptable.
	adoptable, err := f.leasesB.Adoptable(h.ctx, 10)
	require.NoError(t, err)
	require.Contains(t, adoptable, h.instanceID)

	require.NoError(t, f.second.Adopt(h.ctx, h.instanceID))
	assert.True(t, f.second.Owns(h.instanceID))

	assert.False(t, f.first.HasCapacity(), "a drained worker must refuse new work")
}

// US4 scenario 4: an instance the user disconnected stays offline across a
// restart. The intent is what decides, not the last observed state.
func TestStoppedInstanceIsNotAdoptedAfterRestart(t *testing.T) {
	f := newFleet(t)
	h := f.harness

	require.NoError(t, f.first.Adopt(h.ctx, h.instanceID))
	_, err := f.first.Disconnect(h.ctx, h.instanceID)
	require.NoError(t, err)

	// A fresh worker booting sees nothing to do.
	adoptable, err := f.leasesB.Adoptable(h.ctx, 10)
	require.NoError(t, err)
	assert.NotContains(t, adoptable, h.instanceID)
}

// US4 scenario 5: an instance the user wants online comes back by itself.
func TestActiveInstanceIsAdoptedOnBoot(t *testing.T) {
	f := newFleet(t)
	h := f.harness

	// The lease already says running, as it would after a connect command.
	adoptable, err := f.leasesB.Adoptable(h.ctx, 10)
	require.NoError(t, err)
	require.Contains(t, adoptable, h.instanceID)

	require.NoError(t, f.second.Adopt(h.ctx, h.instanceID))
	assert.True(t, f.second.Owns(h.instanceID))
}

// Capacity is an operational limit, not a product quota: a worker at its ceiling
// leaves the session for another one rather than refusing it outright.
func TestWorkerAtCapacityLeavesTheSessionForAnother(t *testing.T) {
	h := newHarness(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	factory := &fakeFactory{sessions: map[domain.ID]*wa.FakeSession{}}

	full := worker.NewSupervisor(worker.Options{
		Queries:       h.infra.Queries,
		Leases:        lease.New(h.infra.Queries, lease.Options{WorkerID: "worker-full", GRPCAddr: "10.0.0.3:9090", Expiry: 30 * time.Second}),
		Publisher:     events.NewPublisher(h.infra.Redis),
		Factory:       factory,
		Logger:        logger,
		PairingWindow: 3 * time.Second,
		MaxSessions:   0,
	})
	t.Cleanup(func() { _ = full.Shutdown(context.Background()) })

	assert.False(t, full.HasCapacity())
	assert.Equal(t, 0, full.Capacity())
	require.Error(t, full.Adopt(h.ctx, h.instanceID))

	// And the session stays free for a worker that has room.
	roomy := lease.New(h.infra.Queries, lease.Options{WorkerID: "worker-roomy", GRPCAddr: "10.0.0.4:9090", Expiry: 30 * time.Second})
	adoptable, err := roomy.Adoptable(h.ctx, 10)
	require.NoError(t, err)
	assert.Contains(t, adoptable, h.instanceID)
}

// Adopting a lease is not the same as bringing the number online.
//
// Without dialling here, a restart leaves the instance owned and silent while
// the database still reports whatever it said before — and a logout done from
// the handset never arrives, because that event only travels over a live
// socket. This is US4 scenario 5, and asserting ownership alone would have
// missed it entirely.
func TestAdoptedSessionIsActuallyConnected(t *testing.T) {
	h := newHarness(t)

	// A paired instance the tenant wants online, exactly as a restart finds it.
	_, err := h.infra.Pool.Exec(h.ctx, `
		UPDATE instances
		SET wa_jid = '5511999999999:11@s.whatsapp.net',
		    phone_number = '5511999999999',
		    connection_intent = 'active',
		    connection_state = 'connected',
		    paired_at = now()
		WHERE id = $1`, h.instanceID)
	require.NoError(t, err)

	paired := wa.NewPairedFakeSession(domain.DeviceIdentity{
		JID: "5511999999999:11@s.whatsapp.net", PhoneNumber: "5511999999999",
	})
	h.factory.sessions[h.instanceID] = paired

	require.NoError(t, h.supervisor.Adopt(h.ctx, h.instanceID))

	require.Eventually(t, func() bool { return paired.Status().Connected },
		10*time.Second, 25*time.Millisecond,
		"an adopted session that should be running was never dialled")

	// And the connection is recorded, so the state stops being a leftover from
	// before the restart.
	h.waitForState(t, domain.InstanceConnected)
	h.waitForTrail(t, "connected")
}

// An instance the tenant stopped must be adopted by nobody — and if a lease is
// somehow held, it still must not be dialled.
func TestAdoptedSessionWithStoppedIntentIsNotDialled(t *testing.T) {
	h := newHarness(t)

	_, err := h.infra.Pool.Exec(h.ctx, `
		UPDATE instances
		SET wa_jid = '5511999999999:11@s.whatsapp.net',
		    connection_intent = 'stopped'
		WHERE id = $1`, h.instanceID)
	require.NoError(t, err)

	paired := wa.NewPairedFakeSession(domain.DeviceIdentity{JID: "5511999999999:11@s.whatsapp.net"})
	h.factory.sessions[h.instanceID] = paired

	require.NoError(t, h.supervisor.Adopt(h.ctx, h.instanceID))

	time.Sleep(500 * time.Millisecond)
	assert.False(t, paired.Status().Connected,
		"a number the tenant switched off must stay off, whoever holds the lease")
}
