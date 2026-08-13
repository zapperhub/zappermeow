package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/events"
	"github.com/zapperhub/zappermeow/internal/lease"
)

// Runtime drives the periodic work of a worker: renewing the leases it holds,
// adopting sessions nobody owns, and reacting to the coordination channels.
type Runtime struct {
	supervisor *Supervisor
	leases     *lease.Manager
	subscriber *events.Subscriber
	logger     *slog.Logger

	heartbeatInterval time.Duration
	reconcileInterval time.Duration
}

// RuntimeOptions configures a Runtime.
type RuntimeOptions struct {
	Supervisor        *Supervisor
	Leases            *lease.Manager
	Subscriber        *events.Subscriber
	Logger            *slog.Logger
	HeartbeatInterval time.Duration
	ReconcileInterval time.Duration
}

// NewRuntime builds a Runtime.
func NewRuntime(opts RuntimeOptions) *Runtime {
	return &Runtime{
		supervisor:        opts.Supervisor,
		leases:            opts.Leases,
		subscriber:        opts.Subscriber,
		logger:            opts.Logger,
		heartbeatInterval: opts.HeartbeatInterval,
		reconcileInterval: opts.ReconcileInterval,
	}
}

// Run blocks until the context is cancelled, driving every periodic loop.
func (r *Runtime) Run(ctx context.Context) error {
	claims, closeClaims, err := r.subscriber.SubscribeControl(ctx, events.ClaimChannel)
	if err != nil {
		return err
	}
	defer func() { _ = closeClaims() }()

	stops, closeStops, err := r.subscriber.SubscribeControl(ctx, events.StopChannel)
	if err != nil {
		return err
	}
	defer func() { _ = closeStops() }()

	heartbeat := time.NewTicker(r.heartbeatInterval)
	defer heartbeat.Stop()
	reconcile := time.NewTicker(r.reconcileInterval)
	defer reconcile.Stop()

	// Reconcile once at boot so a restarted worker picks up orphaned sessions
	// immediately instead of waiting out the first tick.
	r.reconcileOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-heartbeat.C:
			r.heartbeatOnce(ctx)

		case <-reconcile.C:
			r.reconcileOnce(ctx)

		case instanceID := <-claims:
			// A claim is what keeps the first QR inside its five-second budget:
			// waiting for the reconciliation tick would blow it every time.
			r.claim(ctx, instanceID)

		case instanceID := <-stops:
			r.stop(ctx, instanceID)
		}
	}
}

// heartbeatOnce renews every lease this worker owns and drops whatever it no
// longer holds.
func (r *Runtime) heartbeatOnce(ctx context.Context) {
	held, err := r.leases.Heartbeat(ctx)
	if err != nil {
		r.logger.Error("heartbeat failed", slog.String("error", err.Error()))
		return
	}

	// Anything running here but missing from the renewal was taken over or
	// stopped. The new owner may already be connecting, so letting go now is
	// the difference between a clean handover and two live sessions.
	for _, instanceID := range r.supervisor.Held() {
		if _, still := held[instanceID]; !still {
			r.supervisor.Drop(ctx, instanceID)
		}
	}
}

// reconcileOnce adopts sessions that should be running and have no live owner.
func (r *Runtime) reconcileOnce(ctx context.Context) {
	if !r.supervisor.HasCapacity() {
		return
	}

	free := int32(r.supervisor.Capacity())
	candidates, err := r.leases.Adoptable(ctx, free)
	if err != nil {
		r.logger.Error("listing adoptable leases failed", slog.String("error", err.Error()))
		return
	}

	for _, instanceID := range candidates {
		if !r.supervisor.HasCapacity() {
			return
		}
		r.adopt(ctx, instanceID)
	}
}

func (r *Runtime) claim(ctx context.Context, instanceID domain.ID) {
	if !r.supervisor.HasCapacity() {
		// Another worker with room will take it; the tick is the safety net if
		// none has room right now.
		return
	}
	r.adopt(ctx, instanceID)
}

func (r *Runtime) adopt(ctx context.Context, instanceID domain.ID) {
	switch err := r.supervisor.Adopt(ctx, instanceID); {
	case err == nil:
		r.logger.Info("session adopted", slog.String("instance_id", instanceID.String()))
	case isLost(err):
		// Losing the race is the expected outcome when several workers wake on
		// the same claim, not a failure worth reporting.
	default:
		r.logger.Error("adopting session failed",
			slog.String("instance_id", instanceID.String()),
			slog.String("error", err.Error()))
	}
}

// stop drops a session this worker owns, on request. It is how suspending a
// tenant takes effect in seconds rather than at the next reconciliation.
func (r *Runtime) stop(ctx context.Context, instanceID domain.ID) {
	if !r.supervisor.Owns(instanceID) {
		return
	}
	r.logger.Info("stopping session on request",
		slog.String("instance_id", instanceID.String()))
	r.supervisor.StopOnRequest(ctx, instanceID, domain.ReasonTenantSuspended)
}

func isLost(err error) bool {
	return err == lease.ErrNotAcquired || err == ErrDraining
}
