// Package lease implements exclusive session ownership.
//
// A WhatsApp session holds Signal cryptographic state, and connecting the same
// device from two processes corrupts it irrecoverably. That is the constraint
// the whole architecture derives from (Principle III), and this package is
// where it is enforced: acquisition is a single atomic UPDATE, ownership is
// renewed by heartbeat, and every command carries a generation so a process
// that lost its lease cannot act on the session anymore.
package lease

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/store"
)

// Desired states a lease can be in, as stored in the database.
const (
	DesiredRunning  = "running"
	DesiredStopped  = "stopped"
	DesiredDraining = "draining"
)

// ErrNotAcquired is returned when another worker holds a live lease.
var ErrNotAcquired = errors.New("lease: not acquired")

// ErrWrongGeneration is returned when a command carries a generation that is no
// longer the current one: the caller is talking to a former owner.
var ErrWrongGeneration = errors.New("lease: wrong generation")

// Manager owns the leases of a single worker process.
type Manager struct {
	queries *store.Queries
	// workerID identifies this process; it is what heartbeat and release match on.
	workerID string
	// grpcAddr is the address the API dials to reach this worker. It must be
	// routable and specific to this process — never a load-balanced VIP, or a
	// command could land on a process that does not own the session.
	grpcAddr string
	expiry   time.Duration
}

// Options configures a Manager.
type Options struct {
	WorkerID string
	GRPCAddr string
	// Expiry is how long a lease survives without a heartbeat. It must be
	// comfortably larger than the heartbeat interval; the configuration layer
	// enforces a factor of three.
	Expiry time.Duration
}

// New builds a Manager.
func New(queries *store.Queries, opts Options) *Manager {
	return &Manager{
		queries:  queries,
		workerID: opts.WorkerID,
		grpcAddr: opts.GRPCAddr,
		expiry:   opts.Expiry,
	}
}

// WorkerID reports this worker's identity.
func (m *Manager) WorkerID() string { return m.workerID }

// Ensure creates the lease row for an instance if it does not exist yet. The
// row starts stopped: creating it must never be enough to start a session.
func (m *Manager) Ensure(ctx context.Context, instanceID domain.ID) error {
	if err := m.queries.EnsureLease(ctx, uuid.UUID(instanceID)); err != nil {
		return fmt.Errorf("ensure lease: %w", err)
	}
	return nil
}

// SetDesired writes the effective state the worker obeys. It is derived from
// the instance intent and the tenant status, never set directly by a user.
func (m *Manager) SetDesired(ctx context.Context, instanceID domain.ID, desired string) error {
	err := m.queries.SetDesiredState(ctx, store.SetDesiredStateParams{
		InstanceID:   uuid.UUID(instanceID),
		DesiredState: desired,
	})
	if err != nil {
		return fmt.Errorf("set desired state: %w", err)
	}
	return nil
}

// SetTenantDesired projects a tenant-wide decision (suspension, reactivation)
// onto every lease of that tenant, leaving the per-instance intent untouched so
// reactivation can restore exactly what was running.
func (m *Manager) SetTenantDesired(ctx context.Context, tenantID domain.ID, desired string) error {
	err := m.queries.SetTenantDesiredState(ctx, store.SetTenantDesiredStateParams{
		TenantID:     uuid.UUID(tenantID),
		DesiredState: desired,
	})
	if err != nil {
		return fmt.Errorf("set tenant desired state: %w", err)
	}
	return nil
}

// Acquire attempts to take ownership of a session and returns the new
// generation. Concurrent callers all issue the same statement; the database
// decides, and exactly one of them gets a row back. Everyone else sees
// ErrNotAcquired — there is no second winner to reconcile later.
func (m *Manager) Acquire(ctx context.Context, instanceID domain.ID) (int64, error) {
	generation, err := m.queries.AcquireLease(ctx, store.AcquireLeaseParams{
		InstanceID: uuid.UUID(instanceID),
		WorkerID:   &m.workerID,
		GrpcAddr:   &m.grpcAddr,
		Expiry:     store.Interval(m.expiry),
	})
	if err != nil {
		if store.IsNoRows(err) {
			return 0, ErrNotAcquired
		}
		return 0, fmt.Errorf("acquire lease: %w", err)
	}
	return generation, nil
}

