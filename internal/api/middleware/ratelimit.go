package middleware

import (
	"log/slog"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"

	"github.com/zapperhub/zappermeow/internal/api/httperr"
	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/metrics"
)

// RateLimiter applies a GCRA allowance keyed by some property of the request.
//
// Redis holds only these counters, never account data. If Redis is unreachable
// the limiter deliberately fails open: losing it must not take authentication
// down with it, and the durable per-account lockout still contains brute force
// on its own (research R4).
type RateLimiter struct {
	api     huma.API
	limiter *redis_rate.Limiter
	limit   redis_rate.Limit
	scope   string
	logger  *slog.Logger
	keyFor  func(huma.Context) (string, bool)
}

// NewLoginRateLimiter limits login attempts per origin address (FR-018).
func NewLoginRateLimiter(api huma.API, client *redis.Client, perMinute int, logger *slog.Logger) *RateLimiter {
	return &RateLimiter{
		api:     api,
		limiter: redis_rate.NewLimiter(client),
		limit:   redis_rate.PerMinute(perMinute),
		scope:   metrics.ScopeLogin,
		logger:  logger,
		keyFor: func(ctx huma.Context) (string, bool) {
			addr := ClientIPFrom(ctx.Context())
			if addr == nil {
				return "", false
			}
			return "rl:login:" + addr.String(), true
		},
	}
}

// NewOperationalRateLimiter limits operational routes per API key, so one
// instance cannot consume another's allowance (constitution, principle II).
func NewOperationalRateLimiter(api huma.API, client *redis.Client, perMinute int, logger *slog.Logger) *RateLimiter {
	return &RateLimiter{
		api:     api,
		limiter: redis_rate.NewLimiter(client),
		limit:   redis_rate.PerMinute(perMinute),
		scope:   metrics.ScopeOperational,
		logger:  logger,
		keyFor: func(ctx huma.Context) (string, bool) {
			operator, ok := OperatorFrom(ctx.Context())
			if !ok {
				return "", false
			}
			return "rl:op:" + operator.KeyID.String(), true
		},
	}
}

// Limit returns the middleware enforcing the allowance.
func (l *RateLimiter) Limit() func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		key, ok := l.keyFor(ctx)
		if !ok {
			next(ctx)
			return
		}

		result, err := l.limiter.Allow(ctx.Context(), key, l.limit)
		if err != nil {
			l.logger.Warn("rate limiter unavailable, allowing request",
				slog.String("scope", l.scope), slog.String("error", err.Error()))
			next(ctx)
			return
		}

		if result.Allowed < 1 {
			metrics.RateLimitRejections.WithLabelValues(l.scope).Inc()
			httperr.Write(l.api, ctx, domain.ErrRateLimited())
			return
		}

		next(ctx)
	}
}
