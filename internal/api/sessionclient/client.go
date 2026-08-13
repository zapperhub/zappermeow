// Package sessionclient is how the stateless API reaches the worker that owns
// a session.
//
// It never load-balances: the destination comes from the lease, because a
// command must land on the exact process holding the session. When that process
// is no longer the owner, the client rereads the lease and retries once against
// the new one — a failover is routine, and the tenant should not see it.
package sessionclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/lease"
	sessionv1 "github.com/zapperhub/zappermeow/internal/pb/sessionv1"
)

// leaseCacheTTL keeps the owner lookup off the database on the hot path without
// letting a stale address live long enough to matter: a wrong address costs one
// retry, and the retry rereads the lease anyway.
const leaseCacheTTL = 5 * time.Second

// ErrNoOwner means no worker currently holds the session. The caller decides
// what that means: for connect it is normal (a claim wakes a worker), for
// anything else it is a 503.
var ErrNoOwner = errors.New("sessionclient: no live owner")

// Client dials session workers.
type Client struct {
	leases *lease.Manager
	redis  *redis.Client
	logger *slog.Logger

	mu    sync.Mutex
	conns map[string]*grpc.ClientConn
}

// New builds a Client.
func New(leases *lease.Manager, redisClient *redis.Client, logger *slog.Logger) *Client {
	return &Client{
		leases: leases,
		redis:  redisClient,
		logger: logger,
		conns:  make(map[string]*grpc.ClientConn),
	}
}

// Close releases every pooled connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var firstErr error
	for addr, conn := range c.conns {
		if err := conn.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(c.conns, addr)
	}
	return firstErr
}

// owner is the cached view of who holds a session.
type owner struct {
	Addr       string `json:"addr"`
	Generation int64  `json:"generation"`
}

func cacheKey(instanceID domain.ID) string {
	return fmt.Sprintf("wa:lease:%s", instanceID)
}

// resolve finds the current owner, preferring the cache. A cache miss or a
// stale entry costs one database read, which is the cheap half of the tradeoff.
func (c *Client) resolve(ctx context.Context, instanceID domain.ID, useCache bool) (owner, error) {
	if useCache {
		if raw, err := c.redis.Get(ctx, cacheKey(instanceID)).Bytes(); err == nil {
			var cached owner
			if json.Unmarshal(raw, &cached) == nil && cached.Addr != "" {
				return cached, nil
			}
		}
	}

	current, err := c.leases.Owner(ctx, instanceID)
	if err != nil {
		if errors.Is(err, lease.ErrNotAcquired) {
			return owner{}, ErrNoOwner
		}
		return owner{}, fmt.Errorf("resolve owner: %w", err)
	}
	if !current.Live || current.GRPCAddr == "" {
		return owner{}, ErrNoOwner
	}

	resolved := owner{Addr: current.GRPCAddr, Generation: current.Generation}
	if raw, err := json.Marshal(resolved); err == nil {
		_ = c.redis.Set(ctx, cacheKey(instanceID), raw, leaseCacheTTL).Err()
	}
	return resolved, nil
}

// invalidate drops the cached owner so the next resolve reads the lease.
func (c *Client) invalidate(ctx context.Context, instanceID domain.ID) {
	_ = c.redis.Del(ctx, cacheKey(instanceID)).Err()
}

// conn returns a pooled connection to a worker. gRPC connections are cheap to
// keep and expensive to rebuild per request, and the address set is small: one
// entry per worker, not per instance.
func (c *Client) conn(addr string) (*grpc.ClientConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if existing, ok := c.conns[addr]; ok {
		return existing, nil
	}

	// Plaintext by design: this traffic never leaves the private network
	// (overlay on Swarm, bridge on Compose), as the constitution establishes.
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial worker %s: %w", addr, err)
	}
	c.conns[addr] = conn
	return conn, nil
}

