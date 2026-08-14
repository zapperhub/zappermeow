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

	// SessionsConnected reports how many WhatsApp sessions a worker holds.
	// Labelled by worker rather than by instance: one series per instance would
	// grow with the tenant base, which is what logs are for.
	SessionsConnected = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "zappermeow",
		Subsystem: "sessions",
		Name:      "connected",
		Help:      "WhatsApp sessions currently connected, by worker.",
	}, []string{"worker_id"})

	// SessionStateTransitions counts every move through the connection state
	// machine, which is what makes an instability visible in aggregate.
	SessionStateTransitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "zappermeow",
		Subsystem: "sessions",
		Name:      "state_transitions_total",
		Help:      "Connection state transitions, by destination state and reason.",
	}, []string{"to", "reason"})

	// PairingAttempts counts pairing attempts by method and outcome.
	PairingAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "zappermeow",
		Subsystem: "sessions",
		Name:      "pairing_attempts_total",
		Help:      "Pairing attempts, by method and result.",
	}, []string{"method", "result"})

	// SessionReconnects counts reconnections, split by which layer drove them:
	// the client's own retry or a lease being adopted elsewhere.
	SessionReconnects = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "zappermeow",
		Subsystem: "sessions",
		Name:      "reconnects_total",
		Help:      "Session reconnections, by layer.",
	}, []string{"layer"})

	// LeaseAcquisitions and LeaseLosses track ownership changes.
	LeaseAcquisitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "zappermeow",
		Subsystem: "leases",
		Name:      "acquisitions_total",
		Help:      "Session leases acquired, by worker.",
	}, []string{"worker_id"})

	LeaseLosses = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "zappermeow",
		Subsystem: "leases",
		Name:      "losses_total",
		Help:      "Session leases lost to another worker, by worker.",
	}, []string{"worker_id"})

	// StreamReplaced must stay at zero. Any increment means the same device
	// credentials were opened twice — a violation of exclusive session
	// ownership (principle III), not a statistic to watch trend.
	StreamReplaced = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "zappermeow",
		Subsystem: "sessions",
		Name:      "stream_replaced_total",
		Help:      "Sessions replaced elsewhere. Non-zero means exclusive ownership was violated.",
	})

	// ProxyConnectFailures counts connections that failed while dialling
	// through a configured proxy. There is no direct-connection fallback, so a
	// rising count means a tenant's proxy is down and their number is offline.
	ProxyConnectFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "zappermeow",
		Subsystem: "proxy",
		Name:      "connect_failures_total",
		Help:      "Connection attempts that failed through a configured egress proxy.",
	})

	// PasskeyPairings counts pairing attempts that went through the passkey
	// step, by outcome.
	PasskeyPairings = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "zappermeow",
		Subsystem: "sessions",
		Name:      "passkey_pairings_total",
		Help:      "Pairing attempts that required the passkey step, by outcome.",
	}, []string{"outcome"})

	// StreamErrors counts stream closures with a code the library does not
	// recognise, labelled by that code. Cardinality is bounded by the server's
	// vocabulary, not by the fleet size, so no per-instance label is added.
	StreamErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "zappermeow",
		Subsystem: "sessions",
		Name:      "stream_errors_total",
		Help:      "Stream closures with an unknown error code, by code.",
	}, []string{"code"})

	// WebSocketClients reports how many event-channel listeners are attached.
	WebSocketClients = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "zappermeow",
		Subsystem: "websocket",
		Name:      "clients",
		Help:      "Currently connected event-channel clients.",
	})

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
		SessionsConnected,
		SessionStateTransitions,
		PairingAttempts,
		SessionReconnects,
		LeaseAcquisitions,
		LeaseLosses,
		StreamReplaced,
		ProxyConnectFailures,
		PasskeyPairings,
		StreamErrors,
		WebSocketClients,
		RateLimitRejections,
		APIKeysActive,
	)
}
