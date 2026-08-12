package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetPayload is the one-shot reset response.
type resetPayload struct {
	TemporaryPassword  string `json:"temporary_password"`
	MustChangePassword bool   `json:"must_change_password"`
}

// US5 scenario 1: changing the password with the correct current one takes
// effect, and the old password stops working.
func TestChangePasswordInvalidatesTheOldOne(t *testing.T) {
	f := newFixture(t)
	acme := f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")

	response := f.do(request{method: http.MethodPost, path: "/auth/password", token: acme.token,
		body: map[string]string{"current_password": "senhaAlice1", "new_password": "novaSenhaAlice2"}})
	require.Equal(t, http.StatusNoContent, response.Status)
	assert.Equal(t, int64(1), f.countEvents("password_changed"))

	f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": "alice@acme.com", "password": "senhaAlice1",
	}}).problem(http.StatusUnauthorized, "INVALID_CREDENTIALS")

	assert.NotEmpty(t, f.login("alice@acme.com", "novaSenhaAlice2"))
}

// SC-004: a token minted before the change must stop being accepted, otherwise
// a stolen token would outlive the password it came from.
//
// The comparison has one-second granularity because that is all a JWT `iat`
// carries, so the test separates the two events by a second — which is also the
// only realistic shape of the threat: a stolen token is seconds or minutes old
// by the time its owner reacts, never sub-second.
func TestChangingPasswordInvalidatesTokensIssuedBefore(t *testing.T) {
	f := newFixture(t)
	acme := f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")

	f.do(request{method: http.MethodGet, path: "/instances", token: acme.token}).data(http.StatusOK, nil)

	time.Sleep(1100 * time.Millisecond)

	f.do(request{method: http.MethodPost, path: "/auth/password", token: acme.token,
		body: map[string]string{"current_password": "senhaAlice1", "new_password": "novaSenhaAlice2"}})

	f.do(request{method: http.MethodGet, path: "/instances", token: acme.token}).
		problem(http.StatusUnauthorized, "UNAUTHENTICATED")

	// A token minted after the change works normally.
	fresh := f.login("alice@acme.com", "novaSenhaAlice2")
	f.do(request{method: http.MethodGet, path: "/instances", token: fresh}).data(http.StatusOK, nil)
}

// US5 scenario 2: a wrong current password is refused.
func TestChangePasswordRequiresTheCurrentOne(t *testing.T) {
	f := newFixture(t)
	acme := f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")

	f.do(request{method: http.MethodPost, path: "/auth/password", token: acme.token,
		body: map[string]string{"current_password": "not-my-password", "new_password": "novaSenhaAlice2"}}).
		problem(http.StatusForbidden, "INVALID_CURRENT_PASSWORD")

	// Nothing changed.
	assert.NotEmpty(t, f.login("alice@acme.com", "senhaAlice1"))
}

func TestChangePasswordValidatesTheNewOne(t *testing.T) {
	f := newFixture(t)
	acme := f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")

	doc := f.do(request{method: http.MethodPost, path: "/auth/password", token: acme.token,
		body: map[string]string{"current_password": "senhaAlice1", "new_password": "short"}}).
		problem(http.StatusUnprocessableEntity, "VALIDATION_ERROR")

	require.NotEmpty(t, doc.Errors)
	assert.Equal(t, "body.new_password", doc.Errors[0].Location)
	for _, detail := range doc.Errors {
		assert.Nil(t, detail.Value, "the rejected password must not be echoed back")
	}
}

// The super-admin changes its own password through the same route.
func TestSuperAdminChangesItsOwnPassword(t *testing.T) {
	f := newFixture(t)
	token := f.platformToken()

	response := f.do(request{method: http.MethodPost, path: "/auth/password", token: token,
		body: map[string]string{"current_password": bootstrapPassword, "new_password": "novaSenhaRoot1"}})
	require.Equal(t, http.StatusNoContent, response.Status)

	assert.NotEmpty(t, f.login(bootstrapEmail, "novaSenhaRoot1"))
}

