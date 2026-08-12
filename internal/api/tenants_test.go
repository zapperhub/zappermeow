package api_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tenantPayload is the tenant shape returned by the API.
type tenantPayload struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Admin  *struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"admin"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// createTenant provisions a tenant and returns it.
func (f *fixture) createTenant(token, name, adminName, adminEmail, adminPassword string) tenantPayload {
	f.t.Helper()

	var tenant tenantPayload
	response := f.do(request{
		method: http.MethodPost,
		path:   "/admin/tenants",
		token:  token,
		body: map[string]any{
			"name":  name,
			"admin": map[string]string{"name": adminName, "email": adminEmail, "password": adminPassword},
		},
	})
	response.data(http.StatusCreated, &tenant)
	assertEnvelopeMembers(f.t, response.Body)
	return tenant
}

// US1 scenario 4: the tenant is created active and its admin can log in.
func TestCreateTenantAndItsAdminCanLogIn(t *testing.T) {
	f := newFixture(t)
	token := f.platformToken()

	tenant := f.createTenant(token, "ACME Corp", "Alice", "alice@acme.com", "senhaAlice1")

	assert.NotEmpty(t, tenant.ID)
	assert.Equal(t, "ACME Corp", tenant.Name)
	assert.Equal(t, "active", tenant.Status, "a tenant is born active")
	require.NotNil(t, tenant.Admin)
	assert.Equal(t, "alice@acme.com", tenant.Admin.Email)
	assert.Equal(t, int64(1), f.countEvents("tenant_created"))

	var login struct {
		Audience string `json:"audience"`
	}
	f.do(request{method: http.MethodPost, path: "/auth/login", body: map[string]string{
		"email": "alice@acme.com", "password": "senhaAlice1",
	}}).data(http.StatusOK, &login)
	assert.Equal(t, "tenant", login.Audience, "a tenant admin authenticates into the tenant plane")
}

// The creation response must never echo the admin password back.
func TestCreateTenantResponseHidesPassword(t *testing.T) {
	f := newFixture(t)
	token := f.platformToken()

	response := f.do(request{
		method: http.MethodPost,
		path:   "/admin/tenants",
		token:  token,
		body: map[string]any{
			"name":  "ACME Corp",
			"admin": map[string]string{"name": "Alice", "email": "alice@acme.com", "password": "senhaAlice1"},
		},
	})

	require.Equal(t, http.StatusCreated, response.Status)
	assert.NotContains(t, string(response.Body), "senhaAlice1")
	assert.NotContains(t, f.logs.String(), "senhaAlice1")
}

// US1 scenario 5: listing shows every tenant with name, status and dates.
func TestListTenants(t *testing.T) {
	f := newFixture(t)
	token := f.platformToken()

	var empty struct {
		Tenants []tenantPayload `json:"tenants"`
	}
	f.do(request{method: http.MethodGet, path: "/admin/tenants", token: token}).data(http.StatusOK, &empty)
	assert.Empty(t, empty.Tenants, "an empty platform lists nothing and is not an error")

	f.createTenant(token, "ACME Corp", "Alice", "alice@acme.com", "senhaAlice1")
	f.createTenant(token, "Globex", "Bob", "bob@globex.com", "senhaBob123")

	var listed struct {
		Tenants []tenantPayload `json:"tenants"`
	}
	f.do(request{method: http.MethodGet, path: "/admin/tenants", token: token}).data(http.StatusOK, &listed)

	require.Len(t, listed.Tenants, 2)
	for _, tenant := range listed.Tenants {
		assert.NotEmpty(t, tenant.Name)
		assert.Equal(t, "active", tenant.Status)
		assert.NotEmpty(t, tenant.CreatedAt)
	}
}

func TestGetAndRenameTenant(t *testing.T) {
	f := newFixture(t)
	token := f.platformToken()
	created := f.createTenant(token, "ACME Corp", "Alice", "alice@acme.com", "senhaAlice1")

	var fetched tenantPayload
	f.do(request{method: http.MethodGet, path: "/admin/tenants/" + created.ID, token: token}).
		data(http.StatusOK, &fetched)
	assert.Equal(t, created.ID, fetched.ID)
	require.NotNil(t, fetched.Admin)
	assert.Equal(t, "Alice", fetched.Admin.Name)

	var renamed tenantPayload
	f.do(request{
		method: http.MethodPatch,
		path:   "/admin/tenants/" + created.ID,
		token:  token,
		body:   map[string]string{"name": "ACME International"},
	}).data(http.StatusOK, &renamed)
	assert.Equal(t, "ACME International", renamed.Name)
	assert.Equal(t, int64(1), f.countEvents("tenant_updated"))
}

func TestGetUnknownTenantIsNotFound(t *testing.T) {
	f := newFixture(t)
	token := f.platformToken()

	f.do(request{
		method: http.MethodGet,
		path:   "/admin/tenants/0193f5c8-0000-7000-8000-000000000000",
		token:  token,
	}).problem(http.StatusNotFound, "RESOURCE_NOT_FOUND")
}

