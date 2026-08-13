package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/zapperhub/zappermeow/internal/store"
)

// retentionLockID is the advisory lock every worker contends for before
// sweeping. Postgres advisory locks are keyed by a plain integer, so this
// constant is effectively the sweep's name — it must not collide with any other
// advisory lock the project introduces later.
const retentionLockID int64 = 0x7A4D_0002

// Retention deletes connection events past their retention window.
//
// It runs in the session worker rather than as a queued job because standing up
// the whole jobs service for one daily DELETE would be disproportionate
// (research R11). The advisory lock is what keeps several workers from all
// running it: whoever takes the lock sweeps, the rest skip without blocking.
type Retention struct {
	queries *store.Queries
	logger  *slog.Logger
	window  time.Duration
	// interval is how often a sweep is attempted; missing one is harmless,
	// which is why this is a plain ticker and not a scheduler.
	interval time.Duration
}

// NewRetention builds the sweeper.
func NewRetention(queries *store.Queries, window, interval time.Duration, logger *slog.Logger) *Retention {
	return &Retention{queries: queries, logger: logger, window: window, interval: interval}
}

// Run sweeps periodically until the context is cancelled.
func (r *Retention) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.SweepOnce(ctx)
		}
	}
}

// SweepOnce attempts a single sweep and reports how many rows it removed. A
// worker that loses the lock removes nothing and says so with zero.
func (r *Retention) SweepOnce(ctx context.Context) int64 {
	acquired, err := r.queries.TryAdvisoryLock(ctx, retentionLockID)
	if err != nil {
		r.logger.Error("retention lock failed", slog.String("error", err.Error()))
		return 0
	}
	if !acquired {
		// Another worker is sweeping. Skipping is the point of the lock.
		return 0
	}
	defer func() {
		if err := r.queries.AdvisoryUnlock(ctx, retentionLockID); err != nil {
			r.logger.Error("retention unlock failed", slog.String("error", err.Error()))
		}
	}()

	cutoff := time.Now().Add(-r.window)
	removed, err := r.queries.DeleteConnectionEventsBefore(ctx, cutoff)
	if err != nil {
		r.logger.Error("retention sweep failed", slog.String("error", err.Error()))
		return 0
	}

	if removed > 0 {
		r.logger.Info("connection events pruned",
			slog.Int64("removed", removed),
			slog.Time("cutoff", cutoff))
	}
	return removed
}
