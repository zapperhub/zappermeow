// Package store holds the data-access layer: the pgx pool, the Redis client,
// the migration runner and the sqlc-generated, type-safe query code.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// NewPool opens the single pgx pool shared by the whole service and verifies
// connectivity before returning.
func NewPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// RedisOptions carries what NewRedis needs without importing the config package.
type RedisOptions struct {
	Addr     string
	Password string
	DB       int
}

// NewRedis opens the Redis client used for GCRA rate limiting and verifies
// connectivity. Redis holds no account data — only ephemeral counters.
func NewRedis(ctx context.Context, opts RedisOptions) (*redis.Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     opts.Addr,
		Password: opts.Password,
		DB:       opts.DB,
	})

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return client, nil
}

// IsNoRows reports whether err is pgx's "no rows" sentinel. Callers use it to
// tell "nothing matched" apart from a real database failure — for the lease,
// that is the difference between losing a race and being unable to run at all.
func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// Interval converts a Go duration into the pgtype value sqlc expects for SQL
// interval parameters.
func Interval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}