// US5 scenarios 3-5: the super-admin resets a forgotten password, the admin
// logs in with the temporary one and is confined to the change route until it
// replaces it.
func TestResetPasswordForcesChangeBeforeAnythingElse(t *testing.T) {
	f := newFixture(t)
	platformToken := f.platformToken()
	acme := f.newTenant(platformToken, "ACME Corp", "alice@acme.com", "senhaAlice1")
	instance := f.createInstance(acme.token, "vendas-01")

	// Scenario 3: the temporary password is shown exactly once.
	var reset resetPayload
	f.do(request{method: http.MethodPost,
		path:  "/admin/tenants/" + acme.tenant.ID + "/admin/reset-password",
		token: platformToken}).
		data(http.StatusOK, &reset)

	require.NotEmpty(t, reset.TemporaryPassword)
	assert.True(t, reset.MustChangePassword)
	assert.Equal(t, int64(1), f.countEvents("password_reset"))
	assert.NotContains(t, f.logs.String(), reset.TemporaryPassword, "a temporary password must never be logged")

	// The old password is gone.
	f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": "alice@acme.com", "password": "senhaAlice1",
	}}).problem(http.StatusUnauthorized, "INVALID_CREDENTIALS")

	// Scenario 4: logging in with the temporary password says a change is due.
	var login struct {
		AccessToken        string `json:"access_token"`
		MustChangePassword bool   `json:"must_change_password"`
	}
	f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": "alice@acme.com", "password": reset.TemporaryPassword,
	}}).data(http.StatusOK, &login)
	assert.True(t, login.MustChangePassword)

	// Every other route is closed until the password is replaced.
	blocked := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/instances"},
		{http.MethodPost, "/instances"},
		{http.MethodGet, "/instances/" + instance.ID},
		{http.MethodGet, "/instances/" + instance.ID + "/keys"},
	}
	for _, route := range blocked {
		t.Run("blocked: "+route.method+" "+route.path, func(t *testing.T) {
			f.do(request{method: route.method, path: route.path, token: login.AccessToken,
				body: map[string]string{"name": "should-not-work"}}).
				problem(http.StatusForbidden, "PASSWORD_CHANGE_REQUIRED")
		})
	}

	// Scenario 5: after setting a new password, full access is restored and the
	// temporary one is dead.
	response := f.do(request{method: http.MethodPost, path: "/auth/password", token: login.AccessToken,
		body: map[string]string{"current_password": reset.TemporaryPassword, "new_password": "definitivaAlice3"}})
	require.Equal(t, http.StatusNoContent, response.Status)

	f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": "alice@acme.com", "password": reset.TemporaryPassword,
	}}).problem(http.StatusUnauthorized, "INVALID_CREDENTIALS")

	restored := f.login("alice@acme.com", "definitivaAlice3")
	f.do(request{method: http.MethodGet, path: "/instances", token: restored}).data(http.StatusOK, nil)
}

// Two resets in a row must produce different secrets.
func TestResetGeneratesAFreshSecretEachTime(t *testing.T) {
	f := newFixture(t)
	platformToken := f.platformToken()
	acme := f.newTenant(platformToken, "ACME Corp", "alice@acme.com", "senhaAlice1")

	var first, second resetPayload
	f.do(request{method: http.MethodPost,
		path: "/admin/tenants/" + acme.tenant.ID + "/admin/reset-password", token: platformToken}).
		data(http.StatusOK, &first)
	f.do(request{method: http.MethodPost,
		path: "/admin/tenants/" + acme.tenant.ID + "/admin/reset-password", token: platformToken}).
		data(http.StatusOK, &second)

	assert.NotEqual(t, first.TemporaryPassword, second.TemporaryPassword)

	// Only the most recent one works.
	f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": "alice@acme.com", "password": first.TemporaryPassword,
	}}).problem(http.StatusUnauthorized, "INVALID_CREDENTIALS")
	f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": "alice@acme.com", "password": second.TemporaryPassword,
	}}).data(http.StatusOK, nil)
}

func TestResetRequiresPlatformTokenAndAKnownTenant(t *testing.T) {
	f := newFixture(t)
	platformToken := f.platformToken()
	acme := f.newTenant(platformToken, "ACME Corp", "alice@acme.com", "senhaAlice1")

	t.Run("tenant token is refused", func(t *testing.T) {
		f.do(request{method: http.MethodPost,
			path: "/admin/tenants/" + acme.tenant.ID + "/admin/reset-password", token: acme.token}).
			problem(http.StatusForbidden, "WRONG_AUDIENCE")
	})

	t.Run("no token is refused", func(t *testing.T) {
		f.do(request{method: http.MethodPost,
			path: "/admin/tenants/" + acme.tenant.ID + "/admin/reset-password"}).
			problem(http.StatusUnauthorized, "UNAUTHENTICATED")
	})

	t.Run("unknown tenant", func(t *testing.T) {
		f.do(request{method: http.MethodPost,
			path:  "/admin/tenants/0193f5c8-0000-7000-8000-000000000000/admin/reset-password",
			token: platformToken}).
			problem(http.StatusNotFound, "RESOURCE_NOT_FOUND")
	})
}

func TestChangePasswordRequiresAuthentication(t *testing.T) {
	f := newFixture(t)

	f.do(request{method: http.MethodPost, path: "/auth/password",
		body: map[string]string{"current_password": "x", "new_password": "novaSenha123"}}).
		problem(http.StatusUnauthorized, "UNAUTHENTICATED")
}

// A reset clears any lockout, otherwise the admin could not use the password
// they were just handed.
func TestResetClearsAnExistingLockout(t *testing.T) {
	f := newFixture(t)
	platformToken := f.platformToken()
	acme := f.newTenant(platformToken, "ACME Corp", "alice@acme.com", "senhaAlice1")

	for range 5 {
		f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
			"email": "alice@acme.com", "password": "wrong-password",
		}})
	}
	f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": "alice@acme.com", "password": "senhaAlice1",
	}}).problem(http.StatusUnauthorized, "INVALID_CREDENTIALS")

	var reset resetPayload
	f.do(request{method: http.MethodPost,
		path: "/admin/tenants/" + acme.tenant.ID + "/admin/reset-password", token: platformToken}).
		data(http.StatusOK, &reset)

	f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": "alice@acme.com", "password": reset.TemporaryPassword,
	}}).data(http.StatusOK, nil)
}