// FR-005: names and emails are unique, and the response says which one collided.
func TestDuplicateTenantNameOrAdminEmailConflicts(t *testing.T) {
	f := newFixture(t)
	token := f.platformToken()
	f.createTenant(token, "ACME Corp", "Alice", "alice@acme.com", "senhaAlice1")

	t.Run("duplicate tenant name", func(t *testing.T) {
		doc := f.do(request{
			method: http.MethodPost, path: "/admin/tenants", token: token,
			body: map[string]any{
				"name":  "ACME Corp",
				"admin": map[string]string{"name": "Carol", "email": "carol@acme.com", "password": "senhaCarol1"},
			},
		}).problem(http.StatusConflict, "RESOURCE_CONFLICT")

		require.NotEmpty(t, doc.Errors)
		assert.Equal(t, "body.name", doc.Errors[0].Location)
	})

	t.Run("duplicate admin email", func(t *testing.T) {
		doc := f.do(request{
			method: http.MethodPost, path: "/admin/tenants", token: token,
			body: map[string]any{
				"name":  "Initech",
				"admin": map[string]string{"name": "Alice II", "email": "alice@acme.com", "password": "senhaAlice2"},
			},
		}).problem(http.StatusConflict, "RESOURCE_CONFLICT")

		require.NotEmpty(t, doc.Errors)
		assert.Equal(t, "body.admin.email", doc.Errors[0].Location)
	})

	// Names are compared case-insensitively, so "acme corp" is still a duplicate.
	t.Run("case-insensitive tenant name", func(t *testing.T) {
		f.do(request{
			method: http.MethodPost, path: "/admin/tenants", token: token,
			body: map[string]any{
				"name":  "acme corp",
				"admin": map[string]string{"name": "Dave", "email": "dave@acme.com", "password": "senhaDave12"},
			},
		}).problem(http.StatusConflict, "RESOURCE_CONFLICT")
	})
}

// A rejected creation must leave nothing behind (FR-022).
func TestConflictLeavesNoPartialState(t *testing.T) {
	f := newFixture(t)
	token := f.platformToken()
	f.createTenant(token, "ACME Corp", "Alice", "alice@acme.com", "senhaAlice1")

	f.do(request{
		method: http.MethodPost, path: "/admin/tenants", token: token,
		body: map[string]any{
			"name":  "Initech",
			"admin": map[string]string{"name": "Alice II", "email": "alice@acme.com", "password": "senhaAlice2"},
		},
	}).problem(http.StatusConflict, "RESOURCE_CONFLICT")

	var listed struct {
		Tenants []tenantPayload `json:"tenants"`
	}
	f.do(request{method: http.MethodGet, path: "/admin/tenants", token: token}).data(http.StatusOK, &listed)
	require.Len(t, listed.Tenants, 1, "the half-built tenant must have been rolled back")
	assert.Equal(t, "ACME Corp", listed.Tenants[0].Name)
}

// FR-022: invalid input is rejected with the offending member and rule.
func TestCreateTenantValidatesInput(t *testing.T) {
	f := newFixture(t)
	token := f.platformToken()

	tests := []struct {
		name         string
		body         map[string]any
		wantLocation string
	}{
		{
			name: "blank tenant name",
			body: map[string]any{
				"name":  "   ",
				"admin": map[string]string{"name": "Alice", "email": "alice@acme.com", "password": "senhaAlice1"},
			},
			wantLocation: "body.name",
		},
		{
			name: "malformed admin email",
			body: map[string]any{
				"name":  "ACME Corp",
				"admin": map[string]string{"name": "Alice", "email": "not-an-email", "password": "senhaAlice1"},
			},
			wantLocation: "body.admin.email",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := f.do(request{method: http.MethodPost, path: "/admin/tenants", token: token, body: tc.body}).
				problem(http.StatusUnprocessableEntity, "VALIDATION_ERROR")

			require.NotEmpty(t, doc.Errors)
			assert.Equal(t, tc.wantLocation, doc.Errors[0].Location)
		})
	}
}

// A short password is rejected, and the rejected value is never echoed back.
func TestCreateTenantRejectsShortPasswordWithoutEchoingIt(t *testing.T) {
	f := newFixture(t)
	token := f.platformToken()

	doc := f.do(request{
		method: http.MethodPost, path: "/admin/tenants", token: token,
		body: map[string]any{
			"name":  "ACME Corp",
			"admin": map[string]string{"name": "Alice", "email": "alice@acme.com", "password": "short"},
		},
	}).problem(http.StatusUnprocessableEntity, "VALIDATION_ERROR")

	require.NotEmpty(t, doc.Errors)
	for _, detail := range doc.Errors {
		assert.Nil(t, detail.Value, "the rejected password must not be echoed")
	}
	assert.NotContains(t, doc.Detail, "short")
}

// US1 scenario 6: a tenant token must not reach the platform plane.
// US1 scenario 7: no credential at all is refused without leaking internals.
func TestPlatformRoutesRejectWrongOrMissingCredentials(t *testing.T) {
	f := newFixture(t)
	platformToken := f.platformToken()
	f.createTenant(platformToken, "ACME Corp", "Alice", "alice@acme.com", "senhaAlice1")
	tenantToken := f.login("alice@acme.com", "senhaAlice1")

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/admin/tenants"},
		{http.MethodGet, "/admin/tenants"},
		{http.MethodGet, "/admin/tenants/0193f5c8-0000-7000-8000-000000000000"},
		{http.MethodPatch, "/admin/tenants/0193f5c8-0000-7000-8000-000000000000"},
	}

	for _, route := range routes {
		t.Run("tenant token on "+route.method+" "+route.path, func(t *testing.T) {
			f.do(request{method: route.method, path: route.path, token: tenantToken, body: map[string]any{}}).
				problem(http.StatusForbidden, "WRONG_AUDIENCE")
		})

		t.Run("no token on "+route.method+" "+route.path, func(t *testing.T) {
			doc := f.do(request{method: route.method, path: route.path, body: map[string]any{}}).
				problem(http.StatusUnauthorized, "UNAUTHENTICATED")
			assert.NotContains(t, doc.Detail, "sql")
			assert.NotContains(t, doc.Detail, "postgres")
		})

		t.Run("garbage token on "+route.method+" "+route.path, func(t *testing.T) {
			f.do(request{method: route.method, path: route.path, token: "not-a-token", body: map[string]any{}}).
				problem(http.StatusUnauthorized, "UNAUTHENTICATED")
		})
	}
}
