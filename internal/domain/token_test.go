package domain

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testIssuer(t *testing.T) *TokenIssuer {
	t.Helper()
	return NewTokenIssuer([]byte("test-signing-key-with-enough-entropy-for-hmac-sha256"), time.Hour)
}

func TestIssueAndParsePlatformToken(t *testing.T) {
	t.Parallel()

	issuer := testIssuer(t)
	subject := NewID()

	raw, issued, err := issuer.Issue(subject, AudiencePlatform, nil, false)
	require.NoError(t, err)
	assert.Equal(t, AudiencePlatform, issued.Audience)
	assert.Nil(t, issued.TenantID)

	parsed, err := issuer.Parse(raw)
	require.NoError(t, err)
	assert.Equal(t, subject, parsed.Subject)
	assert.Equal(t, AudiencePlatform, parsed.Audience)
	assert.Nil(t, parsed.TenantID, "a platform token must not carry a tenant")
	assert.False(t, parsed.PasswordChange)
	assert.WithinDuration(t, time.Now().Add(time.Hour), parsed.ExpiresAt, time.Minute)
}

func TestIssueAndParseTenantToken(t *testing.T) {
	t.Parallel()

	issuer := testIssuer(t)
	subject := NewID()
	tenantID := NewID()

	raw, _, err := issuer.Issue(subject, AudienceTenant, &tenantID, true)
	require.NoError(t, err)

	parsed, err := issuer.Parse(raw)
	require.NoError(t, err)
	assert.Equal(t, AudienceTenant, parsed.Audience)
	require.NotNil(t, parsed.TenantID)
	assert.Equal(t, tenantID, *parsed.TenantID)
	assert.True(t, parsed.PasswordChange, "the pending password change must survive the round trip")
}

func TestIssueRejectsInvalidCombinations(t *testing.T) {
	t.Parallel()

	issuer := testIssuer(t)

	t.Run("tenant audience without tenant id", func(t *testing.T) {
		t.Parallel()
		_, _, err := issuer.Issue(NewID(), AudienceTenant, nil, false)
		require.Error(t, err)
	})

	t.Run("unknown audience", func(t *testing.T) {
		t.Parallel()
		_, _, err := issuer.Issue(NewID(), Audience("root"), nil, false)
		require.Error(t, err)
	})
}

func TestParseRejectsTamperedOrForeignTokens(t *testing.T) {
	t.Parallel()

	issuer := testIssuer(t)
	raw, _, err := issuer.Issue(NewID(), AudiencePlatform, nil, false)
	require.NoError(t, err)

	t.Run("wrong signing key", func(t *testing.T) {
		t.Parallel()
		other := NewTokenIssuer([]byte("a-completely-different-signing-key-value-here"), time.Hour)
		_, err := other.Parse(raw)
		require.ErrorIs(t, err, ErrInvalidToken)
	})

	t.Run("tampered payload", func(t *testing.T) {
		t.Parallel()
		_, err := issuer.Parse(raw[:len(raw)-3] + "abc")
		require.ErrorIs(t, err, ErrInvalidToken)
	})

	t.Run("garbage", func(t *testing.T) {
		t.Parallel()
		_, err := issuer.Parse("not-a-token")
		require.ErrorIs(t, err, ErrInvalidToken)
	})
}

func TestParseRejectsExpiredToken(t *testing.T) {
	t.Parallel()

	issuer := NewTokenIssuer([]byte("test-signing-key-with-enough-entropy-for-hmac-sha256"), -time.Minute)
	raw, _, err := issuer.Issue(NewID(), AudiencePlatform, nil, false)
	require.NoError(t, err)

	_, err = issuer.Parse(raw)
	require.ErrorIs(t, err, ErrInvalidToken)
}

// A token signed with "alg": "none" must never be accepted: that is the classic
// JWT downgrade attack.
func TestParseRejectsUnsignedToken(t *testing.T) {
	t.Parallel()

	issuer := testIssuer(t)
	unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, jwtClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   NewID().String(),
			Audience:  jwt.ClaimStrings{string(AudiencePlatform)},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)

	_, err = issuer.Parse(unsigned)
	require.ErrorIs(t, err, ErrInvalidToken)
}

func TestParseRejectsPlatformTokenCarryingTenant(t *testing.T) {
	t.Parallel()

	issuer := testIssuer(t)
	forged, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwtClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   NewID().String(),
			Audience:  jwt.ClaimStrings{string(AudiencePlatform)},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		TenantID: NewID().String(),
	}).SignedString([]byte("test-signing-key-with-enough-entropy-for-hmac-sha256"))
	require.NoError(t, err)

	_, err = issuer.Parse(forged)
	require.ErrorIs(t, err, ErrInvalidToken)
}

func TestParseRejectsTenantTokenWithoutTenant(t *testing.T) {
	t.Parallel()

	issuer := testIssuer(t)
	forged, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwtClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   NewID().String(),
			Audience:  jwt.ClaimStrings{string(AudienceTenant)},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}).SignedString([]byte("test-signing-key-with-enough-entropy-for-hmac-sha256"))
	require.NoError(t, err)

	_, err = issuer.Parse(forged)
	require.ErrorIs(t, err, ErrInvalidToken)
}
