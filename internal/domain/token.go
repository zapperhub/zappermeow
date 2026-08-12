package domain

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Audience separates the two administrative planes. A token minted for one is
// never accepted by the other (constitution, principle II).
type Audience string

const (
	// AudiencePlatform is the super-admin plane: tenant management.
	AudiencePlatform Audience = "platform"
	// AudienceTenant is the tenant plane: instances, keys and webhooks.
	AudienceTenant Audience = "tenant"
)

// Valid reports whether a is one of the two known audiences.
func (a Audience) Valid() bool {
	return a == AudiencePlatform || a == AudienceTenant
}

// TokenClaims is the decoded content of an access token.
type TokenClaims struct {
	Subject  ID
	Audience Audience
	// TenantID is set only for the tenant audience.
	TenantID *ID
	// PasswordChange mirrors the user's pending temporary password at issue
	// time. It is a hint only: middlewares re-read the authoritative flag from
	// the database on every request.
	PasswordChange bool
	IssuedAt       time.Time
	ExpiresAt      time.Time
}

// jwtClaims is the wire representation signed with HS256.
type jwtClaims struct {
	jwt.RegisteredClaims
	TenantID       string `json:"tenant_id,omitempty"`
	PasswordChange bool   `json:"pwd_change"`
}

// ErrInvalidToken covers every rejection reason of an access token; callers
// must not surface the distinction to clients.
var ErrInvalidToken = errors.New("invalid token")

// TokenIssuer mints and verifies access tokens. Issuer and verifier are the
// same service, so a symmetric algorithm (HS256) is the right trade-off
// (research R3).
type TokenIssuer struct {
	key []byte
	ttl time.Duration
	now func() time.Time
}

// NewTokenIssuer builds an issuer from the deploy-provided signing key.
func NewTokenIssuer(key []byte, ttl time.Duration) *TokenIssuer {
	return &TokenIssuer{key: key, ttl: ttl, now: func() time.Time { return time.Now().UTC() }}
}

// TTL is the lifetime given to freshly issued tokens.
func (i *TokenIssuer) TTL() time.Duration { return i.ttl }

// Issue signs a token for the subject and returns it with its claims.
func (i *TokenIssuer) Issue(subject ID, audience Audience, tenantID *ID, passwordChange bool) (string, TokenClaims, error) {
	if !audience.Valid() {
		return "", TokenClaims{}, fmt.Errorf("issue token: unknown audience %q", audience)
	}
	if audience == AudienceTenant && tenantID == nil {
		return "", TokenClaims{}, errors.New("issue token: tenant audience requires a tenant id")
	}

	issuedAt := i.now()
	expiresAt := issuedAt.Add(i.ttl)

	claims := jwtClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject.String(),
			Audience:  jwt.ClaimStrings{string(audience)},
			IssuedAt:  jwt.NewNumericDate(issuedAt),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
		PasswordChange: passwordChange,
	}
	if tenantID != nil {
		claims.TenantID = tenantID.String()
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(i.key)
	if err != nil {
		return "", TokenClaims{}, fmt.Errorf("sign token: %w", err)
	}

	return signed, TokenClaims{
		Subject:        subject,
		Audience:       audience,
		TenantID:       tenantID,
		PasswordChange: passwordChange,
		IssuedAt:       issuedAt.Truncate(time.Second),
		ExpiresAt:      expiresAt.Truncate(time.Second),
	}, nil
}

// Parse verifies the signature, algorithm and expiry, then returns the claims.
// Every failure collapses into ErrInvalidToken.
func (i *TokenIssuer) Parse(raw string) (TokenClaims, error) {
	parsed, err := jwt.ParseWithClaims(raw, &jwtClaims{},
		func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("%w: unexpected signing method %v", ErrInvalidToken, t.Header["alg"])
			}
			return i.key, nil
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return TokenClaims{}, fmt.Errorf("%w: %s", ErrInvalidToken, err.Error())
	}

	claims, ok := parsed.Claims.(*jwtClaims)
	if !ok || !parsed.Valid {
		return TokenClaims{}, ErrInvalidToken
	}

	subject, err := uuidFromString(claims.Subject)
	if err != nil {
		return TokenClaims{}, fmt.Errorf("%w: malformed subject", ErrInvalidToken)
	}

	if len(claims.Audience) != 1 {
		return TokenClaims{}, fmt.Errorf("%w: exactly one audience is required", ErrInvalidToken)
	}
	audience := Audience(claims.Audience[0])
	if !audience.Valid() {
		return TokenClaims{}, fmt.Errorf("%w: unknown audience", ErrInvalidToken)
	}

	out := TokenClaims{
		Subject:        subject,
		Audience:       audience,
		PasswordChange: claims.PasswordChange,
	}
	if claims.IssuedAt != nil {
		out.IssuedAt = claims.IssuedAt.UTC()
	}
	if claims.ExpiresAt != nil {
		out.ExpiresAt = claims.ExpiresAt.UTC()
	}

	switch audience {
	case AudienceTenant:
		tenantID, err := uuidFromString(claims.TenantID)
		if err != nil {
			return TokenClaims{}, fmt.Errorf("%w: tenant audience requires a tenant id", ErrInvalidToken)
		}
		out.TenantID = &tenantID
	case AudiencePlatform:
		if claims.TenantID != "" {
			return TokenClaims{}, fmt.Errorf("%w: platform audience must not carry a tenant id", ErrInvalidToken)
		}
	}

	return out, nil
}

func uuidFromString(raw string) (ID, error) {
	if raw == "" {
		return ID{}, errors.New("empty uuid")
	}
	return parseUUID(raw)
}
