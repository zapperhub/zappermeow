package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validKey is a 64-character signing key, the minimum accepted length.
const validKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func setMinimalEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ZAPPERMEOW_DATABASE_URL", "postgres://user:pass@localhost:5432/db")
	t.Setenv("ZAPPERMEOW_JWT_SIGNING_KEY", validKey)
	// Point away from the real /run/secrets so the host never influences a test.
	t.Setenv("ZAPPERMEOW_SECRETS_DIR", filepath.Join(t.TempDir(), "empty"))
}

func TestLoadAppliesDocumentedDefaults(t *testing.T) {
	setMinimalEnv(t)

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, ":8080", cfg.ListenAddr)
	assert.Equal(t, "localhost:6379", cfg.RedisAddr)
	assert.Equal(t, time.Hour, cfg.JWTTTL, "tokens default to a one-hour lifetime")
	assert.Equal(t, 5, cfg.LockoutMaxFailures)
	assert.Equal(t, 15*time.Minute, cfg.LockoutWindow)
	assert.Equal(t, 30, cfg.LoginRateLimit)
	assert.False(t, cfg.TrustProxyHeaders, "forwarded headers must be opt-in")
	assert.False(t, cfg.OTelEnabled, "tracing is off by default")
	assert.False(t, cfg.BootstrapConfigured())
}

func TestLoadReadsOverrides(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("ZAPPERMEOW_LISTEN_ADDR", ":9000")
	t.Setenv("ZAPPERMEOW_LOCKOUT_MAX_FAILURES", "3")
	t.Setenv("ZAPPERMEOW_LOCKOUT_WINDOW", "5m")
	t.Setenv("ZAPPERMEOW_JWT_TTL", "30m")

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, ":9000", cfg.ListenAddr)
	assert.Equal(t, 3, cfg.LockoutMaxFailures)
	assert.Equal(t, 5*time.Minute, cfg.LockoutWindow)
	assert.Equal(t, 30*time.Minute, cfg.JWTTTL)
}

// Deploy-runtime secrets must win over environment variables: in production the
// env var is at most a leftover, the mounted secret is the truth.
func TestSecretFilesOverrideEnvironment(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("ZAPPERMEOW_BOOTSTRAP_EMAIL", "env@example.com")
	t.Setenv("ZAPPERMEOW_BOOTSTRAP_PASSWORD", "from-env-password")

	secretsDir := t.TempDir()
	t.Setenv("ZAPPERMEOW_SECRETS_DIR", secretsDir)
	// A trailing newline is what a `docker secret` file normally looks like.
	require.NoError(t, os.WriteFile(filepath.Join(secretsDir, "bootstrap_email"), []byte("secret@example.com\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(secretsDir, "bootstrap_password"), []byte("from-secret-file\n"), 0o600))

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, "secret@example.com", cfg.BootstrapEmail)
	assert.Equal(t, "from-secret-file", cfg.BootstrapPassword)
	assert.True(t, cfg.BootstrapConfigured())
}

func TestMissingSecretsDirIsNotAnError(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("ZAPPERMEOW_SECRETS_DIR", "/nonexistent/run/secrets")

	_, err := Load()
	require.NoError(t, err, "development runs have no /run/secrets mount")
}

func TestValidateRejectsIncompleteConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantMsg string
	}{
		{
			name:    "missing database url",
			mutate:  func(c *Config) { c.DatabaseURL = "" },
			wantMsg: "DATABASE_URL is required",
		},
		{
			name:    "missing signing key",
			mutate:  func(c *Config) { c.JWTSigningKey = "" },
			wantMsg: "JWT_SIGNING_KEY is required",
		},
		{
			name:    "signing key too short",
			mutate:  func(c *Config) { c.JWTSigningKey = "too-short" },
			wantMsg: "at least 64 characters",
		},
		{
			name:    "malformed bootstrap email",
			mutate:  func(c *Config) { c.BootstrapEmail = "not-an-email"; c.BootstrapPassword = "whatever1" },
			wantMsg: "BOOTSTRAP_EMAIL is not a valid email",
		},
		{
			name:    "bootstrap email without password",
			mutate:  func(c *Config) { c.BootstrapEmail = "root@example.com"; c.BootstrapPassword = "" },
			wantMsg: "BOOTSTRAP_PASSWORD is required",
		},
		{
			name:    "non-positive lockout window",
			mutate:  func(c *Config) { c.LockoutWindow = 0 },
			wantMsg: "LOCKOUT_WINDOW must be positive",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{
				DatabaseURL:          "postgres://localhost/db",
				RedisAddr:            "localhost:6379",
				JWTSigningKey:        validKey,
				JWTTTL:               time.Hour,
				LockoutMaxFailures:   5,
				LockoutWindow:        15 * time.Minute,
				LoginRateLimit:       30,
				OperationalRateLimit: 600,
			}
			tc.mutate(cfg)

			err := cfg.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}

// A configuration error must never quote the secret it rejected.
func TestValidateDoesNotEchoSecrets(t *testing.T) {
	cfg := &Config{
		DatabaseURL:          "postgres://user:sup3rs3cr3t@localhost/db",
		RedisAddr:            "localhost:6379",
		JWTSigningKey:        "short-but-secret-key",
		JWTTTL:               time.Hour,
		LockoutMaxFailures:   5,
		LockoutWindow:        time.Minute,
		LoginRateLimit:       30,
		OperationalRateLimit: 600,
	}

	err := cfg.Validate()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "short-but-secret-key")
	assert.NotContains(t, err.Error(), "sup3rs3cr3t")
}

func TestLoadAppliesSessionWorkerDefaults(t *testing.T) {
	setMinimalEnv(t)

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, ":9090", cfg.WorkerGRPCListenAddr)
	assert.Equal(t, 200, cfg.MaxSessionsPerWorker, "sizing knob, not a product quota")
	assert.Equal(t, 180*time.Second, cfg.PairingWindow)
	assert.Equal(t, 10*time.Second, cfg.LeaseHeartbeatInterval)
	assert.Equal(t, 30*time.Second, cfg.LeaseExpiry)
	assert.Equal(t, 15*time.Second, cfg.ReconcileInterval)
	assert.Equal(t, 720*time.Hour, cfg.ConnectionEventsRetention, "30 days of connection trail")
}

