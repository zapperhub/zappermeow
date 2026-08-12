package httperr

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zapperhub/zappermeow/internal/domain"
)

func TestFromMapsEveryDomainCodeToItsContractStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"invalid credentials", domain.ErrInvalidCredentials(), http.StatusUnauthorized, "INVALID_CREDENTIALS"},
		{"unauthenticated", domain.ErrUnauthenticated(""), http.StatusUnauthorized, "UNAUTHENTICATED"},
		{"wrong audience", domain.ErrWrongAudience(), http.StatusForbidden, "WRONG_AUDIENCE"},
		{"tenant suspended", domain.ErrTenantSuspended(), http.StatusForbidden, "TENANT_SUSPENDED"},
		{"password change required", domain.ErrPasswordChangeRequired(), http.StatusForbidden, "PASSWORD_CHANGE_REQUIRED"},
		{"invalid current password", domain.ErrInvalidCurrentPassword(), http.StatusForbidden, "INVALID_CURRENT_PASSWORD"},
		{"not found", domain.ErrNotFound(), http.StatusNotFound, "RESOURCE_NOT_FOUND"},
		{"conflict", domain.ErrConflict("body.name", "already taken"), http.StatusConflict, "RESOURCE_CONFLICT"},
		{"validation", domain.ErrValidation("body.password", "too short"), http.StatusUnprocessableEntity, "VALIDATION_ERROR"},
		{"rate limited", domain.ErrRateLimited(), http.StatusTooManyRequests, "RATE_LIMIT_EXCEEDED"},
		{"internal", domain.ErrInternal(assert.AnError), http.StatusInternalServerError, "INTERNAL_ERROR"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			problem, ok := From(tc.err).(*ProblemDetail)
			require.True(t, ok)
			assert.Equal(t, tc.wantStatus, problem.Status)
			assert.Equal(t, tc.wantStatus, problem.GetStatus())
			assert.Equal(t, tc.wantCode, problem.Code)
			assert.NotEmpty(t, problem.Type)
			assert.NotEmpty(t, problem.Title)
			assert.False(t, problem.Timestamp.IsZero())
		})
	}
}

// An unexpected failure must surface as a bare 500 — no internals, no cause.
func TestFromHidesNonDomainErrors(t *testing.T) {
	t.Parallel()

	problem, ok := From(assert.AnError).(*ProblemDetail)
	require.True(t, ok)
	assert.Equal(t, http.StatusInternalServerError, problem.Status)
	assert.Equal(t, "INTERNAL_ERROR", problem.Code)
	assert.NotContains(t, problem.Detail, assert.AnError.Error())
}

func TestFromKeepsInternalCauseOutOfTheWire(t *testing.T) {
	t.Parallel()

	problem, ok := From(domain.ErrInternal(assert.AnError)).(*ProblemDetail)
	require.True(t, ok)

	encoded, err := json.Marshal(problem)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), assert.AnError.Error())
}

func TestFromCarriesFieldLocations(t *testing.T) {
	t.Parallel()

	err := domain.ErrValidationFields(
		domain.FieldError{Location: "body.email", Message: "must be a valid email"},
		domain.FieldError{Location: "body.new_password", Message: "expected length >= 8"},
	)

	problem, ok := From(err).(*ProblemDetail)
	require.True(t, ok)
	require.Len(t, problem.Errors, 2)
	assert.Equal(t, "body.email", problem.Errors[0].Location)
	assert.Equal(t, "expected length >= 8", problem.Errors[1].Message)
}

func TestProblemIsServedAsProblemJSON(t *testing.T) {
	t.Parallel()

	problem := &ProblemDetail{}
	assert.Equal(t, "application/problem+json", problem.ContentType("application/json"))
	assert.Equal(t, "application/problem+cbor", problem.ContentType("application/cbor"))
	assert.Equal(t, "text/plain", problem.ContentType("text/plain"))
}

func TestValidationTypeURIMatchesTheContract(t *testing.T) {
	t.Parallel()

	problem, ok := From(domain.ErrValidation("body.name", "required")).(*ProblemDetail)
	require.True(t, ok)
	assert.Equal(t, "https://zappermeow.dev/errors/validation", problem.Type)
}

// Errors huma raises on its own (body validation, bad routes) must carry the
// same extensions as ours, otherwise clients would meet two error shapes.
func TestInstallExtendsHumaErrors(t *testing.T) {
	Install()

	err := huma.NewError(http.StatusUnprocessableEntity, "validation failed",
		&huma.ErrorDetail{Message: "expected length >= 8", Location: "body.password", Value: "hunter2"})

	problem, ok := err.(*ProblemDetail)
	require.True(t, ok, "huma.NewError must produce our problem model")
	assert.Equal(t, "VALIDATION_ERROR", problem.Code)
	assert.False(t, problem.Timestamp.IsZero())
	require.Len(t, problem.Errors, 1)
	assert.Nil(t, problem.Errors[0].Value, "a rejected password must never be echoed back")
}

