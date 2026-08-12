package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zapperhub/zappermeow/internal/config"
)

// createdKeyPayload is the one-shot creation response.
type createdKeyPayload struct {
	ID        string  `json:"id"`
	Label     *string `json:"label"`
	KeyPrefix string  `json:"key_prefix"`
	APIKey    string  `json:"api_key"`
	CreatedAt string  `json:"created_at"`
}

// keyPayload is a key as shown in listings.
type keyPayload struct {
	ID        string  `json:"id"`
	Label     *string `json:"label"`
	KeyPrefix string  `json:"key_prefix"`
	Status    string  `json:"status"`
	CreatedAt string  `json:"created_at"`
	RevokedAt *string `json:"revoked_at"`
}

// whoamiPayload is the operational verification response.
type whoamiPayload struct {
	Instance struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		State    string `json:"state"`
		TenantID string `json:"tenant_id"`
	} `json:"instance"`
	Key struct {
		KeyPrefix string  `json:"key_prefix"`
		Label     *string `json:"label"`
	} `json:"key"`
}

// createKey issues an API key for an instance.
func (f *fixture) createKey(token, instanceID, label string) createdKeyPayload {
	f.t.Helper()

	var created createdKeyPayload
	f.do(request{method: http.MethodPost, path: "/instances/" + instanceID + "/keys", token: token,
		body: map[string]string{"label": label}}).
		data(http.StatusCreated, &created)
	return created
}

// US3 scenario 1: the secret is returned once, in full, at creation.
func TestCreateAPIKeyReturnsSecretOnce(t *testing.T) {
	f := newFixture(t)
	acme := f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")
	instance := f.createInstance(acme.token, "vendas-01")

	created := f.createKey(acme.token, instance.ID, "produção")

	assert.True(t, strings.HasPrefix(created.APIKey, "zmk_"))
	assert.Equal(t, created.APIKey[:12], created.KeyPrefix)
	require.NotNil(t, created.Label)
	assert.Equal(t, "produção", *created.Label)
	assert.Equal(t, int64(1), f.countEvents("api_key_created"))
}

// US3 scenario 2: the listing shows metadata and never the secret (FR-011).
func TestListAPIKeysNeverExposesTheSecret(t *testing.T) {
	f := newFixture(t)
	acme := f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")
	instance := f.createInstance(acme.token, "vendas-01")
	created := f.createKey(acme.token, instance.ID, "produção")

	response := f.do(request{method: http.MethodGet, path: "/instances/" + instance.ID + "/keys", token: acme.token})

	var listed struct {
		Keys []keyPayload `json:"keys"`
	}
	response.data(http.StatusOK, &listed)

	require.Len(t, listed.Keys, 1)
	key := listed.Keys[0]
	assert.Equal(t, created.ID, key.ID)
	assert.Equal(t, created.KeyPrefix, key.KeyPrefix)
	assert.Equal(t, "active", key.Status)
	assert.Nil(t, key.RevokedAt)

	body := string(response.Body)
	assert.NotContains(t, body, created.APIKey, "the full secret must never be recoverable")
	assert.NotContains(t, body, "api_key")
	assert.NotContains(t, body, "secret_hash")
}

// SC-006: the secret must not reach the log stream either.
func TestAPIKeySecretIsNeverLogged(t *testing.T) {
	f := newFixture(t)
	acme := f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")
	instance := f.createInstance(acme.token, "vendas-01")
	created := f.createKey(acme.token, instance.ID, "produção")

	f.do(request{method: http.MethodGet, path: "/instances/" + instance.ID + "/whoami", apiKey: created.APIKey}).
		data(http.StatusOK, nil)

	logs := f.logs.String()
	assert.NotContains(t, logs, created.APIKey)
	assert.Contains(t, logs, created.KeyPrefix, "the prefix is what makes the key identifiable in the trail")
}

// US3 scenario 3: several keys of an instance work at the same time, which is
// what makes rotation without downtime possible.
func TestMultipleActiveKeysWorkSimultaneously(t *testing.T) {
	f := newFixture(t)
	acme := f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")
	instance := f.createInstance(acme.token, "vendas-01")

	first := f.createKey(acme.token, instance.ID, "produção")
	second := f.createKey(acme.token, instance.ID, "rotação")

	for name, key := range map[string]createdKeyPayload{"first": first, "second": second} {
		t.Run(name, func(t *testing.T) {
			var who whoamiPayload
			f.do(request{method: http.MethodGet, path: "/instances/" + instance.ID + "/whoami", apiKey: key.APIKey}).
				data(http.StatusOK, &who)
			assert.Equal(t, instance.ID, who.Instance.ID)
			assert.Equal(t, key.KeyPrefix, who.Key.KeyPrefix)
		})
	}
}

