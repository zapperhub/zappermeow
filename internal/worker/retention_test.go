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

	"github.com/zapperhub/zappermeow/internal/worker"
)

func TestRetentionRemovesOnlyExpiredEvents(t *testing.T) {
	h := newHarness(t)

	// One inside the window, one well outside it.
	for _, age := range []string{"1 hour", "60 days"} {
		_, err := h.infra.Pool.Exec(h.ctx,
			`INSERT INTO connection_events (instance_id, type, occurred_at)
			 VALUES ($1, 'connected', now() - $2::interval)`,
			uuid.UUID(h.instanceID), age)
		require.NoError(t, err)
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	retention := worker.NewRetention(h.infra.Queries, 30*24*time.Hour, time.Hour, logger)

	removed := retention.SweepOnce(h.ctx)
	assert.Equal(t, int64(1), removed)

	remaining := h.trail(t)
	assert.Len(t, remaining, 1, "an entry inside the window must survive the sweep")
}

// The advisory lock is what keeps a fleet from running the same DELETE several
// times over: whoever takes it sweeps, the rest skip without blocking.
func TestRetentionRunsOnceAcrossWorkers(t *testing.T) {
	h := newHarness(t)

	for range 3 {
		_, err := h.infra.Pool.Exec(h.ctx,
			`INSERT INTO connection_events (instance_id, type, occurred_at)
			 VALUES ($1, 'connected', now() - interval '60 days')`,
			uuid.UUID(h.instanceID))
		require.NoError(t, err)
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	// Two sweepers on separate connections, as two worker processes would be.
	first := worker.NewRetention(h.infra.Queries, 30*24*time.Hour, time.Hour, logger)
	second := worker.NewRetention(h.infra.Queries, 30*24*time.Hour, time.Hour, logger)

	firstRemoved := first.SweepOnce(h.ctx)
	secondRemoved := second.SweepOnce(h.ctx)

	// The lock is released between the two calls here, so the second sweeper
	// does acquire it — and finds nothing left, which is the same end state a
	// skipped sweep produces: the rows are deleted exactly once.
	assert.Equal(t, int64(3), firstRemoved)
	assert.Equal(t, int64(0), secondRemoved)
	assert.Empty(t, h.trail(t))
}

// A sweeper that cannot take the lock must return immediately rather than queue
// behind the one holding it.
func TestRetentionSkipsWhenTheLockIsHeld(t *testing.T) {
	h := newHarness(t)

	_, err := h.infra.Pool.Exec(h.ctx,
		`INSERT INTO connection_events (instance_id, type, occurred_at)
		 VALUES ($1, 'connected', now() - interval '60 days')`,
		uuid.UUID(h.instanceID))
	require.NoError(t, err)

	// Hold the same advisory lock on a dedicated connection, standing in for a
	// worker mid-sweep.
	conn, err := h.infra.Pool.Acquire(h.ctx)
	require.NoError(t, err)
	defer conn.Release()

	var acquired bool
	require.NoError(t, conn.QueryRow(h.ctx, `SELECT pg_try_advisory_lock($1)`, int64(0x7A4D_0002)).Scan(&acquired))
	require.True(t, acquired)
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, int64(0x7A4D_0002))
	}()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	retention := worker.NewRetention(h.infra.Queries, 30*24*time.Hour, time.Hour, logger)

	done := make(chan int64, 1)
	go func() { done <- retention.SweepOnce(h.ctx) }()

	select {
	case removed := <-done:
		assert.Equal(t, int64(0), removed, "a sweeper without the lock deletes nothing")
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep blocked on the lock instead of skipping")
	}

	assert.Len(t, h.trail(t), 1, "the expired row waits for whoever holds the lock")
}
