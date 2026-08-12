package domain

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// MinPasswordLength is the minimum accepted password size (FR-022).
const MinPasswordLength = 8

// argon2Params are the OWASP-recommended Argon2id parameters (research R1).
// They are encoded into every hash, so raising them later only requires a
// verify-then-rehash on the next successful login.
type argon2Params struct {
	memoryKiB   uint32
	iterations  uint32
	parallelism uint8
	saltLength  uint32
	keyLength   uint32
}

var defaultArgon2Params = argon2Params{
	memoryKiB:   64 * 1024,
	iterations:  1,
	parallelism: 4,
	saltLength:  16,
	keyLength:   32,
}

// ErrInvalidPasswordHash marks a stored hash that cannot be parsed. It signals
// data corruption, never a wrong password.
var ErrInvalidPasswordHash = errors.New("invalid password hash format")

// HashPassword derives an Argon2id hash and returns it in PHC string format:
// $argon2id$v=19$m=65536,t=1,p=4$<salt>$<hash>
func HashPassword(plain string) (string, error) {
	params := defaultArgon2Params

	salt := make([]byte, params.saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	key := argon2.IDKey([]byte(plain), salt, params.iterations, params.memoryKiB, params.parallelism, params.keyLength)

	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		params.memoryKiB, params.iterations, params.parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether plain matches the encoded PHC hash. The
// comparison is constant-time. A malformed hash returns ErrInvalidPasswordHash
// rather than a silent false, so corruption is not mistaken for a bad password.
func VerifyPassword(encoded, plain string) (bool, error) {
	params, salt, want, err := decodePHC(encoded)
	if err != nil {
		return false, err
	}

	got := argon2.IDKey([]byte(plain), salt, params.iterations, params.memoryKiB, params.parallelism, params.keyLength)

	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func decodePHC(encoded string) (argon2Params, []byte, []byte, error) {
	var params argon2Params

	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash]
	if len(parts) != 6 || parts[0] != "" {
		return params, nil, nil, ErrInvalidPasswordHash
	}
	if parts[1] != "argon2id" {
		return params, nil, nil, fmt.Errorf("%w: unsupported algorithm %q", ErrInvalidPasswordHash, parts[1])
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return params, nil, nil, ErrInvalidPasswordHash
	}
	if version != argon2.Version {
		return params, nil, nil, fmt.Errorf("%w: unsupported version %d", ErrInvalidPasswordHash, version)
	}

	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &params.memoryKiB, &params.iterations, &params.parallelism); err != nil {
		return params, nil, nil, ErrInvalidPasswordHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return params, nil, nil, ErrInvalidPasswordHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return params, nil, nil, ErrInvalidPasswordHash
	}

	params.saltLength = uint32(len(salt))
	params.keyLength = uint32(len(key))
	if params.keyLength == 0 || params.saltLength == 0 {
		return params, nil, nil, ErrInvalidPasswordHash
	}

	return params, salt, key, nil
}

// temporaryPasswordBytes yields a 24-character base64url secret, comfortably
// above MinPasswordLength and unguessable.
const temporaryPasswordBytes = 18

// GenerateTemporaryPassword produces the one-shot password handed to the
// super-admin on reset (US5). It is shown exactly once and never stored in clear.
func GenerateTemporaryPassword() (string, error) {
	buf := make([]byte, temporaryPasswordBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate temporary password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
