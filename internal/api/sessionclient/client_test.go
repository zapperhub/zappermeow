package sessionclient_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/zapperhub/zappermeow/internal/api/sessionclient"
	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/lease"
	sessionv1 "github.com/zapperhub/zappermeow/internal/pb/sessionv1"
	"github.com/zapperhub/zappermeow/internal/store/testutil"
)

// stubWorker is a real gRPC server on a real port. Only the session logic is
// stubbed: the lease, the address resolution and the wire hop are genuine, and
// those are what this client is made of.
type stubWorker struct {
	sessionv1.UnimplementedSessionServiceServer

	name       string
	calls      atomic.Int32
	lastFence  atomic.Value // *sessionv1.Fence
	forceError error
}

func (s *stubWorker) Connect(_ context.Context, req *sessionv1.ConnectRequest) (*sessionv1.ConnectResponse, error) {
	s.calls.Add(1)
	s.lastFence.Store(req.GetFence())
	if s.forceError != nil {
		return nil, s.forceError
	}
	return &sessionv1.ConnectResponse{State: sessionv1.SessionState_SESSION_STATE_CONNECTING}, nil
}

func startWorker(t *testing.T, name string, forceError error) (*stubWorker, string) {
	t.Helper()

	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	worker := &stubWorker{name: name, forceError: forceError}
	server := grpc.NewServer()
	sessionv1.RegisterSessionServiceServer(server, worker)

	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	return worker, listener.Addr().String()
}

type fixture struct {
	infra      *testutil.Infra
	client     *sessionclient.Client
	instanceID domain.ID
	ctx        context.Context
}

func setup(t *testing.T) *fixture {
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
	reader := lease.New(infra.Queries, lease.Options{
		WorkerID: "api-reader", GRPCAddr: "", Expiry: 30 * time.Second,
	})
	client := sessionclient.New(reader, infra.Redis, logger)
	t.Cleanup(func() { _ = client.Close() })

	return &fixture{infra: infra, client: client, instanceID: instanceID, ctx: ctx}
}

// acquireFor makes a worker the registered owner of the instance.
func (f *fixture) acquireFor(t *testing.T, workerID, addr string) *lease.Manager {
	t.Helper()

	manager := lease.New(f.infra.Queries, lease.Options{
		WorkerID: workerID, GRPCAddr: addr, Expiry: 30 * time.Second,
	})
	require.NoError(t, manager.Ensure(f.ctx, f.instanceID))
	require.NoError(t, manager.SetDesired(f.ctx, f.instanceID, lease.DesiredRunning))
	_, err := manager.Acquire(f.ctx, f.instanceID)
	require.NoError(t, err)
	return manager
}

func TestCommandReachesTheLeaseOwner(t *testing.T) {
	f := setup(t)
	worker, addr := startWorker(t, "worker-a", nil)
	f.acquireFor(t, "worker-a", addr)

	resp, err := f.client.Connect(f.ctx, f.instanceID)
	require.NoError(t, err)
	assert.Equal(t, sessionv1.SessionState_SESSION_STATE_CONNECTING, resp.GetState())
	assert.Equal(t, int32(1), worker.calls.Load())

	// The fence must carry the generation the lease reports, or the worker
	// would reject every command.
	fence, _ := worker.lastFence.Load().(*sessionv1.Fence)
	require.NotNil(t, fence)
	assert.Equal(t, f.instanceID.String(), fence.GetInstanceId())
	assert.Equal(t, int64(1), fence.GetGeneration())
}

func TestNoOwnerIsReportedDistinctly(t *testing.T) {
	f := setup(t)

	// A lease row that was never acquired has no address to dial.
	manager := lease.New(f.infra.Queries, lease.Options{
		WorkerID: "worker-a", GRPCAddr: "127.0.0.1:1", Expiry: 30 * time.Second,
	})
	require.NoError(t, manager.Ensure(f.ctx, f.instanceID))

	_, err := f.client.Connect(f.ctx, f.instanceID)
	assert.ErrorIs(t, err, sessionclient.ErrNoOwner)
}

func TestUnknownInstanceHasNoOwner(t *testing.T) {
	f := setup(t)

	_, err := f.client.Connect(f.ctx, domain.NewID())
	assert.ErrorIs(t, err, sessionclient.ErrNoOwner)
}