func TestLoadReadsSessionWorkerOverrides(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("ZAPPERMEOW_WORKER_GRPC_LISTEN_ADDR", "0.0.0.0:7000")
	t.Setenv("ZAPPERMEOW_WORKER_ADVERTISE_ADDR", "worker-1.internal:7000")
	t.Setenv("ZAPPERMEOW_MAX_SESSIONS_PER_WORKER", "50")
	t.Setenv("ZAPPERMEOW_PAIRING_WINDOW", "90s")
	t.Setenv("ZAPPERMEOW_CONNECTION_EVENTS_RETENTION", "168h")

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, "0.0.0.0:7000", cfg.WorkerGRPCListenAddr)
	assert.Equal(t, 50, cfg.MaxSessionsPerWorker)
	assert.Equal(t, 90*time.Second, cfg.PairingWindow)
	assert.Equal(t, 168*time.Hour, cfg.ConnectionEventsRetention)

	advertise, err := cfg.WorkerAdvertise()
	require.NoError(t, err)
	assert.Equal(t, "worker-1.internal:7000", advertise)
}

// A lease that expires before its owner can renew it would be handed to a
// second worker while the first is still connected — the double ownership
// Principle III forbids. The configuration must refuse that shape outright.
func TestValidateRejectsLeaseExpiryTooCloseToHeartbeat(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("ZAPPERMEOW_LEASE_HEARTBEAT_INTERVAL", "10s")
	t.Setenv("ZAPPERMEOW_LEASE_EXPIRY", "20s")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "LEASE_EXPIRY must be at least 3x")
}

func TestValidateRejectsNonPositiveWorkerDurations(t *testing.T) {
	for _, tc := range []struct{ env, want string }{
		{"ZAPPERMEOW_PAIRING_WINDOW", "PAIRING_WINDOW must be positive"},
		{"ZAPPERMEOW_RECONCILE_INTERVAL", "RECONCILE_INTERVAL must be positive"},
		{"ZAPPERMEOW_CONNECTION_EVENTS_RETENTION", "CONNECTION_EVENTS_RETENTION must be positive"},
	} {
		t.Run(tc.env, func(t *testing.T) {
			setMinimalEnv(t)
			t.Setenv(tc.env, "0s")

			_, err := Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// Without an explicit advertise address the worker falls back to its hostname,
// which is what makes the default work unchanged inside a container.
func TestWorkerAdvertiseFallsBackToHostname(t *testing.T) {
	setMinimalEnv(t)

	cfg, err := Load()
	require.NoError(t, err)

	advertise, err := cfg.WorkerAdvertise()
	require.NoError(t, err)

	host, err := os.Hostname()
	require.NoError(t, err)
	assert.Equal(t, host+":9090", advertise)
}