// Heartbeat renews every lease this worker owns in a single statement and
// returns the generations that were actually renewed.
//
// The result is not a formality: a lease missing from it was taken over or
// stopped while this process believed it was the owner. The caller must drop
// those sessions immediately, before the new owner connects them.
func (m *Manager) Heartbeat(ctx context.Context) (map[domain.ID]int64, error) {
	rows, err := m.queries.HeartbeatLeases(ctx, &m.workerID)
	if err != nil {
		return nil, fmt.Errorf("heartbeat leases: %w", err)
	}

	held := make(map[domain.ID]int64, len(rows))
	for _, row := range rows {
		held[domain.ID(row.InstanceID)] = row.Generation
	}
	return held, nil
}

// Release hands one lease back. Generation and desired state are preserved, so
// another worker adopts the session within seconds instead of waiting out the
// expiry.
func (m *Manager) Release(ctx context.Context, instanceID domain.ID) error {
	err := m.queries.ReleaseLease(ctx, store.ReleaseLeaseParams{
		InstanceID: uuid.UUID(instanceID),
		WorkerID:   &m.workerID,
	})
	if err != nil {
		return fmt.Errorf("release lease: %w", err)
	}
	return nil
}

// ReleaseAll hands back every lease of this worker. Called on SIGTERM so a
// rolling deploy costs seconds of downtime per session instead of a full expiry.
func (m *Manager) ReleaseAll(ctx context.Context) error {
	if err := m.queries.ReleaseWorkerLeases(ctx, &m.workerID); err != nil {
		return fmt.Errorf("release worker leases: %w", err)
	}
	return nil
}

// Adoptable lists sessions that should be running and have no live owner,
// skipping instances parked on a permanent failure: retrying a logged-out or
// banned number does not recover it.
func (m *Manager) Adoptable(ctx context.Context, limit int32) ([]domain.ID, error) {
	rows, err := m.queries.ListAdoptableLeases(ctx, store.ListAdoptableLeasesParams{
		Expiry:           store.Interval(m.expiry),
		PermanentReasons: domain.PermanentReasonList(),
		MaxRows:          limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list adoptable leases: %w", err)
	}

	ids := make([]domain.ID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, domain.ID(row.InstanceID))
	}
	return ids, nil
}

// Owner is the current holder of a lease, as seen by the API before dialling.
type Owner struct {
	GRPCAddr   string
	Generation int64
	// Live is false when the heartbeat is stale: the address still points at
	// the last owner, but that process is no longer holding the session.
	Live bool
}

// Owner reads who currently holds a session.
func (m *Manager) Owner(ctx context.Context, instanceID domain.ID) (Owner, error) {
	row, err := m.queries.GetLeaseOwner(ctx, store.GetLeaseOwnerParams{
		InstanceID: uuid.UUID(instanceID),
		Expiry:     store.Interval(m.expiry),
	})
	if err != nil {
		if store.IsNoRows(err) {
			return Owner{}, ErrNotAcquired
		}
		return Owner{}, fmt.Errorf("get lease owner: %w", err)
	}

	owner := Owner{Generation: row.Generation, Live: row.IsLive != nil && *row.IsLive}
	if row.GrpcAddr != nil {
		owner.GRPCAddr = *row.GrpcAddr
	}
	return owner, nil
}

// Count reports how many sessions this worker owns, used to respect the
// per-worker capacity knob.
func (m *Manager) Count(ctx context.Context) (int64, error) {
	n, err := m.queries.CountLeasesByWorker(ctx, &m.workerID)
	if err != nil {
		return 0, fmt.Errorf("count leases: %w", err)
	}
	return n, nil
}

// CheckGeneration is the fencing test every command and every emitted event
// must pass. A process that lost its lease — to a long GC pause, a network
// partition, whatever — fails here and cannot touch the session.
func (m *Manager) CheckGeneration(ctx context.Context, instanceID domain.ID, generation int64) error {
	row, err := m.queries.GetLease(ctx, uuid.UUID(instanceID))
	if err != nil {
		if store.IsNoRows(err) {
			return ErrNotAcquired
		}
		return fmt.Errorf("check generation: %w", err)
	}
	if row.Generation != generation {
		return ErrWrongGeneration
	}
	if row.WorkerID == nil || *row.WorkerID != m.workerID {
		return ErrWrongGeneration
	}
	return nil
}
