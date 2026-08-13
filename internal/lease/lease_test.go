package lease_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/lease"
	"github.com/zapperhub/zappermeow/internal/store/testutil"
)

const testExpiry = 30 * time.Second

// seedInstance creates the tenant and instance rows a lease points at. Real
// Postgres, real foreign keys: the lease cannot be tested against anything less
// without losing the guarantee it exists to provide.
func seedInstance(t *testing.T, infra *testutil.Infra) domain.ID {
	t.Helper()
	ctx := context.Background()

	tenantID := uuid.New()
	_, err := infra.Pool.Exec(ctx,
		`INSERT INTO tenants (id, name) VALUES ($1, $2)`, tenantID, "tenant-"+tenantID.String()[:8])
	require.NoError(t, err)

	instanceID := uuid.New()
	_, err = infra.Pool.Exec(ctx,
		`INSERT INTO instances (id, tenant_id, name) VALUES ($1, $2, $3)`,
		instanceID, tenantID, "instance-"+instanceID.String()[:8])
	require.NoError(t, err)

	return domain.ID(instanceID)
}

func newManager(infra *testutil.Infra, workerID, addr string) *lease.Manager {
	return lease.New(infra.Queries, lease.Options{
		WorkerID: workerID,
		GRPCAddr: addr,
		Expiry:   testExpiry,
	})
}

func setup(t *testing.T) (*testutil.Infra, context.Context) {
	t.Helper()
	infra := testutil.Shared(t)
	infra.Reset(t)
	return infra, context.Background()
}

// The invariant of Principle III: however many workers race, exactly one owns
// the session. Anything else corrupts Signal state irrecoverably.
func TestConcurrentAcquisitionHasExactlyOneWinner(t *testing.T) {
	infra, ctx := setup(t)
	instanceID := seedInstance(t, infra)

	owner := newManager(infra, "worker-0", "10.0.0.1:9090")
	require.NoError(t, owner.Ensure(ctx, instanceID))
	require.NoError(t, owner.SetDesired(ctx, instanceID, lease.DesiredRunning))

	const workers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
		gens    []int64
	)

	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := newManager(infra, "worker-"+string(rune('a'+i)), "10.0.0.1:909"+string(rune('0'+i)))

			<-start // maximise the overlap
			generation, err := m.Acquire(ctx, instanceID)
			if err != nil {
				assert.ErrorIs(t, err, lease.ErrNotAcquired, "losers must lose cleanly, not error out")
				return
			}
			mu.Lock()
			winners = append(winners, m.WorkerID())
			gens = append(gens, generation)
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()

	require.Len(t, winners, 1, "exactly one worker may own the session")
	assert.Equal(t, []int64{1}, gens, "the first acquisition is generation 1")
}

func TestAcquireRequiresRunningDesiredState(t *testing.T) {
	infra, ctx := setup(t)
	instanceID := seedInstance(t, infra)

	m := newManager(infra, "worker-a", "10.0.0.1:9090")
	require.NoError(t, m.Ensure(ctx, instanceID))

	// A freshly created row defaults to stopped: creating it must never be
	// enough to start a session.
	_, err := m.Acquire(ctx, instanceID)
	require.ErrorIs(t, err, lease.ErrNotAcquired)

	require.NoError(t, m.SetDesired(ctx, instanceID, lease.DesiredRunning))
	generation, err := m.Acquire(ctx, instanceID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), generation)
}

func TestLiveLeaseIsNotStolen(t *testing.T) {
	infra, ctx := setup(t)
	instanceID := seedInstance(t, infra)

	first := newManager(infra, "worker-a", "10.0.0.1:9090")
	require.NoError(t, first.Ensure(ctx, instanceID))
	require.NoError(t, first.SetDesired(ctx, instanceID, lease.DesiredRunning))
	_, err := first.Acquire(ctx, instanceID)
	require.NoError(t, err)

	second := newManager(infra, "worker-b", "10.0.0.2:9090")
	_, err = second.Acquire(ctx, instanceID)
	assert.ErrorIs(t, err, lease.ErrNotAcquired, "a heartbeating owner keeps its session")
}

