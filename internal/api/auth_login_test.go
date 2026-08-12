package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zapperhub/zappermeow/internal/config"
	"github.com/zapperhub/zappermeow/internal/store/testutil"
)

// US1 scenario 1: a clean install with a configured credential creates the
// super-admin and lets it log in.
func TestBootstrapCreatesSuperAdminOnCleanInstall(t *testing.T) {
	f := newFixture(t)

	token := f.platformToken()
	assert.NotEmpty(t, token)
	assert.Equal(t, int64(1), f.countEvents("bootstrap_admin_created"))
}

// US1 scenario 2: restarting with the credential still configured must not
// create a second super-admin nor disturb the existing one.
func TestBootstrapIsIdempotentAcrossRestarts(t *testing.T) {
	f := newFixture(t)

	// A restart against the same database, with the bootstrap variables changed.
	restarted := bootApplication(t, f.infra, func(c *config.Config) {
		c.BootstrapEmail = "other-root@example.com"
		c.BootstrapPassword = "another-bootstrap-secret"
	})

	count, err := f.infra.Queries.CountSuperAdmins(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "the bootstrap must not create a second super-admin")
	assert.Equal(t, int64(1), f.countEvents("bootstrap_admin_created"))

	// The original credential still works, the new one was ignored entirely.
	assert.NotEmpty(t, restarted.login(bootstrapEmail, bootstrapPassword))
	restarted.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": "other-root@example.com", "password": "another-bootstrap-secret",
	}}).problem(http.StatusUnauthorized, "INVALID_CREDENTIALS")
}

// Spec edge case: without a credential and without a super-admin the service
// still starts, but says loudly that nobody can administer it.
func TestBootstrapWithoutCredentialsWarnsAndStarts(t *testing.T) {
	infra := testutil.Shared(t)
	infra.Reset(t)

	f := bootApplication(t, infra, func(c *config.Config) {
		c.BootstrapEmail = ""
		c.BootstrapPassword = ""
	})

	count, err := infra.Queries.CountSuperAdmins(context.Background())
	require.NoError(t, err)
	assert.Zero(t, count)
	assert.Contains(t, f.logs.String(), "no administrative access is possible")

	// The service is up regardless.
	f.do(request{method: http.MethodGet, path: "/healthz"}).data(http.StatusOK, nil)
}

// US1 scenario 3: the super-admin receives a platform-audience token.
func TestLoginIssuesPlatformAudienceForSuperAdmin(t *testing.T) {
	f := newFixture(t)

	var data struct {
		AccessToken        string `json:"access_token"`
		TokenType          string `json:"token_type"`
		ExpiresIn          int    `json:"expires_in"`
		Audience           string `json:"audience"`
		MustChangePassword bool   `json:"must_change_password"`
	}
	f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": bootstrapEmail, "password": bootstrapPassword,
	}}).data(http.StatusOK, &data)

	assert.Equal(t, "platform", data.Audience)
	assert.Equal(t, "Bearer", data.TokenType)
	assert.Equal(t, 3600, data.ExpiresIn)
	assert.False(t, data.MustChangePassword)
	assert.Equal(t, int64(1), f.countEvents("login_succeeded"))
}

// FR-019: an unknown email and a wrong password must be indistinguishable.
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	f := newFixture(t)

	unknown := f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": "nobody@example.com", "password": bootstrapPassword,
	}})
	wrongPassword := f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": bootstrapEmail, "password": "not-the-password",
	}})

	unknownDoc := unknown.problem(http.StatusUnauthorized, "INVALID_CREDENTIALS")
	wrongDoc := wrongPassword.problem(http.StatusUnauthorized, "INVALID_CREDENTIALS")

	assert.Equal(t, unknown.Status, wrongPassword.Status)
	assert.Equal(t, unknownDoc.Detail, wrongDoc.Detail)
	assert.Equal(t, unknownDoc.Title, wrongDoc.Title)
	assert.Equal(t, unknownDoc.Type, wrongDoc.Type)
	assert.Empty(t, unknownDoc.Errors, "a login failure must not point at a field")
	assert.Empty(t, wrongDoc.Errors)
}

// A failed login is auditable even when the email belongs to nobody.
func TestLoginFailureIsRecorded(t *testing.T) {
	f := newFixture(t)

	f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": "nobody@example.com", "password": "whatever",
	}}).problem(http.StatusUnauthorized, "INVALID_CREDENTIALS")

	assert.Equal(t, int64(1), f.countEvents("login_failed"))
}

// The response of a successful login must not leak credential material.
func TestLoginResponseCarriesNoSecrets(t *testing.T) {
	f := newFixture(t)

	response := f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": bootstrapEmail, "password": bootstrapPassword,
	}})

	body := string(response.Body)
	assert.NotContains(t, body, bootstrapPassword)
	assert.NotContains(t, body, "argon2id")
	assert.NotContains(t, body, "password_hash")
}

// The password must never reach the log stream either (SC-006).
func TestLoginDoesNotLogPassword(t *testing.T) {
	f := newFixture(t)
	f.platformToken()

	assert.NotContains(t, f.logs.String(), bootstrapPassword)
	assert.NotContains(t, f.logs.String(), "argon2id")
}

func TestHealthAndMetricsNeedNoCredential(t *testing.T) {
	f := newFixture(t)

	var health struct {
		Status string `json:"status"`
	}
	healthResponse := f.do(request{method: http.MethodGet, path: "/healthz"})
	healthResponse.data(http.StatusOK, &health)
	assert.Equal(t, "ok", health.Status)
	assertEnvelopeMembers(t, healthResponse.Body)

	metrics := f.do(request{method: http.MethodGet, path: "/metrics"})
	assert.Equal(t, http.StatusOK, metrics.Status)
	assert.Contains(t, string(metrics.Body), "zappermeow_http_request_duration_seconds")
}

// FR-023: the contract is generated from the code and served by the API itself.
func TestOpenAPIDocumentIsServed(t *testing.T) {
	f := newFixture(t)

	response := f.do(request{method: http.MethodGet, path: "/openapi.json"})
	require.Equal(t, http.StatusOK, response.Status)

	body := string(response.Body)
	assert.Contains(t, body, `"openapi":"3.1`)
	assert.Contains(t, body, "/auth/login")
	assert.Contains(t, body, "/admin/tenants")
	assert.Contains(t, body, "bearerAuth")
	assert.Contains(t, body, "apiKeyAuth")
	assert.Contains(t, body, "VALIDATION_ERROR", "the error model must document the stable code member")
}