// call runs fn against the current owner, retrying once against a freshly read
// lease when the first attempt reveals a stale owner.
//
// Exactly one retry: a second failure means ownership is changing faster than
// the API can follow, and looping would turn a failover into a stampede.
func call[T any](
	ctx context.Context,
	c *Client,
	instanceID domain.ID,
	fn func(context.Context, sessionv1.SessionServiceClient, *sessionv1.Fence) (T, error),
) (T, error) {
	var zero T

	for attempt := range 2 {
		useCache := attempt == 0

		current, err := c.resolve(ctx, instanceID, useCache)
		if err != nil {
			return zero, err
		}

		conn, err := c.conn(current.Addr)
		if err != nil {
			return zero, err
		}

		fence := &sessionv1.Fence{
			InstanceId: instanceID.String(),
			Generation: current.Generation,
		}
		result, err := fn(ctx, sessionv1.NewSessionServiceClient(conn), fence)
		if err == nil {
			return result, nil
		}

		if attempt == 0 && shouldRetry(err) {
			c.logger.Debug("retrying against a freshly read lease",
				slog.String("instance_id", instanceID.String()),
				slog.String("previous_addr", current.Addr),
				slog.String("error", err.Error()))
			c.invalidate(ctx, instanceID)
			continue
		}
		return zero, err
	}

	return zero, ErrNoOwner
}

// shouldRetry reports whether the failure looks like "you talked to the wrong
// owner" rather than "the command itself is invalid".
func shouldRetry(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch st.Code() {
	case codes.Unavailable:
		// Covers both a worker that is draining and a process that died with
		// its lease still registered.
		return true
	case codes.FailedPrecondition:
		// Only a fencing mismatch is worth retrying; NOT_PAIRED and friends are
		// real answers that a different worker would repeat.
		return st.Message() == "WRONG_GENERATION"
	default:
		return false
	}
}

// Connect brings a session up.
func (c *Client) Connect(ctx context.Context, instanceID domain.ID) (*sessionv1.ConnectResponse, error) {
	return call(ctx, c, instanceID,
		func(ctx context.Context, client sessionv1.SessionServiceClient, fence *sessionv1.Fence) (*sessionv1.ConnectResponse, error) {
			return client.Connect(ctx, &sessionv1.ConnectRequest{Fence: fence})
		})
}

// PairPhone starts a phone-code pairing attempt.
func (c *Client) PairPhone(ctx context.Context, instanceID domain.ID, phoneNumber string, replaceActive bool) (*sessionv1.PairPhoneResponse, error) {
	return call(ctx, c, instanceID,
		func(ctx context.Context, client sessionv1.SessionServiceClient, fence *sessionv1.Fence) (*sessionv1.PairPhoneResponse, error) {
			return client.PairPhone(ctx, &sessionv1.PairPhoneRequest{
				Fence:         fence,
				PhoneNumber:   phoneNumber,
				ReplaceActive: replaceActive,
			})
		})
}

// Disconnect takes a session offline, keeping its device material.
func (c *Client) Disconnect(ctx context.Context, instanceID domain.ID) (*sessionv1.DisconnectResponse, error) {
	return call(ctx, c, instanceID,
		func(ctx context.Context, client sessionv1.SessionServiceClient, fence *sessionv1.Fence) (*sessionv1.DisconnectResponse, error) {
			return client.Disconnect(ctx, &sessionv1.DisconnectRequest{Fence: fence})
		})
}

// Logout ends the session on WhatsApp and deletes the local material.
func (c *Client) Logout(ctx context.Context, instanceID domain.ID, allowTemporaryConnect bool) (*sessionv1.LogoutResponse, error) {
	return call(ctx, c, instanceID,
		func(ctx context.Context, client sessionv1.SessionServiceClient, fence *sessionv1.Fence) (*sessionv1.LogoutResponse, error) {
			return client.Logout(ctx, &sessionv1.LogoutRequest{
				Fence:                 fence,
				AllowTemporaryConnect: allowTemporaryConnect,
			})
		})
}

// Status reads the owner's live view of a session.
func (c *Client) Status(ctx context.Context, instanceID domain.ID) (*sessionv1.GetStatusResponse, error) {
	return call(ctx, c, instanceID,
		func(ctx context.Context, client sessionv1.SessionServiceClient, fence *sessionv1.Fence) (*sessionv1.GetStatusResponse, error) {
			return client.GetStatus(ctx, &sessionv1.GetStatusRequest{Fence: fence})
		})
}
