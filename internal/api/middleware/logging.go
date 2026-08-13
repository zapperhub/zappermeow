package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zapperhub/zappermeow/internal/metrics"
)

// statusRecorder captures the status code for logging and metrics.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Unwrap exposes the underlying writer so http.ResponseController can reach
// capabilities this wrapper does not implement — hijacking above all. Without
// it a WebSocket upgrade behind this middleware answers 501, because the
// upgrader cannot take over the connection it was handed.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// RequestLogger emits one structured line per request and records its latency.
// The tenant and instance attributes are read from the context after the auth
// middlewares ran, so authenticated traffic is always correlatable.
func RequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			recorder := &statusRecorder{ResponseWriter: w}

			next.ServeHTTP(recorder, r)

			if recorder.status == 0 {
				recorder.status = http.StatusOK
			}
			elapsed := time.Since(start)
			route := routePattern(r)

			metrics.RequestDuration.
				WithLabelValues(r.Method, route, strconv.Itoa(recorder.status)).
				Observe(elapsed.Seconds())

			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("route", route),
				slog.Int("status", recorder.status),
				slog.Int64("duration_ms", elapsed.Milliseconds()),
			}
			ctx := r.Context()
			if tenantID, ok := TenantIDFrom(ctx); ok {
				attrs = append(attrs, slog.String("tenant_id", tenantID.String()))
			}
			if instanceID, ok := InstanceIDFrom(ctx); ok {
				attrs = append(attrs, slog.String("instance_id", instanceID.String()))
			}

			level := slog.LevelInfo
			if recorder.status >= http.StatusInternalServerError {
				level = slog.LevelError
			}
			logger.LogAttrs(ctx, level, "http_request", attrs...)
		})
	}
}

// routePattern prefers the chi route template so metric cardinality stays
// bounded by the number of routes, not by the number of resource IDs.
func routePattern(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if pattern := rctx.RoutePattern(); pattern != "" {
			return pattern
		}
	}
	return "unmatched"
}

// ClientIP resolves the origin of a request for the per-origin login limiter.
// X-Forwarded-For is only honoured when the deployment declares it sits behind
// a trusted proxy; otherwise a client could spoof the header and sidestep the
// limit entirely.
func ClientIP(r *http.Request, trustProxyHeaders bool) string {
	if trustProxyHeaders {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			// The left-most entry is the original client.
			if first, _, found := strings.Cut(forwarded, ","); found {
				forwarded = first
			}
			if ip := strings.TrimSpace(forwarded); ip != "" {
				return ip
			}
		}
		if realIP := strings.TrimSpace(r.Header.Get("X-Real-Ip")); realIP != "" {
			return realIP
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
