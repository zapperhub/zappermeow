// Package ws serves the per-instance event channel.
//
// It is mounted as a plain chi handler rather than through huma: a WebSocket
// upgrade is not a JSON response and has no place in the OpenAPI response
// schemas. The consequence is that nothing here is inherited — authentication,
// rate limiting and structured logging are applied explicitly.
package ws

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/events"
)

// Subprotocol is required on every connection. Versioning the channel here lets
// a future frame format ship without breaking existing clients.
const Subprotocol = "zappermeow.v1"

// bearerPrefix is how a browser smuggles a credential: the WebSocket API cannot
// set headers, and a token in the query string would land in access logs, in
// the proxy and in browser history — which the constitution forbids.
const bearerPrefix = "bearer."

// Close codes beyond the standard ones, as published in the contract.
const (
	CloseRevoked         websocket.StatusCode = 4403
	CloseInstanceDeleted websocket.StatusCode = 4404
	CloseSlowConsumer    websocket.StatusCode = 4429
)

const (
	pingInterval = 30 * time.Second
	pongTimeout  = 10 * time.Second
	writeTimeout = 5 * time.Second
	// revalidateInterval bounds how long a revoked credential keeps receiving
	// events: authorisation is not only checked at the door (FR-042).
	revalidateInterval = 30 * time.Second
)

// Principal is who opened the connection, resolved from whichever credential
// was presented.
type Principal struct {
	TenantID domain.ID
	// RateLimitKey is the bucket this connection draws from, so a handshake
	// costs the same allowance as any other operational call.
	RateLimitKey string
}

// Authenticator resolves a credential to an instance the caller may watch.
// Returning an error means the handshake is refused before any frame is sent.
type Authenticator interface {
	Authorize(ctx context.Context, credential string, instanceID domain.ID) (Principal, error)
}

// Limiter enforces the shared allowance. The upgrade is a chi handler, outside
// huma's middleware chain, so it has to ask rather than inherit.
type Limiter interface {
	AllowKey(ctx context.Context, key string) bool
}

// Snapshotter builds the first frame: the current state of the instance,
// including any pairing code in flight.
type Snapshotter interface {
	Snapshot(ctx context.Context, instanceID domain.ID) (events.Envelope, error)
}

// Handler serves GET /instances/{instanceId}/ws.
type Handler struct {
	auth       Authenticator
	limiter    Limiter
	subscriber *events.Subscriber
	snapshots  Snapshotter
	logger     *slog.Logger

	// RevalidateInterval is how often an open connection rechecks its
	// credential. Exposed so tests can prove the recheck happens instead of
	// asserting that the code merely exists.
	RevalidateInterval time.Duration
}

// NewHandler builds the handler.
func NewHandler(
	auth Authenticator,
	limiter Limiter,
	subscriber *events.Subscriber,
	snapshots Snapshotter,
	logger *slog.Logger,
) *Handler {
	return &Handler{
		auth:               auth,
		limiter:            limiter,
		subscriber:         subscriber,
		snapshots:          snapshots,
		logger:             logger,
		RevalidateInterval: revalidateInterval,
	}
}

// ServeHTTP authenticates, upgrades and then streams.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, instanceID domain.ID) {
	start := time.Now()
	logger := h.logger.With(slog.String("instance_id", instanceID.String()))

	credential, err := credentialFrom(r)
	if err != nil {
		// Refusals happen before the upgrade, as ordinary HTTP: a client that
		// gets a 401 sees a status code, not a socket that closes immediately.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		logger.Warn("websocket handshake refused", slog.String("reason", "missing credential"))
		return
	}

	principal, err := h.auth.Authorize(r.Context(), credential, instanceID)
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, ErrForbidden) {
			status = http.StatusForbidden
		}
		if errors.Is(err, ErrNotFound) {
			status = http.StatusNotFound
		}
		http.Error(w, http.StatusText(status), status)
		logger.Warn("websocket handshake refused", slog.String("reason", err.Error()))
		return
	}

	// Structured logging is applied here rather than inherited: this handler
	// never passes through the API's middleware chain (principle VI).
	logger = logger.With(slog.String("tenant_id", principal.TenantID.String()))

	if h.limiter != nil && !h.limiter.AllowKey(r.Context(), principal.RateLimitKey) {
		// Refused before the upgrade: opening a socket only to close it would
		// cost more than the request it is meant to shed.
		http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
		logger.Warn("websocket handshake refused", slog.String("reason", "rate limited"))
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{Subprotocol},
	})
	if err != nil {
		logger.Warn("websocket upgrade failed", slog.String("error", err.Error()))
		return
	}
	if conn.Subprotocol() != Subprotocol {
		_ = conn.Close(websocket.StatusPolicyViolation, "subprotocol "+Subprotocol+" is required")
		return
	}

	logger.Info("websocket opened")
	defer func() {
		logger.Info("websocket closed", slog.Duration("duration", time.Since(start)))
	}()

	h.stream(r.Context(), conn, instanceID, credential, logger)
}

