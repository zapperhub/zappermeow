// Package api mounts the HTTP surface: chi for routing, huma for typed
// operations and the generated OpenAPI 3.1 document, plus the health and
// metrics endpoints that are the only routes allowed to skip authentication.
package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/zapperhub/zappermeow/internal/api/httperr"
	"github.com/zapperhub/zappermeow/internal/api/middleware"
	"github.com/zapperhub/zappermeow/internal/metrics"
)

// Version is the API version advertised in the OpenAPI document.
const Version = "1.0.0"

// Server owns the router and the huma API the operations are registered on.
type Server struct {
	router chi.Router
	api    huma.API
	logger *slog.Logger
}

// NewServer builds the router, installs the shared middlewares and registers
// the unauthenticated infrastructure routes.
func NewServer(logger *slog.Logger, trustProxyHeaders bool) *Server {
	// Make every error huma raises on its own carry our code/timestamp
	// extensions, and describe this model in the generated OpenAPI document.
	httperr.Install()

	router := chi.NewRouter()
	router.Use(chimw.RequestID)
	router.Use(chimw.Recoverer)
	router.Use(middleware.RequestLogger(logger))
	router.Use(middleware.ClientIPResolver(trustProxyHeaders))

	router.Handle("/metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{
		Registry: metrics.Registry,
	}))

	config := huma.DefaultConfig("zappermeow", Version)
	config.Info.Description = "Account foundation of the zappermeow platform: tenants, instances and credentials."

	// huma installs a schema-link transformer through a create hook, which
	// injects a `$schema` member into every response body and serves the
	// per-schema files it points at. The success envelope is a rigid contract of
	// exactly three members, so both are dropped; the generated OpenAPI document
	// remains what clients read, served at /openapi.json with the docs UI.
	config.CreateHooks = nil
	config.SchemasPath = ""
	config.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"bearerAuth": {
			Type:         "http",
			Scheme:       "bearer",
			BearerFormat: "JWT",
			Description:  "Short-lived administrative token. The audience claim selects the plane: `platform` for tenant management, `tenant` for instances and keys.",
		},
		"apiKeyAuth": {
			Type:        "apiKey",
			In:          "header",
			Name:        "X-Api-Key",
			Description: "Per-instance operational credential. The key must belong to the instance in the URL.",
		},
	}

	api := humachi.New(router, config)
	server := &Server{router: router, api: api, logger: logger}
	server.registerHealth()
	return server
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler { return s.router }

// API exposes the huma API so each feature area can register its operations.
func (s *Server) API() huma.API { return s.api }

// HealthData is the payload of the health check.
type HealthData struct {
	Status string `json:"status" example:"ok" doc:"Liveness marker."`
}

// registerHealth publishes GET /healthz. Together with /metrics it forms the
// only unauthenticated surface allowed by the constitution.
func (s *Server) registerHealth() {
	huma.Register(s.api, huma.Operation{
		OperationID:   "health-check",
		Method:        http.MethodGet,
		Path:          "/healthz",
		Summary:       "Liveness probe",
		Description:   "Reports that the service is up. Unauthenticated by design.",
		Tags:          []string{"infra"},
		DefaultStatus: http.StatusOK,
	}, func(_ context.Context, _ *struct{}) (*httperr.Response[HealthData], error) {
		return httperr.OK(HealthData{Status: "ok"}), nil
	})
}
