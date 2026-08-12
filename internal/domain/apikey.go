package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// APIKeyPrefix marks a zappermeow key. A fixed, searchable prefix is what lets
// secret scanners recognise a leaked credential in a repository or a log.
const APIKeyPrefix = "zmk_"

// apiKeySecretBytes is the entropy behind a key: 256 bits from crypto/rand.
const apiKeySecretBytes = 32

// apiKeySecretChars is how many base62 digits it takes to carry those 256 bits:
// 62^43 is just above 2^256, so 43 digits represent every possible value.
const apiKeySecretChars = 43

// APIKeyPrefixLength is how much of the token is stored in clear and shown in
// listings, enough for an admin to tell two keys apart.
const APIKeyPrefixLength = 12

// base62Alphabet avoids punctuation so a key survives shells, URLs and headers.
const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// APIKeyStatus is the lifecycle state of a key.
type APIKeyStatus string

const (
	// APIKeyActive keys authenticate operational routes.
	APIKeyActive APIKeyStatus = "active"
	// APIKeyRevoked is terminal: a revoked key is never reactivated.
	APIKeyRevoked APIKeyStatus = "revoked"
)

// APIKey is the metadata of an operational credential. The secret itself is not
// a field: it exists only in the response that creates it.
type APIKey struct {
	ID         ID
	InstanceID ID
	Label      *string
	KeyPrefix  string
	Status     APIKeyStatus
	CreatedAt  time.Time
	RevokedAt  *time.Time
}

// GeneratedAPIKey is a freshly minted credential: the plaintext to hand over
// once, and the material that is safe to store.
type GeneratedAPIKey struct {
	// Secret is the only time the full token exists. It is returned to the
	// caller and then dropped; it is never stored, logged or recoverable.
	Secret string
	Prefix string
	Hash   []byte
}

// GenerateAPIKey mints a key: 256 random bits rendered in base62 behind the
// zmk_ prefix, stored as a SHA-256 digest.
//
// A slow KDF (Argon2, bcrypt) would be wrong here, not just expensive: it
// protects low-entropy human passwords against offline guessing. A 256-bit
// random secret cannot be guessed, and the digest sits on the hot path of every
// operational request (research R2).
func GenerateAPIKey() (GeneratedAPIKey, error) {
	secret, err := randomBase62()
	if err != nil {
		return GeneratedAPIKey{}, err
	}

	token := APIKeyPrefix + secret
	digest := HashAPIKey(token)

	return GeneratedAPIKey{
		Secret: token,
		Prefix: token[:APIKeyPrefixLength],
		Hash:   digest,
	}, nil
}

// HashAPIKey derives the stored verification material for a token.
func HashAPIKey(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// APIKeyMatches compares two digests in constant time.
func APIKeyMatches(candidate, stored []byte) bool {
	return subtle.ConstantTimeCompare(candidate, stored) == 1
}

// LooksLikeAPIKey reports whether a value has the shape of one of our keys. It
// is a cheap filter, never a substitute for verifying the digest.
func LooksLikeAPIKey(token string) bool {
	return strings.HasPrefix(token, APIKeyPrefix) && len(token) > APIKeyPrefixLength
}

// randomBase62 draws 256 random bits and renders them as a fixed-width base62
// string. Fixed width matters: it keeps every key the same length, so the
// stored prefix always covers the same portion of the token.
func randomBase62() (string, error) {
	buf := make([]byte, apiKeySecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate api key: %w", err)
	}

	value := new(big.Int).SetBytes(buf)
	base := big.NewInt(int64(len(base62Alphabet)))
	remainder := new(big.Int)

	out := make([]byte, apiKeySecretChars)
	for i := apiKeySecretChars - 1; i >= 0; i-- {
		value.DivMod(value, base, remainder)
		out[i] = base62Alphabet[remainder.Int64()]
	}
	return string(out), nil
}
