package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"google.golang.org/grpc"

	"github.com/zapperhub/zappermeow/internal/config"
	"github.com/zapperhub/zappermeow/internal/events"
	"github.com/zapperhub/zappermeow/internal/lease"
	sessionv1 "github.com/zapperhub/zappermeow/internal/pb/sessionv1"
	"github.com/zapperhub/zappermeow/internal/store"
	"github.com/zapperhub/zappermeow/internal/wa"
	"github.com/zapperhub/zappermeow/internal/worker"
)

// drainGrace bounds how long the worker may spend handing back leases and
// closing sessions. The deploy targets give it a stop_grace_period well above
// this, so a rolling update never kills a worker mid-drain.
const drainGrace = 45 * time.Second

// runSessionWorker boots the stateful plane: the process that owns WhatsApp
// sessions. It shares the same Postgres and Redis as the API — one pool, one
// Redis — and exposes no public HTTP: its only listener is the private gRPC
// endpoint the API dials through the lease.
//
// Migrations of the API schema are NOT applied here; `serve` owns those. The
// worker applies only the HyperMeow store upgrade, versioned separately by the
// library itself.
func runSessionWorker(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := newLogger(cfg.SlogLevel())
	slog.SetDefault(logger)

	advertiseAddr, err := cfg.WorkerAdvertise()
	if err != nil {
		return err
	}
	workerID, err := workerIdentity(advertiseAddr)
	if err != nil {
		return err
	}
	logger = logger.With(slog.String("worker_id", workerID))

	pool, err := store.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	redisClient, err := store.NewRedis(ctx, store.RedisOptions{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})
	if err != nil {
		return err
	}
	defer func() { _ = redisClient.Close() }()

	container, err := wa.NewContainer(ctx, pool, cfg.DeviceName, logger)
	if err != nil {
		return err
	}
	defer func() { _ = container.Close() }()
	logger.Info("whatsmeow store upgraded")

	queries := store.New(pool)
	leases := lease.New(queries, lease.Options{
		WorkerID: workerID,
		GRPCAddr: advertiseAddr,
		Expiry:   cfg.LeaseExpiry,
	})

	supervisor := worker.NewSupervisor(worker.Options{
		Queries:       queries,
		Leases:        leases,
		Publisher:     events.NewPublisher(redisClient),
		Factory:       container,
		Logger:        logger,
		PairingWindow: cfg.PairingWindow,
		MaxSessions:   cfg.MaxSessionsPerWorker,
	})

	runtime := worker.NewRuntime(worker.RuntimeOptions{
		Supervisor:        supervisor,
		Leases:            leases,
		Subscriber:        events.NewSubscriber(redisClient, logger),
		Logger:            logger,
		HeartbeatInterval: cfg.LeaseHeartbeatInterval,
		ReconcileInterval: cfg.ReconcileInterval,
	})

	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", cfg.WorkerGRPCListenAddr)
	if err != nil {
		return fmt.Errorf("listen for grpc: %w", err)
	}

	grpcServer := grpc.NewServer()
	sessionv1.RegisterSessionServiceServer(grpcServer, worker.NewGRPCServer(supervisor, leases, logger))

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("grpc listening",
			slog.String("addr", cfg.WorkerGRPCListenAddr),
			slog.String("advertise", advertiseAddr))
		serveErr <- grpcServer.Serve(listener)
	}()

	// Retention runs alongside the session loops: one daily DELETE does not
	// justify standing up the jobs service, and the advisory lock keeps several
	// workers from duplicating it (research R11).
	retention := worker.NewRetention(queries, cfg.ConnectionEventsRetention, 24*time.Hour, logger)
	go retention.Run(ctx)

	runErr := make(chan error, 1)
	go func() { runErr <- runtime.Run(ctx) }()

	select {
	case err := <-serveErr:
		return err
	case err := <-runErr:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining sessions")

		// Stop accepting commands before releasing anything: a command arriving
		// mid-drain could reconnect a session this process is about to hand over.
		grpcServer.GracefulStop()

		drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainGrace)
		defer cancel()
		if err := supervisor.Shutdown(drainCtx); err != nil {
			return fmt.Errorf("drain: %w", err)
		}
		return nil
	}
}

// workerIdentity builds an identity unique to this *process*, not to the host
// or the address it serves on.
//
// The random suffix is what makes it safe: a worker killed without draining
// leaves its leases marked as owned, and a replacement sharing the identity
// would renew those heartbeats forever while holding no session in memory. The
// sessions would then never expire, never be adopted, and never come back —
// every command against them answering "the owner has no such session". With a
// fresh identity per process the abandoned leases simply expire and the fleet
// picks them up, which is exactly the failover path that already works.
func workerIdentity(advertiseAddr string) (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("resolve hostname: %w", err)
	}

	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("generate worker identity: %w", err)
	}
	return fmt.Sprintf("%s/%s/%s", hostname, advertiseAddr, hex.EncodeToString(suffix)), nil
}