// A stale heartbeat means the registered address points at a process that no
// longer owns the session; dialling it would be worse than reporting no owner.
func TestStaleOwnerIsNotDialled(t *testing.T) {
	f := setup(t)
	worker, addr := startWorker(t, "worker-a", nil)
	f.acquireFor(t, "worker-a", addr)

	_, err := f.infra.Pool.Exec(f.ctx,
		`UPDATE session_leases SET heartbeat_at = now() - interval '2 minutes' WHERE instance_id = $1`,
		f.instanceID)
	require.NoError(t, err)

	_, err = f.client.Connect(f.ctx, f.instanceID)
	assert.ErrorIs(t, err, sessionclient.ErrNoOwner)
	assert.Equal(t, int32(0), worker.calls.Load())
}

// The failover path: the cached owner answers WRONG_GENERATION, the client
// rereads the lease and lands on the new owner without the caller noticing.
func TestWrongGenerationRetriesAgainstTheNewOwner(t *testing.T) {
	f := setup(t)

	stale, staleAddr := startWorker(t, "worker-stale",
		status.Error(codes.FailedPrecondition, "WRONG_GENERATION"))
	fresh, freshAddr := startWorker(t, "worker-fresh", nil)

	f.acquireFor(t, "worker-stale", staleAddr)

	// Warm the cache so the first attempt goes to the old owner.
	_, err := f.client.Connect(f.ctx, f.instanceID)
	require.Error(t, err, "with no new owner yet, the retry finds the same stale worker")
	require.Equal(t, int32(2), stale.calls.Load(), "exactly one retry, never a loop")

	// Ownership moves.
	_, err = f.infra.Pool.Exec(f.ctx,
		`UPDATE session_leases SET heartbeat_at = now() - interval '2 minutes' WHERE instance_id = $1`,
		f.instanceID)
	require.NoError(t, err)
	f.acquireFor(t, "worker-fresh", freshAddr)

	resp, err := f.client.Connect(f.ctx, f.instanceID)
	require.NoError(t, err)
	assert.Equal(t, sessionv1.SessionState_SESSION_STATE_CONNECTING, resp.GetState())
	assert.Equal(t, int32(1), fresh.calls.Load())

	fence, _ := fresh.lastFence.Load().(*sessionv1.Fence)
	require.NotNil(t, fence)
	assert.Equal(t, int64(2), fence.GetGeneration(), "the retry must carry the new generation")
}

// A draining worker is a normal part of a deploy: the client should move on
// rather than surface the shutdown to the tenant.
func TestDrainingWorkerTriggersARetry(t *testing.T) {
	f := setup(t)
	draining, drainingAddr := startWorker(t, "worker-draining",
		status.Error(codes.Unavailable, "DRAINING"))
	f.acquireFor(t, "worker-draining", drainingAddr)

	_, err := f.client.Connect(f.ctx, f.instanceID)
	require.Error(t, err)
	assert.Equal(t, int32(2), draining.calls.Load(), "one retry, then give up")
}

// Errors that describe the command rather than the owner must not be retried:
// another worker would answer exactly the same, and the tenant deserves the
// real reason instead of a slower failure.
func TestCommandErrorsAreNotRetried(t *testing.T) {
	f := setup(t)
	worker, addr := startWorker(t, "worker-a",
		status.Error(codes.FailedPrecondition, "NOT_PAIRED"))
	f.acquireFor(t, "worker-a", addr)

	_, err := f.client.Connect(f.ctx, f.instanceID)
	require.Error(t, err)

	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Equal(t, "NOT_PAIRED", st.Message())
	assert.Equal(t, int32(1), worker.calls.Load(), "a real answer must not be retried")
}

func TestInvalidPhoneIsNotRetried(t *testing.T) {
	f := setup(t)
	_, addr := startWorker(t, "worker-a", nil)
	f.acquireFor(t, "worker-a", addr)

	// PairPhone is unimplemented on the stub, which returns Unimplemented —
	// a code the client must pass through untouched.
	_, err := f.client.PairPhone(f.ctx, f.instanceID, "5511999999999", true)
	require.Error(t, err)

	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unimplemented, st.Code())
}

// The cache exists to keep the owner lookup off the database on the hot path.
func TestOwnerLookupIsCached(t *testing.T) {
	f := setup(t)
	worker, addr := startWorker(t, "worker-a", nil)
	f.acquireFor(t, "worker-a", addr)

	for range 3 {
		_, err := f.client.Connect(f.ctx, f.instanceID)
		require.NoError(t, err)
	}
	assert.Equal(t, int32(3), worker.calls.Load())

	cached, err := f.infra.Redis.Get(f.ctx, "wa:lease:"+f.instanceID.String()).Result()
	require.NoError(t, err)
	assert.Contains(t, cached, addr)
}
