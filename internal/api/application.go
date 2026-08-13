package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/zapperhub/zappermeow/internal/api/sessionclient"
	"github.com/zapperhub/zappermeow/internal/config"
	"github.com/zapperhub/zappermeow/internal/events"
	"github.com/zapperhub/zappermeow/internal/lease"
	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/domain/services"
	"github.com/zapperhub/zappermeow/internal/store"
)

// Options carries the process-wide dependencies into the composition root.
type Options struct {
	Config *config.Config
	Logger *slog.Logger
	Pool   *pgxpool.Pool
	Redis  *redis.Client
}

// Application is the composition root: it wires stores, services, middlewares
// and routes into a ready-to-serve handler.
type Application struct {
	server *Server
}

// NewApplication builds every service and registers every route. It also runs
// the super-admin bootstrap, which must happen after migrations and before the
// service starts accepting traffic.
func NewApplication(ctx context.Context, opts Options) (*Application, error) {
	cfg := opts.Config
	logger := opts.Logger

	queries := store.New(opts.Pool)
	recorder := services.NewEventRecorder(logger)
	issuer := domain.NewTokenIssuer([]byte(cfg.JWTSigningKey), cfg.JWTTTL)

	// The initial super-admin must exist before the service accepts traffic,
	// and the schema before that — migrations already ran by this point.
	bootstrap := services.NewBootstrapService(opts.Pool, queries, recorder, logger)
	if err := bootstrap.Run(ctx, services.BootstrapCredentials{
		Email:    cfg.BootstrapEmail,
		Password: cfg.BootstrapPassword,
	}); err != nil {
		return nil, err
	}

	// The API reads leases but never acquires them: it only needs to know which
	// worker owns a session in order to dial it.
	leases := lease.New(queries, lease.Options{
		WorkerID: "api",
		Expiry:   cfg.LeaseExpiry,
	})
	sessions := sessionclient.New(leases, opts.Redis, logger)

	server := NewServer(logger, cfg.TrustProxyHeaders)
	server.RegisterRoutes(RouteDeps{
		Config:    cfg,
		Logger:    logger,
		Pool:      opts.Pool,
		Redis:     opts.Redis,
		Queries:   queries,
		Recorder:  recorder,
		Issuer:    issuer,
		Leases:    leases,
		Sessions:  sessions,
		Publisher: events.NewPublisher(opts.Redis),
	})

	return &Application{server: server}, nil
}

// Handler returns the root HTTP handler of the application.
func (a *Application) Handler() http.Handler { return a.server.Handler() }
