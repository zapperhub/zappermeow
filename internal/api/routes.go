package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/zapperhub/zappermeow/internal/api/handlers"
	"github.com/zapperhub/zappermeow/internal/api/middleware"
	"github.com/zapperhub/zappermeow/internal/api/sessionclient"
	"github.com/zapperhub/zappermeow/internal/api/ws"
	"github.com/zapperhub/zappermeow/internal/config"
	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/domain/services"
	"github.com/zapperhub/zappermeow/internal/events"
	"github.com/zapperhub/zappermeow/internal/lease"
	"github.com/zapperhub/zappermeow/internal/store"
)

// RouteDeps carries everything the route groups need. Handlers receive their
// service through it; none of them reach for a global.
type RouteDeps struct {
	Config   *config.Config
	Logger   *slog.Logger
	Pool     *pgxpool.Pool
	Redis    *redis.Client
	Queries  *store.Queries
	Recorder *services.EventRecorder
	Issuer   *domain.TokenIssuer

	// Session plane. Nil in deployments that run the API without a worker
	// fleet — the connection routes simply do not exist there, which is
	// honester than mounting endpoints that can only fail.
	Leases    *lease.Manager
	Sessions  *sessionclient.Client
	Publisher *events.Publisher
}

// RegisterRoutes mounts every operation of the feature, grouped by the
// credential each plane requires. Grouping the middleware rather than checking
// inside handlers is what makes "no route without authentication" structural.
func (s *Server) RegisterRoutes(deps RouteDeps) {
	cfg := deps.Config

	auth := services.NewAuthService(
		deps.Pool, deps.Queries, deps.Recorder, deps.Issuer,
		cfg.LockoutMaxFailures, cfg.LockoutWindow,
	)
	tenants := services.NewTenantService(deps.Pool, deps.Queries, deps.Recorder)
	instances := services.NewInstanceService(deps.Pool, deps.Queries, deps.Recorder)
	apiKeys := services.NewAPIKeyService(deps.Pool, deps.Queries, deps.Recorder)
	passwords := services.NewPasswordService(deps.Pool, deps.Queries, deps.Recorder)

	authenticator := middleware.NewAuthenticator(s.api, deps.Queries, deps.Issuer)
	loginLimiter := middleware.NewLoginRateLimiter(s.api, deps.Redis, cfg.LoginRateLimit, deps.Logger)
	keyAuthenticator := middleware.NewAPIKeyAuthenticator(s.api, deps.Queries)
	operationalLimiter := middleware.NewOperationalRateLimiter(s.api, deps.Redis, cfg.OperationalRateLimit, deps.Logger)

	// Public plane: login only, guarded by the per-origin limiter.
	publicGroup := huma.NewGroup(s.api)
	publicGroup.UseMiddleware(loginLimiter.Limit())
	handlers.NewAuthHandler(auth).Register(publicGroup)

	// Platform plane: the super-admin manages tenants.
	platformGroup := huma.NewGroup(s.api)
	platformGroup.UseMiddleware(authenticator.Platform())
	tenantHandler := handlers.NewTenantHandler(tenants)
	tenantHandler.Register(platformGroup)
	tenantHandler.RegisterLifecycle(platformGroup)
	passwordHandler := handlers.NewPasswordHandler(passwords)
	passwordHandler.RegisterReset(platformGroup)

	// Authenticated on either plane, and the only group that tolerates a pending
	// temporary password — changing it is exactly what such a session is for.
	anyAudienceGroup := huma.NewGroup(s.api)
	anyAudienceGroup.UseMiddleware(authenticator.AnyAudience())
	passwordHandler.RegisterChange(anyAudienceGroup)

	// Tenant plane: a tenant admin manages its own instances and keys.
	tenantGroup := huma.NewGroup(s.api)
	tenantGroup.UseMiddleware(authenticator.Tenant())
	handlers.NewInstanceHandler(instances).Register(tenantGroup)
	handlers.NewAPIKeyHandler(apiKeys).Register(tenantGroup)
	apiKeys.RefreshActiveKeyGauge(context.Background())

	// Operational plane: authenticated by the instance's own API key and rate
	// limited per key, so one instance cannot consume another's allowance.
	operationalGroup := huma.NewGroup(s.api)
	operationalGroup.UseMiddleware(keyAuthenticator.Authenticate())
	operationalGroup.UseMiddleware(operationalLimiter.Limit())
	handlers.NewWhoamiHandler().Register(operationalGroup)

	// Connection plane: these routes drive a session and accept either
	// credential, so they get their own group — and their own limiter, keyed by
	// whichever credential was presented. They are operational routes, and the
	// constitution requires every one of those to be rate limited.
	if deps.Sessions != nil {
		connectionAuth := middleware.NewConnectionAuthenticator(s.api, deps.Queries, authenticator, keyAuthenticator)
		connectionLimiter := middleware.NewConnectionRateLimiter(s.api, deps.Redis, cfg.OperationalRateLimit, deps.Logger)

		connectionGroup := huma.NewGroup(s.api)
		connectionGroup.UseMiddleware(connectionAuth.Authenticate())
		connectionGroup.UseMiddleware(connectionLimiter.Limit())

		connections := services.NewConnectionService(deps.Queries, deps.Leases, deps.Sessions, deps.Publisher, deps.Logger, cfg.ClaimWait)
		connectionHandler := handlers.NewConnectionHandler(connections)
		connectionHandler.Register(connectionGroup)
		connectionHandler.RegisterQueries(connectionGroup)

		// Deleting an instance must not leave a live session behind.
		instances.WithSessions(connections)

		// The event channel is mounted straight on chi: a WebSocket upgrade is
		// not a JSON response and has no place among the OpenAPI response
		// schemas. Everything the huma chain would have given it — credential
		// check, allowance, structured logging — is applied inside the handler.
		wsHandler := ws.NewHandler(
			&wsAuthenticator{queries: deps.Queries, issuer: deps.Issuer},
			connectionLimiter,
			events.NewSubscriber(deps.Redis, deps.Logger),
			&wsSnapshotter{queries: deps.Queries, publisher: deps.Publisher},
			deps.Logger,
		)
		s.router.Get("/instances/{instanceId}/ws", func(w http.ResponseWriter, r *http.Request) {
			instanceID, err := domain.ParseID("path.instanceId", chi.URLParam(r, "instanceId"))
			if err != nil {
				http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
				return
			}
			wsHandler.ServeHTTP(w, r, instanceID)
		})
	}
}
