package api_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// suspendedTenant is a tenant with a live instance, key and token, then suspended.
type suspendedTenant struct {
	setup    tenantSetup
	instance instancePayload
	key      createdKeyPayload
}

// newLiveTenant provisions a tenant with one instance and one working key.
func (f *fixture) newLiveTenant(platformToken string) suspendedTenant {
	f.t.Helper()

	acme := f.newTenant(platformToken, "ACME Corp", "alice@acme.com", "senhaAlice1")
	instance := f.createInstance(acme.token, "vendas-01")
	key := f.createKey(acme.token, instance.ID, "produção")

	// Everything works before the suspension.
	f.do(request{method: http.MethodGet, path: "/instances/" + instance.ID + "/whoami", apiKey: key.APIKey}).
		data(http.StatusOK, nil)

	return suspendedTenant{setup: acme, instance: instance, key: key}
}

// US4 scenarios 1-3: suspending blocks the login, the tokens already issued and
// every API key of the tenant's instances — all from the next request onwards.
func TestSuspendingTenantCascadesToEveryCredential(t *testing.T) {
	f := newFixture(t)
	platformToken := f.platformToken()
	live := f.newLiveTenant(platformToken)

	var suspended tenantPayload
	f.do(request{method: http.MethodPost, path: "/admin/tenants/" + live.setup.tenant.ID + "/suspend",
		token: platformToken}).
		data(http.StatusOK, &suspended)
	assert.Equal(t, "suspended", suspended.Status)
	assert.Equal(t, int64(1), f.countEvents("tenant_suspended"))

	// Scenario 1: the admin can no longer log in, and is told why — they hold
	// the right password, so nothing is leaked by saying so.
	t.Run("login is refused", func(t *testing.T) {
		f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
			"email": "alice@acme.com", "password": "senhaAlice1",
		}}).problem(http.StatusForbidden, "TENANT_SUSPENDED")
	})

	// A wrong password against a suspended tenant must stay generic, otherwise
	// the 403 would confirm which accounts exist.
	t.Run("wrong password stays generic", func(t *testing.T) {
		f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
			"email": "alice@acme.com", "password": "wrong-password",
		}}).problem(http.StatusUnauthorized, "INVALID_CREDENTIALS")
	})

	// Scenario 2: a token minted before the suspension stops being accepted.
	t.Run("token issued before the suspension is refused", func(t *testing.T) {
		f.do(request{method: http.MethodGet, path: "/instances", token: live.setup.token}).
			problem(http.StatusForbidden, "TENANT_SUSPENDED")
	})

	// Scenario 3: the operational keys of its instances stop working.
	t.Run("api key is refused", func(t *testing.T) {
		f.do(request{method: http.MethodGet, path: "/instances/" + live.instance.ID + "/whoami",
			apiKey: live.key.APIKey}).
			problem(http.StatusForbidden, "TENANT_SUSPENDED")
	})
}

// US4 scenario 4: reactivation restores everything without recreating a single
// credential — the same token and the same key work again.
func TestReactivatingTenantRestoresCredentials(t *testing.T) {
	f := newFixture(t)
	platformToken := f.platformToken()
	live := f.newLiveTenant(platformToken)

	f.do(request{method: http.MethodPost, path: "/admin/tenants/" + live.setup.tenant.ID + "/suspend",
		token: platformToken}).data(http.StatusOK, nil)

	var activated tenantPayload
	f.do(request{method: http.MethodPost, path: "/admin/tenants/" + live.setup.tenant.ID + "/activate",
		token: platformToken}).data(http.StatusOK, &activated)
	assert.Equal(t, "active", activated.Status)
	assert.Equal(t, int64(1), f.countEvents("tenant_activated"))

	// The very same credentials, never reissued.
	f.do(request{method: http.MethodGet, path: "/instances", token: live.setup.token}).data(http.StatusOK, nil)
	f.do(request{method: http.MethodGet, path: "/instances/" + live.instance.ID + "/whoami",
		apiKey: live.key.APIKey}).data(http.StatusOK, nil)
	assert.NotEmpty(t, f.login("alice@acme.com", "senhaAlice1"))
}

// Suspending twice is not an error: the operator's intent is already satisfied.
func TestSuspendAndActivateAreIdempotent(t *testing.T) {
	f := newFixture(t)
	platformToken := f.platformToken()
	acme := f.newTenant(platformToken, "ACME Corp", "alice@acme.com", "senhaAlice1")

	for range 2 {
		var suspended tenantPayload
		f.do(request{method: http.MethodPost, path: "/admin/tenants/" + acme.tenant.ID + "/suspend",
			token: platformToken}).data(http.StatusOK, &suspended)
		assert.Equal(t, "suspended", suspended.Status)
	}

	for range 2 {
		var activated tenantPayload
		f.do(request{method: http.MethodPost, path: "/admin/tenants/" + acme.tenant.ID + "/activate",
			token: platformToken}).data(http.StatusOK, &activated)
		assert.Equal(t, "active", activated.Status)
	}
}