// Failover: once the heartbeat goes stale the session becomes adoptable, and
// the generation bump is what makes the dead owner's commands unusable.
func TestExpiredLeaseIsAdoptedWithNewGeneration(t *testing.T) {
	infra, ctx := setup(t)
	instanceID := seedInstance(t, infra)

	dead := newManager(infra, "worker-dead", "10.0.0.1:9090")
	require.NoError(t, dead.Ensure(ctx, instanceID))
	require.NoError(t, dead.SetDesired(ctx, instanceID, lease.DesiredRunning))
	firstGen, err := dead.Acquire(ctx, instanceID)
	require.NoError(t, err)

	// Simulate a process that died: no more heartbeats, expiry elapsed.
	_, err = infra.Pool.Exec(ctx,
		`UPDATE session_leases SET heartbeat_at = now() - interval '2 minutes' WHERE instance_id = $1`,
		uuid.UUID(instanceID))
	require.NoError(t, err)

	alive := newManager(infra, "worker-alive", "10.0.0.2:9090")
	adoptable, err := alive.Adoptable(ctx, 10)
	require.NoError(t, err)
	require.Contains(t, adoptable, instanceID)

	secondGen, err := alive.Acquire(ctx, instanceID)
	require.NoError(t, err)
	assert.Greater(t, secondGen, firstGen, "each acquisition must bump the fencing token")

	owner, err := alive.Owner(ctx, instanceID)
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.2:9090", owner.GRPCAddr)
	assert.True(t, owner.Live)
}

// Fencing: the previous owner may still be running — a long GC pause, a network
// partition — and must not be able to touch the session it no longer owns.
func TestFencingRejectsThePreviousOwner(t *testing.T) {
	infra, ctx := setup(t)
	instanceID := seedInstance(t, infra)

	first := newManager(infra, "worker-a", "10.0.0.1:9090")
	require.NoError(t, first.Ensure(ctx, instanceID))
	require.NoError(t, first.SetDesired(ctx, instanceID, lease.DesiredRunning))
	firstGen, err := first.Acquire(ctx, instanceID)
	require.NoError(t, err)
	require.NoError(t, first.CheckGeneration(ctx, instanceID, firstGen))

	require.NoError(t, first.Release(ctx, instanceID))
	second := newManager(infra, "worker-b", "10.0.0.2:9090")
	secondGen, err := second.Acquire(ctx, instanceID)
	require.NoError(t, err)

	// Same generation number, wrong worker, and the old generation on the old
	// worker: both have to be refused.
	assert.ErrorIs(t, first.CheckGeneration(ctx, instanceID, firstGen), lease.ErrWrongGeneration)
	assert.ErrorIs(t, first.CheckGeneration(ctx, instanceID, secondGen), lease.ErrWrongGeneration,
		"holding the right number is not enough; the worker must be the owner")
	require.NoError(t, second.CheckGeneration(ctx, instanceID, secondGen))
}

func TestHeartbeatReportsOnlyLeasesStillHeld(t *testing.T) {
	infra, ctx := setup(t)
	kept := seedInstance(t, infra)
	stolen := seedInstance(t, infra)

	worker := newManager(infra, "worker-a", "10.0.0.1:9090")
	for _, id := range []domain.ID{kept, stolen} {
		require.NoError(t, worker.Ensure(ctx, id))
		require.NoError(t, worker.SetDesired(ctx, id, lease.DesiredRunning))
		_, err := worker.Acquire(ctx, id)
		require.NoError(t, err)
	}

	// Another worker takes one of them over after an expiry.
	_, err := infra.Pool.Exec(ctx,
		`UPDATE session_leases SET heartbeat_at = now() - interval '2 minutes' WHERE instance_id = $1`,
		uuid.UUID(stolen))
	require.NoError(t, err)
	thief := newManager(infra, "worker-b", "10.0.0.2:9090")
	_, err = thief.Acquire(ctx, stolen)
	require.NoError(t, err)

	held, err := worker.Heartbeat(ctx)
	require.NoError(t, err)

	assert.Contains(t, held, kept)
	assert.NotContains(t, held, stolen,
		"a lease missing from the heartbeat tells the worker to drop that session now")
}

