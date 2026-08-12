package domain

import "github.com/google/uuid"

// ID is the identifier type of every entity. UUID v7 values are time-ordered,
// which keeps primary-key indexes append-friendly without a Postgres extension.
type ID = uuid.UUID

// NewID returns a fresh UUID v7. It panics only if the system CSPRNG fails,
// which is unrecoverable and must not be papered over with a weaker identifier.
func NewID() ID {
	return uuid.Must(uuid.NewV7())
}

// ParseID parses a textual UUID, reporting a domain validation error against
// the given request member when it is malformed.
func ParseID(location, raw string) (ID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, ErrValidation(location, "must be a valid UUID")
	}
	return id, nil
}

// parseUUID parses a textual UUID without dressing the failure as a domain
// error; used where the caller supplies its own error semantics.
func parseUUID(raw string) (ID, error) {
	return uuid.Parse(raw)
}
