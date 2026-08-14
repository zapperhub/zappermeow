package domain

import (
	"errors"
	"fmt"
)

// Code is the stable, machine-readable error identifier published to clients.
// Changing or removing a published code is a breaking API change.
type Code string

const (
	CodeInvalidCredentials     Code = "INVALID_CREDENTIALS"
	CodeUnauthenticated        Code = "UNAUTHENTICATED"
	CodeWrongAudience          Code = "WRONG_AUDIENCE"
	CodeTenantSuspended        Code = "TENANT_SUSPENDED"
	CodePasswordChangeRequired Code = "PASSWORD_CHANGE_REQUIRED"
	CodeInvalidCurrentPassword Code = "INVALID_CURRENT_PASSWORD"
	CodeResourceNotFound       Code = "RESOURCE_NOT_FOUND"
	CodeResourceConflict       Code = "RESOURCE_CONFLICT"
	CodeValidation             Code = "VALIDATION_ERROR"
	CodeRateLimitExceeded      Code = "RATE_LIMIT_EXCEEDED"
	CodeInternal               Code = "INTERNAL_ERROR"

	// Connection codes (feature 002). Each is contract: clients branch on them.
	CodeInstanceNotPaired   Code = "INSTANCE_NOT_PAIRED"
	CodeAlreadyPaired       Code = "ALREADY_PAIRED"
	CodePairingInProgress   Code = "PAIRING_IN_PROGRESS"
	CodeInvalidPhoneNumber  Code = "INVALID_PHONE_NUMBER"
	CodeSessionUnavailable  Code = "SESSION_UNAVAILABLE"
	CodeWhatsAppUnavailable Code = "WHATSAPP_UNAVAILABLE"
	CodeSessionNotRunning   Code = "SESSION_NOT_RUNNING"

	// Connection extras (feature 003).
	CodeInvalidProxyURL        Code = "INVALID_PROXY_URL"
	CodeUnsupportedProxyScheme Code = "UNSUPPORTED_PROXY_SCHEME"
	CodeNoPasskeyChallenge     Code = "NO_PASSKEY_CHALLENGE"
	CodeNoPasskeyCode          Code = "NO_PASSKEY_CODE"
	CodeInstanceNotConnected   Code = "INSTANCE_NOT_CONNECTED"
	CodeIdentityNotResolvable  Code = "IDENTITY_NOT_RESOLVABLE"
	CodeInvalidContact         Code = "INVALID_CONTACT"
	CodeCannotVerifySelf       Code = "CANNOT_VERIFY_SELF"
	CodeContactUnavailable     Code = "CONTACT_UNAVAILABLE"

	// Codes below cover the protocol-level refusals the framework raises before
	// a handler is ever reached. They are part of the published catalogue: a
	// client must be able to tell "you sent the wrong media type" apart from
	// "something broke on our side".
	CodeUnsupportedMediaType Code = "UNSUPPORTED_MEDIA_TYPE"
	CodeMethodNotAllowed     Code = "METHOD_NOT_ALLOWED"
	CodeRequestTooLarge      Code = "REQUEST_TOO_LARGE"
	CodeBadRequest           Code = "BAD_REQUEST"
)

// FieldError points at the request member that violated a rule. Location uses
// the same dotted notation as the API contract (for example "body.new_password").
type FieldError struct {
	Location string
	Message  string
}

