package api_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// instancePayload is the instance shape returned by the API.
type instancePayload struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	State     string `json:"state"`
	TenantID  string `json:"tenant_id"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// tenantSetup is a provisioned tenant with a logged-in admin.
type tenantSetup struct {
	tenant tenantPayload
	token  string
	email  string
}

// newTenant provisions a tenant and logs its admin in.
func (f *fixture) newTenant(platformToken, name, adminEmail, password string) tenantSetup {
	f.t.Helper()
	tenant := f.createTenant(platformToken, name, "Admin of "+name, adminEmail, password)
	return tenantSetup{tenant: tenant, token: f.login(adminEmail, password), email: adminEmail}
}

// createInstance registers an instance for the tenant behind the token.
func (f *fixture) createInstance(token, name string) instancePayload {
	f.t.Helper()

	var instance instancePayload
	f.do(request{method: http.MethodPost, path: "/instances", token: token,
		body: map[string]string{"name": name}}).
		data(http.StatusCreated, &instance)
	return instance
}

// US2 scenario 1: the tenant admin gets a tenant-audience token carrying its
// tenant. Scenario 2: a new instance is born "registered".
func TestCreateInstanceStartsRegistered(t *testing.T) {
	f := newFixture(t)
	acme := f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")

	instance := f.createInstance(acme.token, "vendas-01")

	assert.NotEmpty(t, instance.ID)
	assert.Equal(t, "vendas-01", instance.Name)
	assert.Equal(t, "registered", instance.State, "an instance is not paired in this feature")
	assert.Equal(t, acme.tenant.ID, instance.TenantID)
	assert.Equal(t, int64(1), f.countEvents("instance_created"))
}

// US2 scenario 3: a tenant only ever sees its own instances.
func TestListInstancesIsScopedToTheCallersTenant(t *testing.T) {
	f := newFixture(t)
	platformToken := f.platformToken()
	acme := f.newTenant(platformToken, "ACME Corp", "alice@acme.com", "senhaAlice1")
	globex := f.newTenant(platformToken, "Globex", "bob@globex.com", "senhaBob123")

	f.createInstance(acme.token, "vendas-01")
	f.createInstance(acme.token, "suporte-01")
	f.createInstance(globex.token, "globex-01")

	var acmeList struct {
		Instances []instancePayload `json:"instances"`
	}
	f.do(request{method: http.MethodGet, path: "/instances", token: acme.token}).data(http.StatusOK, &acmeList)
	require.Len(t, acmeList.Instances, 2)
	for _, instance := range acmeList.Instances {
		assert.Equal(t, acme.tenant.ID, instance.TenantID)
	}

	var globexList struct {
		Instances []instancePayload `json:"instances"`
	}
	f.do(request{method: http.MethodGet, path: "/instances", token: globex.token}).data(http.StatusOK, &globexList)
	require.Len(t, globexList.Instances, 1)
	assert.Equal(t, "globex-01", globexList.Instances[0].Name)
}

// US2 scenario 4: reaching for another tenant's instance is refused without
// confirming that it exists — the answer is identical to an unknown id (FR-009).
func TestForeignInstanceIsIndistinguishableFromMissing(t *testing.T) {
	f := newFixture(t)
	platformToken := f.platformToken()
	acme := f.newTenant(platformToken, "ACME Corp", "alice@acme.com", "senhaAlice1")
	globex := f.newTenant(platformToken, "Globex", "bob@globex.com", "senhaBob123")

	foreign := f.createInstance(acme.token, "vendas-01")
	const missing = "0193f5c8-0000-7000-8000-000000000000"

	cases := []struct {
		name   string
		method string
		body   any
	}{
		{name: "get", method: http.MethodGet},
		{name: "rename", method: http.MethodPatch, body: map[string]string{"name": "hijacked"}},
		{name: "delete", method: http.MethodDelete},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			foreignResponse := f.do(request{
				method: tc.method, path: "/instances/" + foreign.ID, token: globex.token, body: tc.body,
			})
			missingResponse := f.do(request{
				method: tc.method, path: "/instances/" + missing, token: globex.token, body: tc.body,
			})

			foreignDoc := foreignResponse.problem(http.StatusNotFound, "RESOURCE_NOT_FOUND")
			missingDoc := missingResponse.problem(http.StatusNotFound, "RESOURCE_NOT_FOUND")
			assert.Equal(t, missingDoc.Detail, foreignDoc.Detail,
				"an instance of another tenant must answer exactly like a missing one")
			assert.Equal(t, missingDoc.Title, foreignDoc.Title)
		})
	}

	// The instance the other tenant tried to reach is untouched.
	var still instancePayload
	f.do(request{method: http.MethodGet, path: "/instances/" + foreign.ID, token: acme.token}).
		data(http.StatusOK, &still)
	assert.Equal(t, "vendas-01", still.Name)
}

func TestGetAndRenameInstance(t *testing.T) {
	f := newFixture(t)
	acme := f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")
	created := f.createInstance(acme.token, "vendas-01")

	var fetched instancePayload
	f.do(request{method: http.MethodGet, path: "/instances/" + created.ID, token: acme.token}).
		data(http.StatusOK, &fetched)
	assert.Equal(t, created.ID, fetched.ID)

	var renamed instancePayload
	f.do(request{method: http.MethodPatch, path: "/instances/" + created.ID, token: acme.token,
		body: map[string]string{"name": "vendas-sp"}}).
		data(http.StatusOK, &renamed)
	assert.Equal(t, "vendas-sp", renamed.Name)
	assert.Equal(t, int64(1), f.countEvents("instance_updated"))
}

// US2 scenario 5: a deleted instance disappears from the listing.
func TestDeleteInstanceRemovesItFromTheListing(t *testing.T) {
	f := newFixture(t)
	acme := f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")
	keep := f.createInstance(acme.token, "vendas-01")
	remove := f.createInstance(acme.token, "suporte-01")

	response := f.do(request{method: http.MethodDelete, path: "/instances/" + remove.ID, token: acme.token})
	require.Equal(t, http.StatusNoContent, response.Status)
	assert.Empty(t, response.Body, "a 204 carries no body, not even the envelope")

	var listed struct {
		Instances []instancePayload `json:"instances"`
	}
	f.do(request{method: http.MethodGet, path: "/instances", token: acme.token}).data(http.StatusOK, &listed)
	require.Len(t, listed.Instances, 1)
	assert.Equal(t, keep.ID, listed.Instances[0].ID)

	// A deleted instance is indistinguishable from one that never existed.
	f.do(request{method: http.MethodGet, path: "/instances/" + remove.ID, token: acme.token}).
		problem(http.StatusNotFound, "RESOURCE_NOT_FOUND")
	assert.Equal(t, int64(1), f.countEvents("instance_deleted"))
}

// Instance names are unique per tenant, but two tenants may use the same name.
func TestInstanceNameIsUniquePerTenantOnly(t *testing.T) {
	f := newFixture(t)
	platformToken := f.platformToken()
	acme := f.newTenant(platformToken, "ACME Corp", "alice@acme.com", "senhaAlice1")
	globex := f.newTenant(platformToken, "Globex", "bob@globex.com", "senhaBob123")

	f.createInstance(acme.token, "vendas-01")

	doc := f.do(request{method: http.MethodPost, path: "/instances", token: acme.token,
		body: map[string]string{"name": "vendas-01"}}).
		problem(http.StatusConflict, "RESOURCE_CONFLICT")
	require.NotEmpty(t, doc.Errors)
	assert.Equal(t, "body.name", doc.Errors[0].Location)

	// The same name in a different tenant is perfectly fine.
	other := f.createInstance(globex.token, "vendas-01")
	assert.Equal(t, "vendas-01", other.Name)
}

func TestCreateInstanceValidatesName(t *testing.T) {
	f := newFixture(t)
	acme := f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")

	doc := f.do(request{method: http.MethodPost, path: "/instances", token: acme.token,
		body: map[string]string{"name": "   "}}).
		problem(http.StatusUnprocessableEntity, "VALIDATION_ERROR")

	require.NotEmpty(t, doc.Errors)
	assert.Equal(t, "body.name", doc.Errors[0].Location)
}

// US2 scenario 6: a platform token must not reach the tenant plane.
func TestInstanceRoutesRejectWrongOrMissingCredentials(t *testing.T) {
	f := newFixture(t)
	platformToken := f.platformToken()

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/instances"},
		{http.MethodGet, "/instances"},
		{http.MethodGet, "/instances/0193f5c8-0000-7000-8000-000000000000"},
		{http.MethodPatch, "/instances/0193f5c8-0000-7000-8000-000000000000"},
		{http.MethodDelete, "/instances/0193f5c8-0000-7000-8000-000000000000"},
	}

	for _, route := range routes {
		t.Run("platform token on "+route.method+" "+route.path, func(t *testing.T) {
			f.do(request{method: route.method, path: route.path, token: platformToken, body: map[string]any{}}).
				problem(http.StatusForbidden, "WRONG_AUDIENCE")
		})

		t.Run("no token on "+route.method+" "+route.path, func(t *testing.T) {
			f.do(request{method: route.method, path: route.path, body: map[string]any{}}).
				problem(http.StatusUnauthorized, "UNAUTHENTICATED")
		})
	}
}