func TestInstallRedactsOnlySensitiveMembers(t *testing.T) {
	Install()

	err := huma.NewError(http.StatusUnprocessableEntity, "validation failed",
		&huma.ErrorDetail{Message: "too long", Location: "body.name", Value: "Alice"},
		&huma.ErrorDetail{Message: "too short", Location: "body.new_password", Value: "abc"},
		&huma.ErrorDetail{Message: "malformed", Location: "header.X-Api-Key", Value: "zmk_leak"},
	)

	problem, ok := err.(*ProblemDetail)
	require.True(t, ok)
	require.Len(t, problem.Errors, 3)
	assert.Equal(t, "Alice", problem.Errors[0].Value, "harmless values stay for debuggability")
	assert.Nil(t, problem.Errors[1].Value)
	assert.Nil(t, problem.Errors[2].Value)
}

// huma probes the constructor with status 0 while building the OpenAPI
// document; that path must not panic or produce an empty title.
func TestInstallSurvivesHumaSchemaProbe(t *testing.T) {
	Install()

	err := huma.NewError(0, "")
	problem, ok := err.(*ProblemDetail)
	require.True(t, ok)
	assert.Equal(t, "Error", problem.Title)
}

func TestEnvelopeCarriesNumericStatus(t *testing.T) {
	t.Parallel()

	response := OK(map[string]string{"hello": "world"})
	assert.Equal(t, http.StatusOK, response.Body.Status)
	assert.False(t, response.Body.Timestamp.IsZero())

	encoded, err := json.Marshal(response.Body)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assert.Equal(t, float64(200), decoded["status"], "status is the numeric HTTP code, never a state string")
	assert.Contains(t, decoded, "data")
	assert.Contains(t, decoded, "timestamp")

	created := Created(map[string]string{})
	assert.Equal(t, http.StatusCreated, created.Body.Status)
}

// A refusal that happens before any handler runs — wrong media type, unparsable
// body — reports the offending payload as a whole under location "body". That
// payload is very often a login request, so the value must be dropped: matching
// on the location alone would let a password through.
func TestInstallRedactsWholeBodyValuesCarryingCredentials(t *testing.T) {
	Install()

	err := huma.NewError(http.StatusUnsupportedMediaType, "validation failed",
		&huma.ErrorDetail{
			Message:  "unknown content type: application/x-www-form-urlencoded",
			Location: "body",
			Value:    `{"email":"root@example.com","password":"bootstrap-secret-1"}`,
		})

	problem, ok := err.(*ProblemDetail)
	require.True(t, ok)
	require.Len(t, problem.Errors, 1)
	assert.Nil(t, problem.Errors[0].Value, "a raw body holding a password must never be echoed")

	encoded, marshalErr := json.Marshal(problem)
	require.NoError(t, marshalErr)
	assert.NotContains(t, string(encoded), "bootstrap-secret-1")
}

// A body with nothing sensitive in it stays visible, because echoing what was
// received is genuinely useful when debugging a malformed request.
func TestInstallKeepsHarmlessBodyValues(t *testing.T) {
	Install()

	err := huma.NewError(http.StatusBadRequest, "validation failed",
		&huma.ErrorDetail{Message: "unparsable", Location: "body", Value: `{"name":"ACME"}`})

	problem, ok := err.(*ProblemDetail)
	require.True(t, ok)
	require.Len(t, problem.Errors, 1)
	assert.Equal(t, `{"name":"ACME"}`, problem.Errors[0].Value)
}

// Every 4xx must carry a client-error code. Reporting INTERNAL_ERROR for a
// malformed request sends the caller hunting for a fault that is not ours.
func TestProtocolLevelStatusesGetClientErrorCodes(t *testing.T) {
	Install()

	tests := []struct {
		status   int
		wantCode string
	}{
		{http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE"},
		{http.StatusNotAcceptable, "UNSUPPORTED_MEDIA_TYPE"},
		{http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED"},
		{http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE"},
		{http.StatusRequestTimeout, "BAD_REQUEST"},
		{http.StatusInternalServerError, "INTERNAL_ERROR"},
		{http.StatusBadGateway, "INTERNAL_ERROR"},
	}

	for _, tc := range tests {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			problem, ok := huma.NewError(tc.status, "boom").(*ProblemDetail)
			require.True(t, ok)
			assert.Equal(t, tc.wantCode, problem.Code)
			assert.NotEmpty(t, problem.Type)
		})
	}
}