// Suspension is scoped: one tenant's block must not touch another's.
func TestSuspensionDoesNotAffectOtherTenants(t *testing.T) {
	f := newFixture(t)
	platformToken := f.platformToken()
	live := f.newLiveTenant(platformToken)
	globex := f.newTenant(platformToken, "Globex", "bob@globex.com", "senhaBob123")
	globexInstance := f.createInstance(globex.token, "globex-01")
	globexKey := f.createKey(globex.token, globexInstance.ID, "produção")

	f.do(request{method: http.MethodPost, path: "/admin/tenants/" + live.setup.tenant.ID + "/suspend",
		token: platformToken}).data(http.StatusOK, nil)

	f.do(request{method: http.MethodGet, path: "/instances", token: globex.token}).data(http.StatusOK, nil)
	f.do(request{method: http.MethodGet, path: "/instances/" + globexInstance.ID + "/whoami",
		apiKey: globexKey.APIKey}).data(http.StatusOK, nil)
}

// US4 scenario 5 / FR-007: deletion is irreversible and takes everything with it.
func TestDeleteTenantRemovesEverythingIrreversibly(t *testing.T) {
	f := newFixture(t)
	platformToken := f.platformToken()
	live := f.newLiveTenant(platformToken)

	response := f.do(request{method: http.MethodDelete, path: "/admin/tenants/" + live.setup.tenant.ID,
		token: platformToken, body: map[string]string{"confirm_name": "ACME Corp"}})
	require.Equal(t, http.StatusNoContent, response.Status)
	assert.Equal(t, int64(1), f.countEvents("tenant_deleted"))

	t.Run("the tenant is gone", func(t *testing.T) {
		f.do(request{method: http.MethodGet, path: "/admin/tenants/" + live.setup.tenant.ID, token: platformToken}).
			problem(http.StatusNotFound, "RESOURCE_NOT_FOUND")

		var listed struct {
			Tenants []tenantPayload `json:"tenants"`
		}
		f.do(request{method: http.MethodGet, path: "/admin/tenants", token: platformToken}).
			data(http.StatusOK, &listed)
		assert.Empty(t, listed.Tenants)
	})

	// The admin user cascaded away, so its token can no longer resolve.
	t.Run("its token stops working", func(t *testing.T) {
		f.do(request{method: http.MethodGet, path: "/instances", token: live.setup.token}).
			problem(http.StatusUnauthorized, "UNAUTHENTICATED")
	})

	t.Run("its login no longer exists", func(t *testing.T) {
		f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
			"email": "alice@acme.com", "password": "senhaAlice1",
		}}).problem(http.StatusUnauthorized, "INVALID_CREDENTIALS")
	})

	t.Run("its api key stops working", func(t *testing.T) {
		f.do(request{method: http.MethodGet, path: "/instances/" + live.instance.ID + "/whoami",
			apiKey: live.key.APIKey}).
			problem(http.StatusUnauthorized, "UNAUTHENTICATED")
	})

	// The audit record states the size of the cascade (research R9).
	assert.Contains(t, f.logs.String(), `"meta_instances_deleted":1`)
	assert.Contains(t, f.logs.String(), `"meta_api_keys_revoked":1`)
	assert.Contains(t, f.logs.String(), `"meta_users_deleted":1`)
}

// A destructive operation must not proceed on an approximate confirmation.
func TestDeleteTenantRequiresExactConfirmation(t *testing.T) {
	f := newFixture(t)
	platformToken := f.platformToken()
	acme := f.newTenant(platformToken, "ACME Corp", "alice@acme.com", "senhaAlice1")

	tests := []struct {
		name    string
		confirm string
	}{
		{name: "wrong name", confirm: "Globex"},
		{name: "different case", confirm: "acme corp"},
		{name: "trailing whitespace", confirm: "ACME Corp "},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := f.do(request{method: http.MethodDelete, path: "/admin/tenants/" + acme.tenant.ID,
				token: platformToken, body: map[string]string{"confirm_name": tc.confirm}}).
				problem(http.StatusUnprocessableEntity, "VALIDATION_ERROR")

			require.NotEmpty(t, doc.Errors)
			assert.Equal(t, "body.confirm_name", doc.Errors[0].Location)
		})
	}

	// The tenant survived every failed attempt.
	f.do(request{method: http.MethodGet, path: "/admin/tenants/" + acme.tenant.ID, token: platformToken}).
		data(http.StatusOK, nil)
}

func TestTenantLifecycleRoutesRequirePlatformToken(t *testing.T) {
	f := newFixture(t)
	platformToken := f.platformToken()
	acme := f.newTenant(platformToken, "ACME Corp", "alice@acme.com", "senhaAlice1")

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/admin/tenants/" + acme.tenant.ID + "/suspend"},
		{http.MethodPost, "/admin/tenants/" + acme.tenant.ID + "/activate"},
		{http.MethodDelete, "/admin/tenants/" + acme.tenant.ID},
	}

	for _, route := range routes {
		t.Run("tenant token on "+route.path, func(t *testing.T) {
			f.do(request{method: route.method, path: route.path, token: acme.token,
				body: map[string]string{"confirm_name": "ACME Corp"}}).
				problem(http.StatusForbidden, "WRONG_AUDIENCE")
		})

		t.Run("no token on "+route.path, func(t *testing.T) {
			f.do(request{method: route.method, path: route.path,
				body: map[string]string{"confirm_name": "ACME Corp"}}).
				problem(http.StatusUnauthorized, "UNAUTHENTICATED")
		})
	}
}
