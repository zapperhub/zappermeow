package domain

import (
	"fmt"
	"net/mail"
	"strings"
	"unicode/utf8"
)

// Name length bounds shared by tenants, users and instances (FR-022).
const (
	MinNameLength = 1
	MaxNameLength = 120
	MaxLabelLen   = 60
)

// NormalizeName trims surrounding whitespace. Names are stored as typed, minus
// the padding, so " ACME " and "ACME" cannot coexist as different tenants.
func NormalizeName(name string) string { return strings.TrimSpace(name) }

// NormalizeEmail trims and lowercases. The column is citext, so this only keeps
// the stored form tidy — uniqueness is case-insensitive either way.
func NormalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// ValidateName checks a display name against the shared length rules, reporting
// the offending request member.
func ValidateName(location, name string) error {
	trimmed := NormalizeName(name)
	if trimmed == "" {
		return ErrValidation(location, "must not be empty")
	}
	if length := utf8.RuneCountInString(trimmed); length > MaxNameLength {
		return ErrValidation(location, fmt.Sprintf("expected length <= %d, got %d", MaxNameLength, length))
	}
	return nil
}

// ValidateEmail checks the address format.
func ValidateEmail(location, email string) error {
	trimmed := NormalizeEmail(email)
	if trimmed == "" {
		return ErrValidation(location, "must not be empty")
	}
	address, err := mail.ParseAddress(trimmed)
	if err != nil || address.Address != trimmed {
		return ErrValidation(location, "must be a valid email address")
	}
	return nil
}

// ValidatePassword enforces the minimum length. The value is never echoed back
// in the error (SC-006).
func ValidatePassword(location, password string) error {
	if length := utf8.RuneCountInString(password); length < MinPasswordLength {
		return ErrValidation(location, fmt.Sprintf("expected length >= %d", MinPasswordLength))
	}
	return nil
}

// ValidateLabel checks the optional API key label.
func ValidateLabel(location, label string) error {
	if length := utf8.RuneCountInString(strings.TrimSpace(label)); length > MaxLabelLen {
		return ErrValidation(location, fmt.Sprintf("expected length <= %d, got %d", MaxLabelLen, length))
	}
	return nil
}

// CollectErrors merges several validation failures into a single response so a
// client fixes every problem in one round trip, never leaving partial state.
func CollectErrors(errs ...error) error {
	var fields []FieldError
	for _, err := range errs {
		if err == nil {
			continue
		}
		domainErr, ok := AsError(err)
		if !ok {
			return err
		}
		fields = append(fields, domainErr.Fields...)
	}
	if len(fields) == 0 {
		return nil
	}
	return ErrValidationFields(fields...)
}
