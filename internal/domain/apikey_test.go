package domain

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateAPIKeyShape(t *testing.T) {
	t.Parallel()

	generated, err := GenerateAPIKey()
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(generated.Secret, APIKeyPrefix),
		"the prefix is what lets secret scanners spot a leaked key")
	assert.Len(t, generated.Secret, len(APIKeyPrefix)+apiKeySecretChars,
		"every key must be the same length so the stored prefix always covers the same portion")
	assert.Len(t, generated.Prefix, APIKeyPrefixLength)
	assert.Equal(t, generated.Secret[:APIKeyPrefixLength], generated.Prefix)
	assert.Len(t, generated.Hash, 32, "SHA-256 produces 32 bytes")
	assert.True(t, LooksLikeAPIKey(generated.Secret))
}

func TestGenerateAPIKeyIsUnique(t *testing.T) {
	t.Parallel()

	seen := make(map[string]struct{}, 200)
	hashes := make(map[string]struct{}, 200)
	for range 200 {
		generated, err := GenerateAPIKey()
		require.NoError(t, err)

		_, duplicate := seen[generated.Secret]
		require.False(t, duplicate, "generated the same secret twice")
		seen[generated.Secret] = struct{}{}

		digest := hex.EncodeToString(generated.Hash)
		_, duplicateHash := hashes[digest]
		require.False(t, duplicateHash, "generated the same digest twice")
		hashes[digest] = struct{}{}
	}
}

// Only the digest is ever stored, and it must be reproducible from the token
// presented in a request — that is the whole verification path.
func TestHashAPIKeyIsDeterministicAndVerifiable(t *testing.T) {
	t.Parallel()

	generated, err := GenerateAPIKey()
	require.NoError(t, err)

	assert.Equal(t, generated.Hash, HashAPIKey(generated.Secret))
	assert.True(t, APIKeyMatches(HashAPIKey(generated.Secret), generated.Hash))

	other, err := GenerateAPIKey()
	require.NoError(t, err)
	assert.False(t, APIKeyMatches(HashAPIKey(other.Secret), generated.Hash))
}

func TestAPIKeyMatchesRejectsMismatchedLengths(t *testing.T) {
	t.Parallel()

	generated, err := GenerateAPIKey()
	require.NoError(t, err)

	assert.False(t, APIKeyMatches(nil, generated.Hash))
	assert.False(t, APIKeyMatches([]byte("short"), generated.Hash))
}

func TestLooksLikeAPIKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		token string
		want  bool
	}{
		{name: "well formed", token: "zmk_a1b2c3d4e5f6g7h8", want: true},
		{name: "prefix only", token: "zmk_", want: false},
		{name: "wrong prefix", token: "sk_a1b2c3d4e5f6", want: false},
		{name: "empty", token: "", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, LooksLikeAPIKey(tc.token))
		})
	}
}

// The generated alphabet must stay free of characters that would break a shell,
// a URL or an HTTP header.
func TestGeneratedSecretUsesSafeAlphabet(t *testing.T) {
	t.Parallel()

	for range 50 {
		generated, err := GenerateAPIKey()
		require.NoError(t, err)

		body := strings.TrimPrefix(generated.Secret, APIKeyPrefix)
		for _, char := range body {
			assert.True(t, strings.ContainsRune(base62Alphabet, char),
				"unexpected character %q in %q", char, generated.Secret)
		}
	}
}
