// Package config loads the service configuration from environment variables
// (12-factor) with deploy-runtime secrets taking precedence, as required by the
// constitution: Swarm/Compose secrets mounted under /run/secrets in production,
// env vars only as a development fallback.
package config

import (
	"errors"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// envPrefix namespaces every variable read by the service.
const envPrefix = "ZAPPERMEOW_"

// Config is the fully resolved configuration of the `serve` subcommand.
type Config struct {
	ListenAddr string `env:"LISTEN_ADDR" envDefault:":8080"`
	LogLevel   string `env:"LOG_LEVEL" envDefault:"info"`

	DatabaseURL   string `env:"DATABASE_URL"`
	RedisAddr     string `env:"REDIS_ADDR" envDefault:"localhost:6379"`
	RedisPassword string `env:"REDIS_PASSWORD"`
	RedisDB       int    `env:"REDIS_DB" envDefault:"0"`

	JWTSigningKey string        `env:"JWT_SIGNING_KEY"`
	JWTTTL        time.Duration `env:"JWT_TTL" envDefault:"1h"`

	BootstrapEmail    string `env:"BOOTSTRAP_EMAIL"`
	BootstrapPassword string `env:"BOOTSTRAP_PASSWORD"`

	LockoutMaxFailures int           `env:"LOCKOUT_MAX_FAILURES" envDefault:"5"`
	LockoutWindow      time.Duration `env:"LOCKOUT_WINDOW" envDefault:"15m"`

	// LoginRateLimit is the per-origin (IP) allowance on POST /auth/login and
	// OperationalRateLimit the per-API-key allowance on operational routes,
	// both expressed in requests per minute (GCRA via redis_rate).
	LoginRateLimit       int `env:"LOGIN_RATE_LIMIT" envDefault:"30"`
	OperationalRateLimit int `env:"OP_RATE_LIMIT" envDefault:"600"`

	// TrustProxyHeaders enables X-Forwarded-For parsing. Only enable when the
	// service sits behind a trusted proxy (Traefik); otherwise clients could
	// spoof their origin and bypass the per-origin login limit.
	TrustProxyHeaders bool `env:"TRUST_PROXY_HEADERS" envDefault:"false"`

	OTelEnabled bool `env:"OTEL_ENABLED" envDefault:"false"`

	SecretsDir string `env:"SECRETS_DIR" envDefault:"/run/secrets"`
}

// secretFiles maps a file name under SecretsDir to the field it overrides.
// A readable file always wins over the environment variable.
func (c *Config) secretFiles() map[string]*string {
	return map[string]*string{
		"database_url":       &c.DatabaseURL,
		"redis_password":     &c.RedisPassword,
		"jwt_signing_key":    &c.JWTSigningKey,
		"bootstrap_email":    &c.BootstrapEmail,
		"bootstrap_password": &c.BootstrapPassword,
	}
}

// Load parses the environment, applies deploy-runtime secrets and validates the
// result. It never logs or returns secret material in error messages.
func Load() (*Config, error) {
	cfg := &Config{}
	if err := env.ParseWithOptions(cfg, env.Options{Prefix: envPrefix}); err != nil {
		return nil, fmt.Errorf("parse environment: %w", err)
	}
	if err := cfg.applySecrets(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applySecrets overlays values found in SecretsDir onto the parsed config.
// A missing directory or file is not an error: env vars remain the fallback.
func (c *Config) applySecrets() error {
	if c.SecretsDir == "" {
		return nil
	}
	for name, field := range c.secretFiles() {
		path := filepath.Join(c.SecretsDir, name)
		raw, err := os.ReadFile(path) //nolint:gosec // path is built from a fixed allowlist
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("read secret %q: %w", name, err)
		}
		if v := strings.TrimSpace(string(raw)); v != "" {
			*field = v
		}
	}
	return nil
}

// minSigningKeyLen is 64 bytes (512 bits) as decided in research R3.
const minSigningKeyLen = 64

// Validate reports configuration that would leave the service unable to serve
// or unsafe to serve. Bootstrap credentials are optional: their absence is
// handled at boot with a warning (spec edge case), not a startup failure.
func (c *Config) Validate() error {
	var problems []string

	if strings.TrimSpace(c.DatabaseURL) == "" {
		problems = append(problems, envPrefix+"DATABASE_URL is required")
	}
	if strings.TrimSpace(c.RedisAddr) == "" {
		problems = append(problems, envPrefix+"REDIS_ADDR is required")
	}
	switch {
	case strings.TrimSpace(c.JWTSigningKey) == "":
		problems = append(problems, envPrefix+"JWT_SIGNING_KEY is required")
	case len(c.JWTSigningKey) < minSigningKeyLen:
		problems = append(problems, fmt.Sprintf("%sJWT_SIGNING_KEY must be at least %d characters", envPrefix, minSigningKeyLen))
	}
	if c.JWTTTL <= 0 {
		problems = append(problems, envPrefix+"JWT_TTL must be positive")
	}
	if c.LockoutMaxFailures < 1 {
		problems = append(problems, envPrefix+"LOCKOUT_MAX_FAILURES must be at least 1")
	}
	if c.LockoutWindow <= 0 {
		problems = append(problems, envPrefix+"LOCKOUT_WINDOW must be positive")
	}
	if c.LoginRateLimit < 1 {
		problems = append(problems, envPrefix+"LOGIN_RATE_LIMIT must be at least 1")
	}
	if c.OperationalRateLimit < 1 {
		problems = append(problems, envPrefix+"OP_RATE_LIMIT must be at least 1")
	}
	if c.BootstrapEmail != "" {
		if _, err := mail.ParseAddress(c.BootstrapEmail); err != nil {
			problems = append(problems, envPrefix+"BOOTSTRAP_EMAIL is not a valid email address")
		}
		if c.BootstrapPassword == "" {
			problems = append(problems, envPrefix+"BOOTSTRAP_PASSWORD is required when "+envPrefix+"BOOTSTRAP_EMAIL is set")
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration: %s", strings.Join(problems, "; "))
	}
	return nil
}

// BootstrapConfigured reports whether both bootstrap credentials are present.
func (c *Config) BootstrapConfigured() bool {
	return c.BootstrapEmail != "" && c.BootstrapPassword != ""
}

// SlogLevel maps the configured level name onto slog's levels.
func (c *Config) SlogLevel() string { return strings.ToLower(c.LogLevel) }
