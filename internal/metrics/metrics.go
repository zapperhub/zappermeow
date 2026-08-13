// Package metrics owns the Prometheus collectors of the service. It lives in
// its own package so the API layer, the middlewares and the domain services can
// all record without importing one another.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Registry is the collector registry exposed on /metrics.
var Registry = prometheus.NewRegistry()

var (
	// RequestDuration tracks latency per route, the baseline required by the
	// constitution's observability principle.
	RequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "zappermeow",
		Subsystem: "http",
		Name:      "request_duration_seconds",
		Help:      "Latency of HTTP requests by route and status.",
		Buckets:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	}, []string{"method", "route", "status"})

	// LoginAttempts counts login outcomes: succeeded, failed or locked.
	LoginAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "zappermeow",
		Subsystem: "auth",
		Name:      "login_attempts_total",
		Help:      "Login attempts by outcome.",
	}, []string{"outcome"})

	// AccountLockouts counts accounts entering temporary lockout.
	AccountLockouts = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "zappermeow",
		Subsystem: "auth",
		Name:      "account_lockouts_total",
		Help:      "Accounts placed in temporary lockout after consecutive failures.",
	})

	// RateLimitRejections counts requests refused by a GCRA limiter.
	RateLimitRejections = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "zappermeow",
		Subsystem: "http",
		Name:      "rate_limit_rejections_total",
		Help:      "Requests rejected by a rate limiter, by limiter scope.",
	}, []string{"scope"})

	// APIKeysActive reports how many API keys are currently usable.
	APIKeysActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "zappermeow",
		Subsystem: "api_keys",
		Name:      "active",
		Help:      "Number of API keys in the active state.",
	})
)

// Login outcome label values.
const (
	OutcomeSucceeded = "succeeded"
	OutcomeFailed    = "failed"
	OutcomeLocked    = "locked"
)

// Rate limiter scope label values.
const (
	ScopeLogin       = "login_origin"
	ScopeOperational = "operational_key"
	// ScopeConnection covers the routes that drive a session, which accept
	// either credential and are therefore keyed by whichever was presented.
	ScopeConnection = "connection"
)

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		RequestDuration,
		LoginAttempts,
		AccountLockouts,
		RateLimitRejections,
		APIKeysActive,
	)
}
