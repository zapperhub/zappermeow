// Package testutil starts the real Postgres and Redis instances the test suite
// runs against. The constitution forbids mocking infrastructure: SQL, locking
// and rate limiting are exactly where mocks hide bugs.
package testutil

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/zapperhub/zappermeow/internal/store"
)

// requireDockerEnv makes a missing Docker daemon a failure instead of a skip.
// CI sets it so the integration suite can never silently disappear.
const requireDockerEnv = "ZAPPERMEOW_REQUIRE_DOCKER"

// Infra holds the connections to the containers shared by a test binary.
type Infra struct {
	Pool        *pgxpool.Pool
	Redis       *redis.Client
	Queries     *store.Queries
	DatabaseURL string
	RedisAddr   string
}

var (
	sharedOnce sync.Once
	shared     *Infra
	sharedErr  error
)

// Shared starts Postgres 17 and Redis once per test binary and applies the
// migrations. Tests call Reset to get a clean slate.
func Shared(t *testing.T) *Infra {
	t.Helper()

	sharedOnce.Do(func() { shared, sharedErr = start() })

	if sharedErr != nil {
		if os.Getenv(requireDockerEnv) != "" {
			t.Fatalf("integration infrastructure unavailable and %s is set: %v", requireDockerEnv, sharedErr)
		}
		t.Skipf("skipping integration test: %v (set %s to make this a failure)", sharedErr, requireDockerEnv)
	}
	return shared
}

func start() (*Infra, error) {
	ctx := context.Background()

	pgContainer, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("zappermeow"),
		tcpostgres.WithUsername("zappermeow"),
		tcpostgres.WithPassword("zappermeow"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("start postgres: %w", err)
	}

	databaseURL, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, fmt.Errorf("postgres connection string: %w", err)
	}

	redisContainer, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		return nil, fmt.Errorf("start redis: %w", err)
	}
	redisURL, err := redisContainer.ConnectionString(ctx)
	if err != nil {
		return nil, fmt.Errorf("redis connection string: %w", err)
	}
	redisOpts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}

	if _, err := store.Migrate(databaseURL); err != nil {
		return nil, fmt.Errorf("apply migrations: %w", err)
	}

	pool, err := store.NewPool(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	redisClient, err := store.NewRedis(ctx, store.RedisOptions{Addr: redisOpts.Addr, DB: redisOpts.DB})
	if err != nil {
		return nil, fmt.Errorf("open redis: %w", err)
	}

	return &Infra{
		Pool:        pool,
		Redis:       redisClient,
		Queries:     store.New(pool),
		DatabaseURL: databaseURL,
		RedisAddr:   redisOpts.Addr,
	}, nil
}

// Reset truncates every table and flushes Redis so each test starts clean.
// Truncating tenants cascades to users, instances and keys, which also keeps
// the cascade wiring itself under test.
func (i *Infra) Reset(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	if _, err := i.Pool.Exec(ctx, `TRUNCATE tenants, users, instances, api_keys, security_events CASCADE`); err != nil {
		t.Fatalf("reset database: %v", err)
	}
	if err := i.Redis.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("reset redis: %v", err)
	}
}
