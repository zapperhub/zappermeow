package middleware

import (
	"context"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/zapperhub/zappermeow/internal/api/httperr"
	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/store"
)

// Admin is the authenticated principal behind an administrative request.
type Admin struct {
	User   domain.User
	Tenant domain.TenantStatus
	Claims domain.TokenClaims
}

// TenantID returns the tenant a tenant-plane principal acts for.
func (a Admin) TenantID() domain.ID {
	if a.User.TenantID == nil {
		return domain.ID{}
	}
	return *a.User.TenantID
}

// WithAdmin stores the authenticated principal on the context.
func WithAdmin(ctx context.Context, admin Admin) context.Context {
	return context.WithValue(ctx, keyAdmin, admin)
}

// AdminFrom retrieves the authenticated principal. Handlers registered behind
// an auth middleware can rely on it being present.
func AdminFrom(ctx context.Context) (Admin, bool) {
	admin, ok := ctx.Value(keyAdmin).(Admin)
	return admin, ok
}

// Authenticator validates access tokens for the administrative planes.
//
// Every request re-reads the user and tenant state from Postgres instead of
// trusting the claims alone. That single indexed lookup is what makes
// suspension, deletion and password changes take effect on the very next
// request, without a token blocklist (research R3, SC-004).
type Authenticator struct {
	api     huma.API
	queries *store.Queries
	issuer  *domain.TokenIssuer
}

// NewAuthenticator builds the JWT middlewares.
func NewAuthenticator(api huma.API, queries *store.Queries, issuer *domain.TokenIssuer) *Authenticator {
	return &Authenticator{api: api, queries: queries, issuer: issuer}
}

// Platform guards the super-admin plane.
func (a *Authenticator) Platform() func(huma.Context, func(huma.Context)) {
	return a.authenticate(domain.AudiencePlatform, true)
}

// Tenant guards the tenant plane.
func (a *Authenticator) Tenant() func(huma.Context, func(huma.Context)) {
	return a.authenticate(domain.AudienceTenant, true)
}

// AnyAudience accepts a token of either plane and — uniquely — tolerates a
// pending password change. It exists for the password change route, the only
// operation a user with a temporary password may perform.
func (a *Authenticator) AnyAudience() func(huma.Context, func(huma.Context)) {
	return a.authenticate("", false)
}

func (a *Authenticator) authenticate(want domain.Audience, enforcePasswordChange bool) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		raw, ok := bearerToken(ctx.Header("Authorization"))
		if !ok {
			httperr.Write(a.api, ctx, domain.ErrUnauthenticated("A bearer token is required"))
			return
		}

		claims, err := a.issuer.Parse(raw)
		if err != nil {
			httperr.Write(a.api, ctx, domain.ErrUnauthenticated("The token is invalid or has expired"))
			return
		}

		if want != "" && claims.Audience != want {
			httperr.Write(a.api, ctx, domain.ErrWrongAudience())
			return
		}

		row, err := a.queries.GetUserByID(ctx.Context(), claims.Subject)
		if err != nil {
			// A deleted user (or a deleted tenant, which cascades) fails here,
			// which is how deletion invalidates tokens already in the wild.
			httperr.Write(a.api, ctx, domain.ErrUnauthenticated("The token is invalid or has expired"))
			return
		}
		user := userFromRow(row)

		// A password change or reset invalidates every token minted before it.
		if user.TokenPredatesPasswordChange(claims.IssuedAt) {
			httperr.Write(a.api, ctx, domain.ErrUnauthenticated("The token is invalid or has expired"))
			return
		}

		// The claim is only a hint; the database holds the truth.
		if enforcePasswordChange && user.MustChangePassword {
			httperr.Write(a.api, ctx, domain.ErrPasswordChangeRequired())
			return
		}

		tenantStatus := domain.TenantActive
		if row.TenantStatus != nil {
			tenantStatus = domain.TenantStatus(*row.TenantStatus)
		}
		if tenantStatus != domain.TenantActive {
			httperr.Write(a.api, ctx, domain.ErrTenantSuspended())
			return
		}

		// A tenant token whose subject is no longer a tenant admin, or whose
		// tenant no longer matches the claim, must not pass.
		if claims.Audience == domain.AudienceTenant {
			if user.Role != domain.RoleTenantAdmin || user.TenantID == nil ||
				claims.TenantID == nil || *user.TenantID != *claims.TenantID {
				httperr.Write(a.api, ctx, domain.ErrUnauthenticated("The token is invalid or has expired"))
				return
			}
		}

		requestCtx := WithAdmin(ctx.Context(), Admin{User: user, Tenant: tenantStatus, Claims: claims})
		if user.TenantID != nil {
			requestCtx = WithTenantID(requestCtx, *user.TenantID)
		}
		next(huma.WithContext(ctx, requestCtx))
	}
}

// bearerToken extracts the credential from an Authorization header.
func bearerToken(header string) (string, bool) {
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}

// userFromRow converts the per-request authorisation lookup into a domain user.
func userFromRow(row store.GetUserByIDRow) domain.User {
	user := domain.User{
		ID:                 row.ID,
		Name:               row.Name,
		Email:              row.Email,
		Role:               domain.Role(row.Role),
		TenantID:           row.TenantID,
		MustChangePassword: row.MustChangePassword,
		FailedLoginCount:   int(row.FailedLoginCount),
		PasswordChangedAt:  row.PasswordChangedAt,
		CreatedAt:          row.CreatedAt,
		UpdatedAt:          row.UpdatedAt,
	}
	if row.LockedUntil != nil {
		user.LockedUntil = *row.LockedUntil
	}
	return user
}