// FR-014: whoami proves the whole credential chain end to end.
func TestWhoamiIdentifiesTheInstanceAndKey(t *testing.T) {
	f := newFixture(t)
	acme := f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")
	instance := f.createInstance(acme.token, "vendas-01")
	created := f.createKey(acme.token, instance.ID, "produção")

	var who whoamiPayload
	f.do(request{method: http.MethodGet, path: "/instances/" + instance.ID + "/whoami", apiKey: created.APIKey}).
		data(http.StatusOK, &who)

	assert.Equal(t, instance.ID, who.Instance.ID)
	assert.Equal(t, "vendas-01", who.Instance.Name)
	assert.Equal(t, "registered", who.Instance.State)
	assert.Equal(t, acme.tenant.ID, who.Instance.TenantID)
	assert.Equal(t, created.KeyPrefix, who.Key.KeyPrefix)
	require.NotNil(t, who.Key.Label)
	assert.Equal(t, "produção", *who.Key.Label)
}

// US3 scenario 4 / FR-013: a key is bound to one instance, and is refused on
// another even inside the same tenant.
func TestKeyOfOneInstanceIsRefusedOnAnother(t *testing.T) {
	f := newFixture(t)
	acme := f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")

	first := f.createInstance(acme.token, "vendas-01")
	second := f.createInstance(acme.token, "suporte-01")
	key := f.createKey(acme.token, first.ID, "produção")

	f.do(request{method: http.MethodGet, path: "/instances/" + second.ID + "/whoami", apiKey: key.APIKey}).
		problem(http.StatusNotFound, "RESOURCE_NOT_FOUND")

	// The key still works where it belongs.
	f.do(request{method: http.MethodGet, path: "/instances/" + first.ID + "/whoami", apiKey: key.APIKey}).
		data(http.StatusOK, nil)
}

// US3 scenario 5 / SC-004: revocation takes effect on the very next request.
func TestRevokedKeyIsRefusedImmediately(t *testing.T) {
	f := newFixture(t)
	acme := f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")
	instance := f.createInstance(acme.token, "vendas-01")
	key := f.createKey(acme.token, instance.ID, "produção")

	f.do(request{method: http.MethodGet, path: "/instances/" + instance.ID + "/whoami", apiKey: key.APIKey}).
		data(http.StatusOK, nil)

	response := f.do(request{method: http.MethodDelete,
		path: "/instances/" + instance.ID + "/keys/" + key.ID, token: acme.token})
	require.Equal(t, http.StatusNoContent, response.Status)

	f.do(request{method: http.MethodGet, path: "/instances/" + instance.ID + "/whoami", apiKey: key.APIKey}).
		problem(http.StatusUnauthorized, "UNAUTHENTICATED")

	var listed struct {
		Keys []keyPayload `json:"keys"`
	}
	f.do(request{method: http.MethodGet, path: "/instances/" + instance.ID + "/keys", token: acme.token}).
		data(http.StatusOK, &listed)
	require.Len(t, listed.Keys, 1)
	assert.Equal(t, "revoked", listed.Keys[0].Status)
	assert.NotNil(t, listed.Keys[0].RevokedAt)
	assert.Equal(t, int64(1), f.countEvents("api_key_revoked"))
}

// Revoking one key must not disturb the others — that is the point of rotation.
func TestRevokingOneKeyLeavesTheOthersWorking(t *testing.T) {
	f := newFixture(t)
	acme := f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")
	instance := f.createInstance(acme.token, "vendas-01")
	old := f.createKey(acme.token, instance.ID, "antiga")
	fresh := f.createKey(acme.token, instance.ID, "nova")

	f.do(request{method: http.MethodDelete,
		path: "/instances/" + instance.ID + "/keys/" + old.ID, token: acme.token})

	f.do(request{method: http.MethodGet, path: "/instances/" + instance.ID + "/whoami", apiKey: old.APIKey}).
		problem(http.StatusUnauthorized, "UNAUTHENTICATED")
	f.do(request{method: http.MethodGet, path: "/instances/" + instance.ID + "/whoami", apiKey: fresh.APIKey}).
		data(http.StatusOK, nil)
}

// US3 scenario 6: key operations on another tenant's instance are refused
// without confirming that the instance exists.
func TestKeyRoutesRefuseForeignInstances(t *testing.T) {
	f := newFixture(t)
	platformToken := f.platformToken()
	acme := f.newTenant(platformToken, "ACME Corp", "alice@acme.com", "senhaAlice1")
	globex := f.newTenant(platformToken, "Globex", "bob@globex.com", "senhaBob123")

	foreign := f.createInstance(acme.token, "vendas-01")
	key := f.createKey(acme.token, foreign.ID, "produção")
	const missing = "0193f5c8-0000-7000-8000-000000000000"

	t.Run("create", func(t *testing.T) {
		foreignDoc := f.do(request{method: http.MethodPost, path: "/instances/" + foreign.ID + "/keys",
			token: globex.token, body: map[string]string{"label": "stolen"}}).
			problem(http.StatusNotFound, "RESOURCE_NOT_FOUND")
		missingDoc := f.do(request{method: http.MethodPost, path: "/instances/" + missing + "/keys",
			token: globex.token, body: map[string]string{"label": "stolen"}}).
			problem(http.StatusNotFound, "RESOURCE_NOT_FOUND")
		assert.Equal(t, missingDoc.Detail, foreignDoc.Detail)
	})

	t.Run("list", func(t *testing.T) {
		f.do(request{method: http.MethodGet, path: "/instances/" + foreign.ID + "/keys", token: globex.token}).
			problem(http.StatusNotFound, "RESOURCE_NOT_FOUND")
	})

	t.Run("revoke", func(t *testing.T) {
		f.do(request{method: http.MethodDelete,
			path: "/instances/" + foreign.ID + "/keys/" + key.ID, token: globex.token}).
			problem(http.StatusNotFound, "RESOURCE_NOT_FOUND")

		// The key survived the attempt.
		f.do(request{method: http.MethodGet, path: "/instances/" + foreign.ID + "/whoami", apiKey: key.APIKey}).
			data(http.StatusOK, nil)
	})
}

