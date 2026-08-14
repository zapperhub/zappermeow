// Package httperr owns the wire format of the API: the success envelope and the
// RFC 9457 problem documents. It is the single place where domain errors become
// HTTP responses, so handlers never build a response by hand and the domain
// stays free of transport concerns.
package httperr

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/zapperhub/zappermeow/internal/domain"
)

// typeBase prefixes the documentation URI of every problem type.
const typeBase = "https://zappermeow.dev/errors/"

// nowFunc is swapped in tests to make timestamps deterministic.
var nowFunc = func() time.Time { return time.Now().UTC() }

// ProblemDetail is an RFC 9457 problem document extended with the stable `code`
// member and a `timestamp`, as mandated by the constitution.
type ProblemDetail struct {
	Type      string              `json:"type,omitempty" format:"uri" example:"https://zappermeow.dev/errors/validation" doc:"A URI reference to human-readable documentation for the error."`
	Title     string              `json:"title,omitempty" example:"Unprocessable Entity" doc:"A short, human-readable summary of the problem type."`
	Status    int                 `json:"status,omitempty" example:"422" doc:"HTTP status code."`
	Detail    string              `json:"detail,omitempty" example:"Request validation failed" doc:"A human-readable explanation specific to this occurrence of the problem."`
	Instance  string              `json:"instance,omitempty" format:"uri" doc:"A URI reference identifying this specific occurrence."`
	Code      string              `json:"code" example:"VALIDATION_ERROR" doc:"Stable, machine-readable error code. Clients branch on this value, never on the message."`
	Errors    []*huma.ErrorDetail `json:"errors,omitempty" doc:"Per-field details; each location points at the offending request member."`
	Timestamp time.Time           `json:"timestamp" doc:"When the error was produced (RFC 3339, UTC)."`
}

// Error implements the error interface with the developer-facing detail.
func (p *ProblemDetail) Error() string { return p.Detail }

// GetStatus lets huma set the response status from the returned error.
func (p *ProblemDetail) GetStatus() int { return p.Status }

// ContentType serves problem documents as application/problem+json.
func (p *ProblemDetail) ContentType(ct string) string {
	switch ct {
	case "application/json":
		return "application/problem+json"
	case "application/cbor":
		return "application/problem+cbor"
	}
	return ct
}

// Install replaces huma's error constructor so that every error the framework
// produces on its own — request validation, unparsable bodies, unknown routes —
// carries the same `code`/`timestamp` extensions as our domain errors. It also
// makes the generated OpenAPI document describe this exact model.
func Install() {
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		details := make([]*huma.ErrorDetail, 0, len(errs))
		for _, err := range errs {
			if err == nil {
				continue
			}
			if detailer, ok := err.(huma.ErrorDetailer); ok {
				details = append(details, redact(detailer.ErrorDetail()))
				continue
			}
			details = append(details, &huma.ErrorDetail{Message: err.Error()})
		}
		return newProblem(status, codeForStatus(status), msg, details)
	}
}

// From converts any error returned by a service into the problem document the
// client sees. Non-domain errors collapse into a generic 500 so internals never
// leak; the cause remains available to the caller for logging.
func From(err error) huma.StatusError {
	if err == nil {
		return nil
	}

	var problem *ProblemDetail
	if ok := asProblem(err, &problem); ok {
		return problem
	}

	domainErr, ok := domain.AsError(err)
	if !ok {
		return newProblem(http.StatusInternalServerError, domain.CodeInternal, "Internal server error", nil)
	}

	details := make([]*huma.ErrorDetail, 0, len(domainErr.Fields))
	for _, field := range domainErr.Fields {
		details = append(details, &huma.ErrorDetail{Message: field.Message, Location: field.Location})
	}
	return newProblem(statusFor(domainErr.Code), domainErr.Code, domainErr.Detail, details)
}

func asProblem(err error, out **ProblemDetail) bool {
	return errors.As(err, out)
}

func newProblem(status int, code domain.Code, detail string, details []*huma.ErrorDetail) *ProblemDetail {
	// huma probes the constructor with status 0 while building the OpenAPI
	// document; keep that path safe and meaningful.
	title := http.StatusText(status)
	if title == "" {
		title = "Error"
	}
	return &ProblemDetail{
		Type:      typeBase + typeSlug(code),
		Title:     title,
		Status:    status,
		Detail:    detail,
		Code:      string(code),
		Errors:    details,
		Timestamp: nowFunc(),
	}
}

