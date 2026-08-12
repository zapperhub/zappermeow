package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/zapperhub/zappermeow/internal/api"
	"github.com/zapperhub/zappermeow/internal/config"
	"github.com/zapperhub/zappermeow/internal/store"
)

// shutdownGrace bounds how long in-flight requests have to finish on SIGTERM.
const shutdownGrace = 20 * time.Second

// runServe boots the stateless API: configuration, connections, migrations,
// super-admin bootstrap and finally the HTTP server.
func runServe(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := newLogger(cfg.SlogLevel())
	slog.SetDefault(logger)

	result, err := store.Migrate(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	logger.Info("migrations applied",
		slog.Uint64("schema_version", uint64(result.Version)),
		slog.Bool("changed", result.Applied))

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

	app, err := api.NewApplication(ctx, api.Options{
		Config: cfg,
		Logger: logger,
		Pool:   pool,
		Redis:  redisClient,
	})
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("http server listening", slog.String("addr", cfg.ListenAddr))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return nil
	}
}

// newLogger builds the JSON structured logger required by the constitution.
func newLogger(level string) *slog.Logger {
	var slogLevel slog.Level
	switch level {
	case "debug":
		slogLevel = slog.LevelDebug
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		slogLevel = slog.LevelInfo
	}

	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slogLevel}))
}