// US2 scenario 5 completed: deleting an instance takes its keys with it, and
// the audit trail records how many disappeared (research R9).
func TestDeletingInstanceRevokesItsKeysInCascade(t *testing.T) {
	f := newFixture(t)
	acme := f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")
	instance := f.createInstance(acme.token, "vendas-01")
	first := f.createKey(acme.token, instance.ID, "produção")
	second := f.createKey(acme.token, instance.ID, "rotação")

	f.do(request{method: http.MethodDelete, path: "/instances/" + instance.ID, token: acme.token})

	for name, key := range map[string]createdKeyPayload{"first": first, "second": second} {
		t.Run(name+" stops working", func(t *testing.T) {
			f.do(request{method: http.MethodGet, path: "/instances/" + instance.ID + "/whoami", apiKey: key.APIKey}).
				problem(http.StatusUnauthorized, "UNAUTHENTICATED")
		})
	}

	assert.Contains(t, f.logs.String(), `"meta_api_keys_revoked":2`,
		"the parent event must record the size of the cascade")
}

// The operational plane accepts only an API key: no key, a malformed one and a
// bearer token are all refused.
func TestWhoamiRejectsAnythingButAValidKey(t *testing.T) {
	f := newFixture(t)
	acme := f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")
	instance := f.createInstance(acme.token, "vendas-01")

	t.Run("no credential", func(t *testing.T) {
		f.do(request{method: http.MethodGet, path: "/instances/" + instance.ID + "/whoami"}).
			problem(http.StatusUnauthorized, "UNAUTHENTICATED")
	})

	t.Run("malformed key", func(t *testing.T) {
		f.do(request{method: http.MethodGet, path: "/instances/" + instance.ID + "/whoami", apiKey: "zmk_nonsense"}).
			problem(http.StatusUnauthorized, "UNAUTHENTICATED")
	})

	t.Run("tenant token instead of a key", func(t *testing.T) {
		f.do(request{method: http.MethodGet, path: "/instances/" + instance.ID + "/whoami", token: acme.token}).
			problem(http.StatusUnauthorized, "UNAUTHENTICATED")
	})
}

func TestCreateAPIKeyAcceptsNoLabel(t *testing.T) {
	f := newFixture(t)
	acme := f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")
	instance := f.createInstance(acme.token, "vendas-01")

	var created createdKeyPayload
	f.do(request{method: http.MethodPost, path: "/instances/" + instance.ID + "/keys", token: acme.token,
		body: map[string]string{}}).
		data(http.StatusCreated, &created)

	assert.Nil(t, created.Label, "the label is optional")
	assert.NotEmpty(t, created.APIKey)
}

// The per-key GCRA allowance protects operational routes, and one instance
// exhausting its quota must not affect another (constitution, principle II).
func TestOperationalRateLimitIsEnforcedPerKey(t *testing.T) {
	f := newFixture(t, func(c *config.Config) { c.OperationalRateLimit = 5 })
	acme := f.newTenant(f.platformToken(), "ACME Corp", "alice@acme.com", "senhaAlice1")

	first := f.createInstance(acme.token, "vendas-01")
	second := f.createInstance(acme.token, "suporte-01")
	firstKey := f.createKey(acme.token, first.ID, "produção")
	secondKey := f.createKey(acme.token, second.ID, "produção")

	// Spend the first key's allowance until it is refused.
	var limited bool
	for range 20 {
		response := f.do(request{method: http.MethodGet,
			path: "/instances/" + first.ID + "/whoami", apiKey: firstKey.APIKey})
		if response.Status == http.StatusTooManyRequests {
			response.problem(http.StatusTooManyRequests, "RATE_LIMIT_EXCEEDED")
			limited = true
			break
		}
	}
	require.True(t, limited, "the per-key allowance must eventually refuse a burst")

	// The other instance's key is untouched by that burst.
	f.do(request{method: http.MethodGet, path: "/instances/" + second.ID + "/whoami", apiKey: secondKey.APIKey}).
		data(http.StatusOK, nil)
}
