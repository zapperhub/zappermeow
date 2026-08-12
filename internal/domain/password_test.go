package domain

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHashPasswordProducesPHCFormat(t *testing.T) {
	t.Parallel()

	encoded, err := HashPassword("correct horse battery staple")
	require.NoError(t, err)

	parts := strings.Split(encoded, "$")
	require.Len(t, parts, 6, "PHC string must have 5 segments: %q", encoded)
	assert.Equal(t, "", parts[0])
	assert.Equal(t, "argon2id", parts[1])
	assert.Equal(t, "v=19", parts[2])
	assert.Equal(t, "m=65536,t=1,p=4", parts[3], "OWASP parameters must be encoded in the hash")
	assert.NotEmpty(t, parts[4])
	assert.NotEmpty(t, parts[5])
}

func TestHashPasswordIsSaltedPerCall(t *testing.T) {
	t.Parallel()

	first, err := HashPassword("same-password")
	require.NoError(t, err)
	second, err := HashPassword("same-password")
	require.NoError(t, err)

	assert.NotEqual(t, first, second, "each hash must use a fresh salt")
}

func TestVerifyPassword(t *testing.T) {
	t.Parallel()

	encoded, err := HashPassword("s3cr3t-password")
	require.NoError(t, err)

	tests := []struct {
		name      string
		candidate string
		want      bool
	}{
		{name: "exact match", candidate: "s3cr3t-password", want: true},
		{name: "wrong password", candidate: "s3cr3t-passwore", want: false},
		{name: "empty password", candidate: "", want: false},
		{name: "case differs", candidate: "S3CR3T-PASSWORD", want: false},
		{name: "prefix only", candidate: "s3cr3t", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ok, err := VerifyPassword(encoded, tc.candidate)
			require.NoError(t, err)
			assert.Equal(t, tc.want, ok)
		})
	}
}

func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		encoded string
	}{
		{name: "empty", encoded: ""},
		{name: "not phc", encoded: "plaintext"},
		{name: "too few segments", encoded: "$argon2id$v=19$m=65536,t=1,p=4$c2FsdA"},
		{name: "unsupported algorithm", encoded: "$bcrypt$v=19$m=65536,t=1,p=4$c2FsdA$aGFzaA"},
		{name: "unsupported version", encoded: "$argon2id$v=16$m=65536,t=1,p=4$c2FsdA$aGFzaA"},
		{name: "malformed parameters", encoded: "$argon2id$v=19$m=abc,t=1,p=4$c2FsdA$aGFzaA"},
		{name: "invalid base64 salt", encoded: "$argon2id$v=19$m=65536,t=1,p=4$!!!$aGFzaA"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ok, err := VerifyPassword(tc.encoded, "whatever")
			assert.False(t, ok)
			require.ErrorIs(t, err, ErrInvalidPasswordHash,
				"corruption must be distinguishable from a wrong password")
		})
	}
}

func TestGenerateTemporaryPassword(t *testing.T) {
	t.Parallel()

	first, err := GenerateTemporaryPassword()
	require.NoError(t, err)
	second, err := GenerateTemporaryPassword()
	require.NoError(t, err)

	assert.NotEqual(t, first, second)
	assert.GreaterOrEqual(t, len(first), MinPasswordLength,
		"a temporary password must satisfy the same rules as a chosen one")
}