// statusFor maps a domain code onto its HTTP status. The pairing is fixed by
// the API contract; a code always answers with the same status.
func statusFor(code domain.Code) int {
	switch code {
	case domain.CodeInvalidCredentials, domain.CodeUnauthenticated:
		return http.StatusUnauthorized
	case domain.CodeWrongAudience, domain.CodeTenantSuspended,
		domain.CodePasswordChangeRequired, domain.CodeInvalidCurrentPassword:
		return http.StatusForbidden
	case domain.CodeResourceNotFound:
		return http.StatusNotFound
	case domain.CodeResourceConflict:
		return http.StatusConflict
	case domain.CodeValidation:
		return http.StatusUnprocessableEntity
	case domain.CodeRateLimitExceeded:
		return http.StatusTooManyRequests
	case domain.CodeInstanceNotPaired, domain.CodeAlreadyPaired, domain.CodePairingInProgress:
		return http.StatusConflict
	case domain.CodeInvalidPhoneNumber:
		return http.StatusUnprocessableEntity
	case domain.CodeSessionUnavailable, domain.CodeSessionNotRunning:
		return http.StatusServiceUnavailable
	case domain.CodeWhatsAppUnavailable:
		return http.StatusBadGateway
	case domain.CodeInvalidProxyURL, domain.CodeUnsupportedProxyScheme,
		domain.CodeIdentityNotResolvable, domain.CodeInvalidContact,
		domain.CodeCannotVerifySelf:
		return http.StatusUnprocessableEntity
	case domain.CodeNoPasskeyChallenge, domain.CodeNoPasskeyCode,
		domain.CodeInstanceNotConnected:
		return http.StatusConflict
	case domain.CodeContactUnavailable:
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

// codeForStatus supplies the code for errors huma raises by itself. Anything in
// the 4xx range must report a client-error code: labelling a malformed request
// as INTERNAL_ERROR would send the caller looking for a fault on our side.
func codeForStatus(status int) domain.Code {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return domain.CodeValidation
	case http.StatusUnauthorized:
		return domain.CodeUnauthenticated
	case http.StatusForbidden:
		return domain.CodeWrongAudience
	case http.StatusNotFound:
		return domain.CodeResourceNotFound
	case http.StatusMethodNotAllowed:
		return domain.CodeMethodNotAllowed
	case http.StatusNotAcceptable, http.StatusUnsupportedMediaType:
		return domain.CodeUnsupportedMediaType
	case http.StatusRequestEntityTooLarge:
		return domain.CodeRequestTooLarge
	case http.StatusConflict:
		return domain.CodeResourceConflict
	case http.StatusTooManyRequests:
		return domain.CodeRateLimitExceeded
	}

	if status >= 400 && status < 500 {
		return domain.CodeBadRequest
	}
	return domain.CodeInternal
}

func typeSlug(code domain.Code) string {
	switch code {
	case domain.CodeValidation:
		return "validation"
	case domain.CodeResourceNotFound:
		return "not-found"
	case domain.CodeResourceConflict:
		return "conflict"
	case domain.CodeRateLimitExceeded:
		return "rate-limit"
	case domain.CodeInternal:
		return "internal"
	case domain.CodeUnsupportedMediaType:
		return "unsupported-media-type"
	case domain.CodeMethodNotAllowed:
		return "method-not-allowed"
	case domain.CodeRequestTooLarge:
		return "request-too-large"
	case domain.CodeBadRequest:
		return "bad-request"
	default:
		return strings.ToLower(strings.ReplaceAll(string(code), "_", "-"))
	}
}

// sensitiveMembers are request members whose value must never be echoed back,
// even inside a validation error (SC-006).
var sensitiveMembers = []string{"password", "secret", "token", "api_key", "apikey"}

// redact strips the offending value whenever it could carry credential material.
//
// Matching the location alone is not enough: an error raised against the body as
// a whole — an unsupported content type, unparsable JSON — reports location
// "body" and carries the entire raw payload as its value, credentials included.
// So the value itself is inspected too, and dropped when it so much as mentions
// a sensitive member.
func redact(detail *huma.ErrorDetail) *huma.ErrorDetail {
	if detail == nil {
		return nil
	}

	if isSensitive(detail.Location) {
		detail.Value = nil
		return detail
	}
	if detail.Value != nil && isSensitive(fmt.Sprint(detail.Value)) {
		detail.Value = nil
	}
	return detail
}

// isSensitive reports whether text names or contains a sensitive member.
// Hyphens are normalised so header names (X-Api-Key) and body members (api_key)
// are matched by the same list.
func isSensitive(text string) bool {
	normalised := strings.ReplaceAll(strings.ToLower(text), "-", "_")
	for _, member := range sensitiveMembers {
		if strings.Contains(normalised, member) {
			return true
		}
	}
	return false
}

// Write sends a domain error as a problem document from inside a middleware,
// where there is no typed handler to return it from. It performs the same
// content negotiation as huma's own error path.
func Write(api huma.API, ctx huma.Context, err error) {
	problem, ok := From(err).(*ProblemDetail)
	if !ok {
		problem = newProblem(http.StatusInternalServerError, domain.CodeInternal, "Internal server error", nil)
	}

	contentType, negotiateErr := api.Negotiate(ctx.Header("Accept"))
	if negotiateErr != nil {
		contentType = "application/json"
	}
	contentType = problem.ContentType(contentType)

	ctx.SetHeader("Content-Type", contentType)
	ctx.SetStatus(problem.Status)
	_ = api.Marshal(ctx.BodyWriter(), contentType, problem)
}
