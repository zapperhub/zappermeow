package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNoSecretEverReachesLogsOrResponses walks the whole feature end to end and
// then asserts that not one piece of credential material appears anywhere it
// should not: not in the log stream, not in the security-event trail, and not
// in any response beyond the single showing each secret is entitled to (SC-006).
//
// It is deliberately a sweep rather than a per-endpoint check: a leak added to
// a new code path is exactly the kind of regression a narrow test misses.
func TestNoSecretEverReachesLogsOrResponses(t *testing.T) {
	f := newFixture(t)

	// Every response body produced during the walk, so the audit can inspect
	// them all at the end.
	var bodies []string
	record := func(r *response) *response {
		bodies = append(bodies, string(r.Body))
		return r
	}

	// The full flow: bootstrap login, tenant, instance, key, operational call,
	// password change and reset.
	platformToken := f.platformToken()
	acme := f.newTenant(platformToken, "ACME Corp", "alice@acme.com", "senhaAlice1")
	instance := f.createInstance(acme.token, "vendas-01")
	key := f.createKey(acme.token, instance.ID, "produção")

	record(f.do(request{method: http.MethodGet, path: "/instances/" + instance.ID + "/whoami", apiKey: key.APIKey}))
	record(f.do(request{method: http.MethodGet, path: "/instances/" + instance.ID + "/keys", token: acme.token}))
	record(f.do(request{method: http.MethodGet, path: "/admin/tenants", token: platformToken}))
	record(f.do(request{method: http.MethodGet, path: "/admin/tenants/" + acme.tenant.ID, token: platformToken}))

	// A failed login and a rejected password, the paths most likely to echo an
	// attempted credential back.
	record(f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": "alice@acme.com", "password": "wrong-password-attempt",
	}}))
	record(f.do(request{method: http.MethodPost, path: "/auth/password", token: acme.token,
		body: map[string]string{"current_password": "senhaAlice1", "new_password": "tiny"}}))

	var reset resetPayload
	resetResponse := f.do(request{method: http.MethodPost,
		path: "/admin/tenants/" + acme.tenant.ID + "/admin/reset-password", token: platformToken})
	resetResponse.data(http.StatusOK, &reset)

	logs := f.logs.String()

	// Secrets that must appear in no log line at all.
	secrets := map[string]string{
		"bootstrap password":      bootstrapPassword,
		"tenant admin password":   "senhaAlice1",
		"attempted password":      "wrong-password-attempt",
		"rejected short password": "tiny",
		"api key secret":          key.APIKey,
		"temporary password":      reset.TemporaryPassword,
		"jwt signing key":         signingKey,
	}

	for name, secret := range secrets {
		t.Run("absent from logs: "+name, func(t *testing.T) {
			require.NotEmpty(t, secret)
			assert.NotContains(t, logs, secret)
		})
	}

	t.Run("no password hash is ever logged", func(t *testing.T) {
		assert.NotContains(t, logs, "argon2id")
		assert.NotContains(t, logs, "password_hash")
		assert.NotContains(t, logs, "secret_hash")
	})

	// The API key secret may appear exactly once across every response: in the
	// body that created it.
	t.Run("the api key secret appears in no later response", func(t *testing.T) {
		for i, body := range bodies {
			assert.NotContains(t, body, key.APIKey, "response %d repeated the key secret", i)
		}
	})

	t.Run("the temporary password appears only in the reset response", func(t *testing.T) {
		for i, body := range bodies {
			assert.NotContains(t, body, reset.TemporaryPassword, "response %d repeated the temporary password", i)
		}
		assert.Contains(t, string(resetResponse.Body), reset.TemporaryPassword,
			"the reset response is the one place it is allowed to appear")
	})

	// The identifying prefix, by contrast, is meant to be visible: without it
	// an operator could not tell which key an audit line refers to.
	t.Run("the key prefix stays visible for auditing", func(t *testing.T) {
		assert.Contains(t, logs, key.KeyPrefix)
		assert.True(t, strings.HasPrefix(key.KeyPrefix, "zmk_"))
	})
}

// The security-event trail is queried directly: metadata is written by hand at
// each call site, so it is the easiest place for a secret to slip in.
func TestSecurityEventMetadataCarriesNoSecrets(t *testing.T) {
	f := newFixture(t)
	platformToken := f.platformToken()
	acme := f.newTenant(platformToken, "ACME Corp", "alice@acme.com", "senhaAlice1")
	instance := f.createInstance(acme.token, "vendas-01")
	key := f.createKey(acme.token, instance.ID, "produção")

	var reset resetPayload
	f.do(request{method: http.MethodPost,
		path: "/admin/tenants/" + acme.tenant.ID + "/admin/reset-password", token: platformToken}).
		data(http.StatusOK, &reset)

	rows, err := f.infra.Pool.Query(t.Context(), `SELECT event_type, metadata::text FROM security_events`)
	require.NoError(t, err)
	defer rows.Close()

	var inspected int
	for rows.Next() {
		var eventType, metadata string
		require.NoError(t, rows.Scan(&eventType, &metadata))
		inspected++

		for _, secret := range []string{key.APIKey, reset.TemporaryPassword, "senhaAlice1", bootstrapPassword} {
			assert.NotContains(t, metadata, secret,
				"event %q leaked a secret into its metadata", eventType)
		}
		assert.NotContains(t, metadata, "argon2id", "event %q leaked a password hash", eventType)
	}
	require.NoError(t, rows.Err())
	assert.Positive(t, inspected, "the walk must have produced events to inspect")
}

// A request whose body the framework refuses outright — wrong media type — must
// not come back carrying the credentials that were in it. The refusal happens
// before any handler runs, which is exactly why it is easy to miss.
func TestRejectedContentTypeDoesNotEchoTheCredentials(t *testing.T) {
	f := newFixture(t)

	httpReq := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/auth/login",
		strings.NewReader(`{"email":"root@example.com","password":"`+bootstrapPassword+`"}`))
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, httpReq)

	body := recorder.Body.String()
	require.Equal(t, http.StatusUnsupportedMediaType, recorder.Code)
	assert.NotContains(t, body, bootstrapPassword, "the refused body must not be echoed back")
	assert.Contains(t, body, "UNSUPPORTED_MEDIA_TYPE", "a client mistake must not report an internal error")
	assert.NotContains(t, body, "INTERNAL_ERROR")
	assert.NotContains(t, f.logs.String(), bootstrapPassword)
}