// stream runs the connection until it ends.
func (h *Handler) stream(
	ctx context.Context,
	conn *websocket.Conn,
	instanceID domain.ID,
	credential string,
	logger *slog.Logger,
) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Subscribe BEFORE reading the snapshot. The other order leaves a window in
	// which an event published between the two steps is lost forever; this way
	// it is merely duplicated, and the sequence number resolves that.
	stream, err := h.subscriber.Subscribe(ctx, instanceID)
	if err != nil {
		logger.Error("subscribing failed", slog.String("error", err.Error()))
		_ = conn.Close(websocket.StatusInternalError, "subscribe failed")
		return
	}
	defer func() { _ = stream.Close() }()

	snapshot, err := h.snapshots.Snapshot(ctx, instanceID)
	if err != nil {
		logger.Error("building snapshot failed", slog.String("error", err.Error()))
		_ = conn.Close(websocket.StatusInternalError, "snapshot failed")
		return
	}
	if err := write(ctx, conn, snapshot); err != nil {
		return
	}

	// Anything already reflected in the snapshot is dropped rather than sent
	// twice — that is the whole point of carrying a sequence.
	lastSeq := snapshot.Seq

	go h.readLoop(ctx, conn, cancel)

	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	revalidate := time.NewTicker(h.RevalidateInterval)
	defer revalidate.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return

		case <-stream.Overflowed():
			logger.Warn("closing slow websocket consumer")
			_ = conn.Close(CloseSlowConsumer, "consumer too slow")
			return

		case envelope, ok := <-stream.Events():
			if !ok {
				_ = conn.Close(websocket.StatusNormalClosure, "")
				return
			}
			if envelope.Seq <= lastSeq {
				continue
			}
			lastSeq = envelope.Seq
			if err := write(ctx, conn, envelope); err != nil {
				return
			}

		case <-ping.C:
			pingCtx, cancelPing := context.WithTimeout(ctx, pongTimeout)
			err := conn.Ping(pingCtx)
			cancelPing()
			if err != nil {
				_ = conn.Close(websocket.StatusPolicyViolation, "ping timeout")
				return
			}

		case <-revalidate.C:
			if _, err := h.auth.Authorize(ctx, credential, instanceID); err != nil {
				logger.Info("closing websocket: credential no longer valid")
				_ = conn.Close(CloseRevoked, "credential revoked")
				return
			}
		}
	}
}

// readLoop exists to notice the client going away. This channel is push-only,
// so anything received is discarded — but without reading, a closed connection
// would go unnoticed until the next ping.
func (h *Handler) readLoop(ctx context.Context, conn *websocket.Conn, cancel context.CancelFunc) {
	defer cancel()
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
	}
}

func write(ctx context.Context, conn *websocket.Conn, envelope events.Envelope) error {
	payload, err := envelope.Marshal()
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "encode failed")
		return err
	}

	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, payload)
}

// credentialFrom extracts the credential without ever looking at the query
// string: a token there would be logged by every proxy in the path.
func credentialFrom(r *http.Request) (string, error) {
	if header := r.Header.Get("Authorization"); header != "" {
		if token, ok := strings.CutPrefix(header, "Bearer "); ok && token != "" {
			return token, nil
		}
	}
	if key := r.Header.Get("X-Api-Key"); key != "" {
		return key, nil
	}

	// Browser path: the credential rides in the subprotocol list.
	for _, proto := range websocketProtocols(r) {
		if token, ok := strings.CutPrefix(proto, bearerPrefix); ok && token != "" {
			return token, nil
		}
	}
	return "", ErrNoCredential
}

func websocketProtocols(r *http.Request) []string {
	raw := r.Header.Values("Sec-WebSocket-Protocol")
	var protocols []string
	for _, value := range raw {
		for _, item := range strings.Split(value, ",") {
			if trimmed := strings.TrimSpace(item); trimmed != "" {
				protocols = append(protocols, trimmed)
			}
		}
	}
	return protocols
}

// Errors an Authenticator may return to shape the HTTP status.
var (
	ErrNoCredential = errors.New("ws: no credential presented")
	ErrForbidden    = errors.New("ws: forbidden")
	ErrNotFound     = errors.New("ws: instance not found")
)