func TestReleaseAllHandsBackEverySession(t *testing.T) {
	infra, ctx := setup(t)
	first := seedInstance(t, infra)
	second := seedInstance(t, infra)

	worker := newManager(infra, "worker-a", "10.0.0.1:9090")
	for _, id := range []domain.ID{first, second} {
		require.NoError(t, worker.Ensure(ctx, id))
		require.NoError(t, worker.SetDesired(ctx, id, lease.DesiredRunning))
		_, err := worker.Acquire(ctx, id)
		require.NoError(t, err)
	}

	require.NoError(t, worker.ReleaseAll(ctx))

	// A graceful drain makes the sessions adoptable immediately, without
	// waiting out the expiry — this is what keeps a deploy to seconds.
	other := newManager(infra, "worker-b", "10.0.0.2:9090")
	adoptable, err := other.Adoptable(ctx, 10)
	require.NoError(t, err)
	assert.ElementsMatch(t, []domain.ID{first, second}, adoptable)

	// Desired state and generation survive the handover.
	generation, err := other.Acquire(ctx, first)
	require.NoError(t, err)
	assert.Equal(t, int64(2), generation, "generation continues rather than restarting")
}

// Instances parked on a permanent failure must stay invisible to reconciliation:
// reconnecting a logged-out or banned number cannot recover it.
func TestAdoptableSkipsPermanentFailures(t *testing.T) {
	infra, ctx := setup(t)
	retryable := seedInstance(t, infra)
	loggedOut := seedInstance(t, infra)
	banned := seedInstance(t, infra)

	worker := newManager(infra, "worker-a", "10.0.0.1:9090")
	for _, id := range []domain.ID{retryable, loggedOut, banned} {
		require.NoError(t, worker.Ensure(ctx, id))
		require.NoError(t, worker.SetDesired(ctx, id, lease.DesiredRunning))
	}

	for id, reason := range map[domain.ID]domain.DisconnectReason{
		retryable: domain.ReasonNetwork,
		loggedOut: domain.ReasonLoggedOutFromPhone,
		banned:    domain.ReasonTemporaryBan,
	} {
		_, err := infra.Pool.Exec(ctx,
			`UPDATE instances SET last_disconnect_reason = $2 WHERE id = $1`, uuid.UUID(id), string(reason))
		require.NoError(t, err)
	}

	adoptable, err := worker.Adoptable(ctx, 10)
	require.NoError(t, err)

	assert.Contains(t, adoptable, retryable, "a network drop must keep being retried")
	assert.NotContains(t, adoptable, loggedOut)
	assert.NotContains(t, adoptable, banned)
}

func TestAdoptableRespectsTheRowLimit(t *testing.T) {
	infra, ctx := setup(t)
	worker := newManager(infra, "worker-a", "10.0.0.1:9090")

	for range 5 {
		id := seedInstance(t, infra)
		require.NoError(t, worker.Ensure(ctx, id))
		require.NoError(t, worker.SetDesired(ctx, id, lease.DesiredRunning))
	}

	adoptable, err := worker.Adoptable(ctx, 3)
	require.NoError(t, err)
	assert.Len(t, adoptable, 3, "capacity is respected at the query, not after loading everything")
}

func TestStoppedLeaseIsNeverAdopted(t *testing.T) {
	infra, ctx := setup(t)
	instanceID := seedInstance(t, infra)

	worker := newManager(infra, "worker-a", "10.0.0.1:9090")
	require.NoError(t, worker.Ensure(ctx, instanceID))
	require.NoError(t, worker.SetDesired(ctx, instanceID, lease.DesiredRunning))
	_, err := worker.Acquire(ctx, instanceID)
	require.NoError(t, err)

	require.NoError(t, worker.SetDesired(ctx, instanceID, lease.DesiredStopped))
	require.NoError(t, worker.ReleaseAll(ctx))

	adoptable, err := worker.Adoptable(ctx, 10)
	require.NoError(t, err)
	assert.NotContains(t, adoptable, instanceID,
		"an instance the user disconnected must stay offline across restarts")
}

