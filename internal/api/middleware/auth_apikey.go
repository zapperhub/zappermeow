package middleware

import (
	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/zapperhub/zappermeow/internal/api/httperr"
	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/store"
)

// APIKeyHeader carries the operational credential.
const APIKeyHeader = "X-Api-Key"

// APIKeyAuthenticator guards the operational plane.
//
// It resolves the whole isolation chain on every request — key → instance →
// tenant — in a single indexed lookup, and is the template every future
// operational route (messages, groups, media) reuses.
type APIKeyAuthenticator struct {
	api     huma.API
	queries *store.Queries
}

// NewAPIKeyAuthenticator builds the operational middleware.
func NewAPIKeyAuthenticator(api huma.API, queries *store.Queries) *APIKeyAuthenticator {
	return &APIKeyAuthenticator{api: api, queries: queries}
}

// Authenticate validates the key and binds it to the instance in the URL.
//
// The three refusals are deliberately different: a bad or revoked key is a 401,
// a valid key pointing at another instance is a 404 that confirms nothing, and
// a suspended tenant is a 403 to a caller that already proved it holds a key.
func (a *APIKeyAuthenticator) Authenticate() func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		token := ctx.Header(APIKeyHeader)
		if token == "" {
			httperr.Write(a.api, ctx, domain.ErrUnauthenticated("An API key is required"))
			return
		}

		row, err := a.queries.GetKeyForAuth(ctx.Context(), domain.HashAPIKey(token))
		if err != nil {
			// Unknown key: the same answer a malformed one gets.
			httperr.Write(a.api, ctx, domain.ErrUnauthenticated("The API key is invalid or has been revoked"))
			return
		}

		// Revocation takes effect here, on the very next request.
		if domain.APIKeyStatus(row.KeyStatus) != domain.APIKeyActive {
			httperr.Write(a.api, ctx, domain.ErrUnauthenticated("The API key is invalid or has been revoked"))
			return
		}

		// The key must belong to the instance addressed by the URL — a key of
		// another instance is refused even inside the same tenant (FR-013).
		urlInstanceID, err := domain.ParseID("path.instanceId", instancePathParam(ctx))
		if err != nil || urlInstanceID != row.InstanceID {
			httperr.Write(a.api, ctx, domain.ErrNotFound())
			return
		}

		if domain.TenantStatus(row.TenantStatus) != domain.TenantActive {
			httperr.Write(a.api, ctx, domain.ErrTenantSuspended())
			return
		}

		operator := Operator{
			InstanceID:    row.InstanceID,
			TenantID:      row.TenantID,
			InstanceName:  row.InstanceName,
			InstanceState: row.InstanceState,
			KeyID:         row.KeyID,
			KeyPrefix:     row.KeyPrefix,
			KeyLabel:      row.Label,
		}

		requestCtx := WithOperator(ctx.Context(), operator)
		requestCtx = WithTenantID(requestCtx, operator.TenantID)
		requestCtx = WithInstanceID(requestCtx, operator.InstanceID)
		next(huma.WithContext(ctx, requestCtx))
	}
}

// instancePathParam reads the instance id from the route, falling back to the
// chi route context when huma cannot resolve it directly.
func instancePathParam(ctx huma.Context) string {
	if value := ctx.Param("instanceId"); value != "" {
		return value
	}
	if rctx := chi.RouteContext(ctx.Context()); rctx != nil {
		return rctx.URLParam("instanceId")
	}
	return ""
}
