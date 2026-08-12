// Package middleware carries the authentication, rate limiting and observability
// middlewares. Security is enforced per route group rather than per handler, so
// "every route requires a credential" holds by construction (FR-003).
package middleware

import (
	"context"
	"net/http"
	"net/netip"

	"github.com/google/uuid"
)

type ctxKey int

const (
	keyTenantID ctxKey = iota
	keyInstanceID
	keyAdmin
	keyOperator
	keyClientIP
)

// WithTenantID tags the request context with the tenant it acts upon, which the
// request logger then attaches to every log line (constitution, principle VI).
func WithTenantID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, keyTenantID, id)
}

// TenantIDFrom returns the tenant tagged on the context, if any.
func TenantIDFrom(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(keyTenantID).(uuid.UUID)
	return id, ok
}

// WithInstanceID tags the request context with the instance it acts upon.
func WithInstanceID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, keyInstanceID, id)
}

// InstanceIDFrom returns the instance tagged on the context, if any.
func InstanceIDFrom(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(keyInstanceID).(uuid.UUID)
	return id, ok
}

// WithClientIP tags the request context with the resolved origin address.
func WithClientIP(ctx context.Context, addr netip.Addr) context.Context {
	return context.WithValue(ctx, keyClientIP, addr)
}

// ClientIPFrom returns the origin of the request, when it could be parsed.
func ClientIPFrom(ctx context.Context) *netip.Addr {
	addr, ok := ctx.Value(keyClientIP).(netip.Addr)
	if !ok {
		return nil
	}
	return &addr
}

// ClientIP resolves the origin once per request and stores it, so the login
// limiter and the security-event trail agree on who the caller is.
func ClientIPResolver(trustProxyHeaders bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if addr, err := netip.ParseAddr(ClientIP(r, trustProxyHeaders)); err == nil {
				r = r.WithContext(WithClientIP(r.Context(), addr))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Operator is the principal behind an operational request: the instance the
// route addresses and the API key that proved access to it.
type Operator struct {
	InstanceID    uuid.UUID
	TenantID      uuid.UUID
	InstanceName  string
	InstanceState string
	KeyID         uuid.UUID
	KeyPrefix     string
	KeyLabel      *string
}

// WithOperator stores the operational principal on the context.
func WithOperator(ctx context.Context, operator Operator) context.Context {
	return context.WithValue(ctx, keyOperator, operator)
}

// OperatorFrom retrieves the operational principal.
func OperatorFrom(ctx context.Context) (Operator, bool) {
	operator, ok := ctx.Value(keyOperator).(Operator)
	return operator, ok
}