func TestOwnerReportsStaleHeartbeatAsNotLive(t *testing.T) {
	infra, ctx := setup(t)
	instanceID := seedInstance(t, infra)

	worker := newManager(infra, "worker-a", "10.0.0.1:9090")
	require.NoError(t, worker.Ensure(ctx, instanceID))
	require.NoError(t, worker.SetDesired(ctx, instanceID, lease.DesiredRunning))
	_, err := worker.Acquire(ctx, instanceID)
	require.NoError(t, err)

	owner, err := worker.Owner(ctx, instanceID)
	require.NoError(t, err)
	require.True(t, owner.Live)

	_, err = infra.Pool.Exec(ctx,
		`UPDATE session_leases SET heartbeat_at = now() - interval '2 minutes' WHERE instance_id = $1`,
		uuid.UUID(instanceID))
	require.NoError(t, err)

	owner, err = worker.Owner(ctx, instanceID)
	require.NoError(t, err)
	assert.False(t, owner.Live, "the API must not dial an address whose owner stopped heartbeating")
	assert.Equal(t, "10.0.0.1:9090", owner.GRPCAddr, "the address is still reported, just not trusted")
}

func TestSetTenantDesiredProjectsOntoEveryInstance(t *testing.T) {
	infra, ctx := setup(t)
	ctxTenant := uuid.New()
	_, err := infra.Pool.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ($1, $2)`, ctxTenant, "acme")
	require.NoError(t, err)

	var ids []domain.ID
	for i := range 3 {
		id := uuid.New()
		_, err := infra.Pool.Exec(ctx,
			`INSERT INTO instances (id, tenant_id, name, connection_intent) VALUES ($1, $2, $3, 'active')`,
			id, ctxTenant, "inst-"+string(rune('a'+i)))
		require.NoError(t, err)
		ids = append(ids, domain.ID(id))
	}

	worker := newManager(infra, "worker-a", "10.0.0.1:9090")
	for _, id := range ids {
		require.NoError(t, worker.Ensure(ctx, id))
		require.NoError(t, worker.SetDesired(ctx, id, lease.DesiredRunning))
	}

	// Suspension stops every session of the tenant...
	require.NoError(t, worker.SetTenantDesired(ctx, domain.ID(ctxTenant), lease.DesiredStopped))
	adoptable, err := worker.Adoptable(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, adoptable)

	// ...and the per-instance intent survives it, so reactivation restores
	// exactly what the user had asked for.
	var intents []string
	rows, err := infra.Pool.Query(ctx, `SELECT connection_intent FROM instances WHERE tenant_id = $1`, ctxTenant)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var intent string
		require.NoError(t, rows.Scan(&intent))
		intents = append(intents, intent)
	}
	assert.Equal(t, []string{"active", "active", "active"}, intents)
}

func TestCountLeasesByWorker(t *testing.T) {
	infra, ctx := setup(t)
	worker := newManager(infra, "worker-a", "10.0.0.1:9090")

	for range 2 {
		id := seedInstance(t, infra)
		require.NoError(t, worker.Ensure(ctx, id))
		require.NoError(t, worker.SetDesired(ctx, id, lease.DesiredRunning))
		_, err := worker.Acquire(ctx, id)
		require.NoError(t, err)
	}

	n, err := worker.Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
}

func TestUnknownInstanceIsNotAcquirable(t *testing.T) {
	infra, ctx := setup(t)
	worker := newManager(infra, "worker-a", "10.0.0.1:9090")

	_, err := worker.Acquire(ctx, domain.ID(uuid.New()))
	assert.ErrorIs(t, err, lease.ErrNotAcquired)

	_, err = worker.Owner(ctx, domain.ID(uuid.New()))
	assert.ErrorIs(t, err, lease.ErrNotAcquired)
}
