package middleware

import (
	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/zapperhub/zappermeow/internal/api/httperr"
	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/store"
)

// PrincipalKind tells which credential opened the door.
type PrincipalKind string

const (
	// PrincipalAPIKey is the instance's own key: an integration acting without
	// a human logged in.
	PrincipalAPIKey PrincipalKind = "api_key"
	// PrincipalTenantAdmin is the tenant administrator's token.
	PrincipalTenantAdmin PrincipalKind = "tenant_admin"
)

// ConnectionPrincipal is who is driving a connection route, whichever
// credential they used. The rate limiter keys on it, so the quota follows the
// credential: per instance for a key, per tenant for an admin token.
type ConnectionPrincipal struct {
	Kind       PrincipalKind
	InstanceID uuid.UUID
	TenantID   uuid.UUID
	// KeyID is set only for PrincipalAPIKey.
	KeyID uuid.UUID
}

// RateLimitKey is the bucket this principal draws from.
func (p ConnectionPrincipal) RateLimitKey() string {
	if p.Kind == PrincipalAPIKey {
		// Per instance, so one number cannot eat another's allowance even
		// inside the same tenant (constitution, principle II).
		return "rl:conn:key:" + p.KeyID.String()
	}
	return "rl:conn:tenant:" + p.TenantID.String()
}

// ConnectionAuthenticator guards the routes that drive a session.
//
// These routes accept either credential (FR-039), which is what lets an
// integration provision and monitor a number without a human logged in. Both
// paths converge on the same invariant: the credential must resolve to the
// instance named in the URL, and a mismatch is a 404 that confirms nothing.
type ConnectionAuthenticator struct {
	api     huma.API
	queries *store.Queries
	admin   *Authenticator
	keys    *APIKeyAuthenticator
}

// NewConnectionAuthenticator builds the dual-credential middleware.
func NewConnectionAuthenticator(
	api huma.API,
	queries *store.Queries,
	admin *Authenticator,
	keys *APIKeyAuthenticator,
) *ConnectionAuthenticator {
	return &ConnectionAuthenticator{api: api, queries: queries, admin: admin, keys: keys}
}

// Authenticate accepts an instance API key or a tenant token.
func (a *ConnectionAuthenticator) Authenticate() func(huma.Context, func(huma.Context)) {
	keyAuth := a.keys.Authenticate()
	tenantAuth := a.admin.Tenant()

	return func(ctx huma.Context, next func(huma.Context)) {
		// The key wins when both are present: it is the more specific
		// credential, scoped to exactly this instance.
		if ctx.Header(APIKeyHeader) != "" {
			keyAuth(ctx, func(inner huma.Context) {
				operator, ok := OperatorFrom(inner.Context())
				if !ok {
					httperr.Write(a.api, inner, domain.ErrInternal(nil))
					return
				}
				principal := ConnectionPrincipal{
					Kind:       PrincipalAPIKey,
					InstanceID: operator.InstanceID,
					TenantID:   operator.TenantID,
					KeyID:      operator.KeyID,
				}
				next(huma.WithContext(inner, WithConnectionPrincipal(inner.Context(), principal)))
			})
			return
		}

		tenantAuth(ctx, func(inner huma.Context) {
			admin, ok := AdminFrom(inner.Context())
			if !ok || admin.User.TenantID == nil {
				httperr.Write(a.api, inner, domain.ErrUnauthenticated("A tenant token is required"))
				return
			}

			instanceID, err := domain.ParseID("path.instanceId", instancePathParam(inner))
			if err != nil {
				httperr.Write(a.api, inner, domain.ErrNotFound())
				return
			}

			// Scoping the lookup by tenant in SQL is what makes another
			// tenant's instance indistinguishable from one that never existed.
			row, err := a.queries.GetInstanceByIDAndTenant(inner.Context(), store.GetInstanceByIDAndTenantParams{
				ID:       instanceID,
				TenantID: *admin.User.TenantID,
			})
			if err != nil {
				httperr.Write(a.api, inner, domain.ErrNotFound())
				return
			}

			principal := ConnectionPrincipal{
				Kind:       PrincipalTenantAdmin,
				InstanceID: row.ID,
				TenantID:   row.TenantID,
			}

			requestCtx := WithConnectionPrincipal(inner.Context(), principal)
			requestCtx = WithInstanceID(requestCtx, row.ID)
			next(huma.WithContext(inner, requestCtx))
		})
	}
}