// Error is a transport-agnostic domain error. The httperr package is solely
// responsible for turning it into an RFC 9457 problem document, which keeps the
// domain free of HTTP concerns.
type Error struct {
	Code   Code
	Detail string
	Fields []FieldError
	cause  error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Detail, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func (e *Error) Unwrap() error { return e.cause }

// WithCause attaches the underlying failure for logging. The cause is never
// serialised to clients.
func (e *Error) WithCause(err error) *Error {
	e.cause = err
	return e
}

// AsError extracts a *Error from an error chain.
func AsError(err error) (*Error, bool) {
	var domainErr *Error
	if errors.As(err, &domainErr) {
		return domainErr, true
	}
	return nil, false
}

// HasCode reports whether err carries the given domain code.
func HasCode(err error, code Code) bool {
	domainErr, ok := AsError(err)
	return ok && domainErr.Code == code
}

// ErrInvalidCredentials is the single, deliberately generic answer to every
// login failure — unknown email, wrong password or a locked account — so the
// response cannot be used to enumerate accounts (FR-019).
func ErrInvalidCredentials() *Error {
	return &Error{Code: CodeInvalidCredentials, Detail: "Invalid email or password"}
}

// ErrUnauthenticated covers a missing, malformed, expired or revoked credential.
func ErrUnauthenticated(detail string) *Error {
	if detail == "" {
		detail = "Authentication required"
	}
	return &Error{Code: CodeUnauthenticated, Detail: detail}
}

// ErrWrongAudience is returned when a valid token targets the other plane.
func ErrWrongAudience() *Error {
	return &Error{Code: CodeWrongAudience, Detail: "Token audience is not valid for this route"}
}

// ErrTenantSuspended is only ever revealed to a caller that already proved
// possession of a valid credential.
func ErrTenantSuspended() *Error {
	return &Error{Code: CodeTenantSuspended, Detail: "Tenant is suspended"}
}

// ErrPasswordChangeRequired blocks every route but the password change while a
// temporary password is pending.
func ErrPasswordChangeRequired() *Error {
	return &Error{Code: CodePasswordChangeRequired, Detail: "A password change is required before any other operation"}
}

// ErrInvalidCurrentPassword rejects a password change with a wrong current password.
func ErrInvalidCurrentPassword() *Error {
	return &Error{Code: CodeInvalidCurrentPassword, Detail: "Current password is incorrect"}
}

// ErrNotFound is returned both for resources that never existed and for
// resources owned by another tenant or instance, so the two are indistinguishable.
func ErrNotFound() *Error {
	return &Error{Code: CodeResourceNotFound, Detail: "Resource not found"}
}

// ErrConflict reports a uniqueness violation on the named request member.
func ErrConflict(location, message string) *Error {
	return &Error{
		Code:   CodeResourceConflict,
		Detail: "Resource conflicts with an existing one",
		Fields: []FieldError{{Location: location, Message: message}},
	}
}

// ErrValidation reports one violated input rule.
func ErrValidation(location, message string) *Error {
	return &Error{
		Code:   CodeValidation,
		Detail: "Request validation failed",
		Fields: []FieldError{{Location: location, Message: message}},
	}
}

// ErrValidationFields reports several violated input rules at once.
func ErrValidationFields(fields ...FieldError) *Error {
	return &Error{Code: CodeValidation, Detail: "Request validation failed", Fields: fields}
}

// ErrRateLimited reports an exceeded GCRA allowance.
func ErrRateLimited() *Error {
	return &Error{Code: CodeRateLimitExceeded, Detail: "Rate limit exceeded"}
}

// ErrInternal wraps an unexpected failure; the cause never reaches the client.
func ErrInternal(cause error) *Error {
	return &Error{Code: CodeInternal, Detail: "Internal server error", cause: cause}
}

// ErrInstanceNotPaired rejects an operation that needs saved session material
// on an instance that has none.
func ErrInstanceNotPaired() *Error {
	return &Error{Code: CodeInstanceNotPaired, Detail: "Instance is not paired with WhatsApp"}
}

// ErrAlreadyPaired rejects a pairing attempt on an instance that already holds
// session material; logging out first is the deliberate step.
func ErrAlreadyPaired() *Error {
	return &Error{Code: CodeAlreadyPaired, Detail: "Instance already has a paired device"}
}

// ErrPairingInProgress reports an attempt already in flight when the caller
// asked not to replace it.
func ErrPairingInProgress() *Error {
	return &Error{Code: CodePairingInProgress, Detail: "A pairing attempt is already in progress"}
}

// ErrInvalidPhoneNumber rejects a number WhatsApp would refuse anyway.
func ErrInvalidPhoneNumber() *Error {
	return &Error{
		Code:   CodeInvalidPhoneNumber,
		Detail: "Phone number must be in international format without a leading plus or zero",
		Fields: []FieldError{{Location: "body.phone_number", Message: "invalid international phone number"}},
	}
}

// ErrSessionUnavailable reports that no worker could serve the command right
// now — a deploy, a failover, or a fleet with no spare capacity.
func ErrSessionUnavailable() *Error {
	return &Error{Code: CodeSessionUnavailable, Detail: "No session worker is available to serve this command"}
}

// ErrInvalidProxyURL rejects an address the proxy dialer could never parse.
// The offending value is not echoed: it may carry a password.
func ErrInvalidProxyURL() *Error {
	return &Error{
		Code:   CodeInvalidProxyURL,
		Detail: "Proxy URL must be a valid absolute URL with a host",
		Fields: []FieldError{{Location: "body.url", Message: "invalid proxy URL"}},
	}
}

// ErrUnsupportedProxyScheme rejects a scheme the library cannot dial.
func ErrUnsupportedProxyScheme() *Error {
	return &Error{
		Code:   CodeUnsupportedProxyScheme,
		Detail: "Proxy scheme must be one of http, https or socks5",
		Fields: []FieldError{{Location: "body.url", Message: "unsupported proxy scheme"}},
	}
}

// ErrNoPasskeyChallenge rejects an assertion when no passkey step is waiting
// for one — including a challenge that was already answered.
func ErrNoPasskeyChallenge() *Error {
	return &Error{Code: CodeNoPasskeyChallenge, Detail: "No passkey challenge is pending for this instance"}
}

// ErrNoPasskeyCode rejects a confirmation sent out of order: before the code
// exists, after the step was consumed, or when the library already confirmed
// automatically.
func ErrNoPasskeyCode() *Error {
	return &Error{Code: CodeNoPasskeyCode, Detail: "No passkey confirmation code is pending for this instance"}
}

// ErrInstanceNotConnected rejects an operation that needs a live session.
func ErrInstanceNotConnected() *Error {
	return &Error{Code: CodeInstanceNotConnected, Detail: "Instance must be connected to serve this request"}
}

// ErrIdentityNotResolvable reports a phone number the instance has never seen
// mapped to a LID. Guessing an identity here would produce a verification code
// for the wrong person, which is worse than failing.
func ErrIdentityNotResolvable() *Error {
	return &Error{
		Code:   CodeIdentityNotResolvable,
		Detail: "No known LID mapping for the given phone number",
		Fields: []FieldError{{Location: "query.contact", Message: "identity not resolvable"}},
	}
}

// ErrInvalidContact rejects a contact that is neither a LID nor a phone number.
func ErrInvalidContact() *Error {
	return &Error{
		Code:   CodeInvalidContact,
		Detail: "Contact must be a LID or an international phone number",
		Fields: []FieldError{{Location: "query.contact", Message: "invalid contact"}},
	}
}

// ErrCannotVerifySelf rejects verifying the instance's own identity: the
// feature exists to check who is on the other side of a conversation.
func ErrCannotVerifySelf() *Error {
	return &Error{
		Code:   CodeCannotVerifySelf,
		Detail: "Identity verification codes describe a contact, not the instance itself",
		Fields: []FieldError{{Location: "query.contact", Message: "contact is the instance itself"}},
	}
}

// ErrContactUnavailable reports a contact WhatsApp knows nothing about, or one
// with no active devices to derive an identity from.
func ErrContactUnavailable() *Error {
	return &Error{Code: CodeContactUnavailable, Detail: "Contact is unknown to WhatsApp or has no active devices"}
}

// ErrWhatsAppUnavailable reports that the command reached a worker but WhatsApp
// itself failed or refused. Distinct from ErrSessionUnavailable on purpose: one
// says the platform has no capacity, the other says the upstream is the problem,
// and conflating them sends the tenant looking in the wrong place.
func ErrWhatsAppUnavailable() *Error {
	return &Error{Code: CodeWhatsAppUnavailable, Detail: "WhatsApp did not accept the command"}
}

// ErrSessionNotRunning reports that the worker holding the lease has no session
// in memory for this instance — a stale ownership record. Reconnecting is the
// way out, so the tenant is told to retry rather than left guessing.
func ErrSessionNotRunning() *Error {
	return &Error{
		Code:   CodeSessionNotRunning,
		Detail: "The session is not running on the worker that owns it; connect again to restart it",
	}
}
