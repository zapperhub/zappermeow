package api_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zapperhub/zappermeow/internal/config"
)

// connectData is the payload of a connect command.
type connectData struct {
	InstanceID string `json:"instance_id"`
	State      string `json:"state"`
	Intent     string `json:"intent"`
	Pairing    *struct {
		Method    string  `json:"method"`
		ExpiresAt *string `json:"expires_at"`
	} `json:"pairing"`
}

type lifecycleData struct {
	InstanceID string `json:"instance_id"`
	State      string `json:"state"`
	Intent     string `json:"intent"`
	LogoutMode string `json:"logout_mode"`
}

// connectionSetup is a tenant with one instance and a key for it.
type connectionSetup struct {
	tenant     tenantSetup
	instanceID string
	key        string
}

func (f *fixture) newConnectionSetup(t *testing.T, tenantName, email string) connectionSetup {
	t.Helper()

	tenant := f.newTenant(f.platformToken(), tenantName, email, "senhaForte1")
	instance := f.createInstance(tenant.token, "vendas-01")
	key := f.createKey(tenant.token, instance.ID, "integracao")

	return connectionSetup{tenant: tenant, instanceID: instance.ID, key: key.APIKey}
}

// These routes accept either credential (FR-039). With no worker holding the
// session the command records the intent and wakes the fleet, answering 202 —
// the QR code arrives over the event channel, never in this response.

func TestConnectAcceptsATenantToken(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	var data connectData
	f.do(request{
		method: http.MethodPost,
		path:   "/instances/" + setup.instanceID + "/connect",
		token:  setup.tenant.token,
	}).data(http.StatusAccepted, &data)

	assert.Equal(t, setup.instanceID, data.InstanceID)
	assert.Equal(t, "active", data.Intent, "connect records the intent to be online")
}

func TestConnectAcceptsAnInstanceAPIKey(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	var data connectData
	f.do(request{
		method: http.MethodPost,
		path:   "/instances/" + setup.instanceID + "/connect",
		apiKey: setup.key,
	}).data(http.StatusAccepted, &data)

	assert.Equal(t, "active", data.Intent,
		"an integration must bring a number online with no human logged in")
}

// Isolation: a key belongs to one instance and opens nothing else, not even a
// sibling under the same tenant (FR-040).
func TestConnectRefusesAKeyFromAnotherInstance(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")
	sibling := f.createInstance(setup.tenant.token, "vendas-02")

	f.do(request{
		method: http.MethodPost,
		path:   "/instances/" + sibling.ID + "/connect",
		apiKey: setup.key,
	}).problem(http.StatusNotFound, "RESOURCE_NOT_FOUND")
}

func TestConnectRefusesAnotherTenantsInstance(t *testing.T) {
	f := newFixture(t)
	owner := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")
	intruder := f.newTenant(f.platformToken(), "Globex", "bob@globex.com", "senhaBob123")

	f.do(request{
		method: http.MethodPost,
		path:   "/instances/" + owner.instanceID + "/connect",
		token:  intruder.token,
	}).problem(http.StatusNotFound, "RESOURCE_NOT_FOUND")
}

func TestConnectionRoutesRequireACredential(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	for _, action := range []string{"/connect", "/disconnect", "/logout"} {
		resp := f.do(request{
			method: http.MethodPost,
			path:   "/instances/" + setup.instanceID + action,
		})
		assert.Equal(t, http.StatusUnauthorized, resp.Status, "route %s", action)
	}
}

func TestDisconnectIsIdempotentWithoutAWorker(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	// Repeating a command in the state already reached is accepted with no side
	// effect (FR-008).
	for range 2 {
		var data lifecycleData
		f.do(request{
			method: http.MethodPost,
			path:   "/instances/" + setup.instanceID + "/disconnect",
			token:  setup.tenant.token,
		}).data(http.StatusAccepted, &data)

		assert.Equal(t, "disconnected", data.State)
		assert.Equal(t, "stopped", data.Intent)
	}
}

// Logging out an instance that was never paired is already the requested end
// state, not an error.
func TestLogoutOnUnpairedInstance(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	var data lifecycleData
	f.do(request{
		method: http.MethodPost,
		path:   "/instances/" + setup.instanceID + "/logout",
		token:  setup.tenant.token,
	}).data(http.StatusAccepted, &data)

	assert.Equal(t, "registered", data.State)
	assert.Equal(t, "local_only", data.LogoutMode,
		"nothing was removed on the server because nothing was paired")
}

// Pairing by phone must reach a worker: it returns a code the caller needs now,
// so with no worker available the answer is a distinct 503.
func TestPairPhoneWithoutAWorkerIsUnavailable(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	f.do(request{
		method: http.MethodPost,
		path:   "/instances/" + setup.instanceID + "/pair-phone",
		token:  setup.tenant.token,
		body:   map[string]any{"phone_number": "5511999999999"},
	}).problem(http.StatusServiceUnavailable, "SESSION_UNAVAILABLE")
}

// The constitution requires a distributed limit on every operational route, and
// these accept an instance key — so they are operational (principle II).
func TestConnectionRoutesAreRateLimited(t *testing.T) {
	f := newFixture(t, func(cfg *config.Config) { cfg.OperationalRateLimit = 2 })
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	var limited bool
	for range 8 {
		resp := f.do(request{
			method: http.MethodPost,
			path:   "/instances/" + setup.instanceID + "/disconnect",
			apiKey: setup.key,
		})
		if resp.Status == http.StatusTooManyRequests {
			limited = true
			resp.problem(http.StatusTooManyRequests, "RATE_LIMIT_EXCEEDED")
			break
		}
	}
	assert.True(t, limited, "an unbounded connection route lets one tenant starve the platform")
}

// The quota follows the credential, so one instance cannot spend another's.
func TestConnectionRateLimitIsPerInstance(t *testing.T) {
	f := newFixture(t, func(cfg *config.Config) { cfg.OperationalRateLimit = 2 })
	noisy := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")
	quietInstance := f.createInstance(noisy.tenant.token, "vendas-02")
	quietKey := f.createKey(noisy.tenant.token, quietInstance.ID, "quieta")

	for range 8 {
		f.do(request{
			method: http.MethodPost,
			path:   "/instances/" + noisy.instanceID + "/disconnect",
			apiKey: noisy.key,
		})
	}

	resp := f.do(request{
		method: http.MethodPost,
		path:   "/instances/" + quietInstance.ID + "/disconnect",
		apiKey: quietKey.APIKey,
	})
	assert.Equal(t, http.StatusAccepted, resp.Status,
		"a neighbour burning its own quota must not affect this instance")
}

// A suspended tenant loses access to every connection route, whichever
// credential it presents (FR-041).
func TestConnectionRoutesRefuseASuspendedTenant(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	f.do(request{
		method: http.MethodPost,
		path:   "/admin/tenants/" + setup.tenant.tenant.ID + "/suspend",
		token:  f.platformToken(),
	}).data(http.StatusOK, nil)

	resp := f.do(request{
		method: http.MethodPost,
		path:   "/instances/" + setup.instanceID + "/connect",
		apiKey: setup.key,
	})
	require.Equal(t, http.StatusForbidden, resp.Status, "body: %s", resp.Body)
	resp.problem(http.StatusForbidden, "TENANT_SUSPENDED")
}

// FR-007: deleting an instance must not leave a session behind. Without a
// worker there is nothing to end, but the deletion still has to go through —
// a tenant unable to remove a record would be worse than an orphan session.
func TestDeleteInstanceWithoutAWorkerStillSucceeds(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	resp := f.do(request{
		method: http.MethodDelete,
		path:   "/instances/" + setup.instanceID,
		token:  setup.tenant.token,
	})
	require.Equal(t, http.StatusNoContent, resp.Status, "body: %s", resp.Body)

	// The cascade removes the lease and the trail with the instance.
	var leases int
	require.NoError(t, f.infra.Pool.QueryRow(t.Context(),
		`SELECT count(*) FROM session_leases WHERE instance_id = $1`, setup.instanceID).Scan(&leases))
	assert.Zero(t, leases, "a deleted instance must not leave a lease behind")

	// And the key stops working immediately.
	f.do(request{
		method: http.MethodPost,
		path:   "/instances/" + setup.instanceID + "/connect",
		apiKey: setup.key,
	}).problem(http.StatusUnauthorized, "UNAUTHENTICATED")
}

// Deleting an instance that had recorded connection history must take the trail
// with it rather than leave rows pointing at nothing.
func TestDeleteInstanceRemovesItsConnectionTrail(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	// A connect leaves a lease row and, once a worker acts, trail entries.
	f.do(request{
		method: http.MethodPost,
		path:   "/instances/" + setup.instanceID + "/connect",
		token:  setup.tenant.token,
	}).data(http.StatusAccepted, nil)

	_, err := f.infra.Pool.Exec(t.Context(),
		`INSERT INTO connection_events (instance_id, type) VALUES ($1, 'connected')`, setup.instanceID)
	require.NoError(t, err)

	f.do(request{
		method: http.MethodDelete,
		path:   "/instances/" + setup.instanceID,
		token:  setup.tenant.token,
	})

	var events int
	require.NoError(t, f.infra.Pool.QueryRow(t.Context(),
		`SELECT count(*) FROM connection_events WHERE instance_id = $1`, setup.instanceID).Scan(&events))
	assert.Zero(t, events)
}

type connectionStatusData struct {
	InstanceID  string  `json:"instance_id"`
	State       string  `json:"state"`
	Intent      string  `json:"intent"`
	ConnectedAt *string `json:"connected_at"`
	Device      *struct {
		JID         string `json:"jid"`
		PhoneNumber string `json:"phone_number"`
		PushName    string `json:"push_name"`
	} `json:"device"`
	LastDisconnect *struct {
		At     string `json:"at"`
		Reason string `json:"reason"`
	} `json:"last_disconnect"`
	SharesNumberWith []string `json:"shares_number_with"`
}

type trailData struct {
	Events []struct {
		Type       string         `json:"type"`
		Reason     string         `json:"reason"`
		Detail     map[string]any `json:"detail"`
		OccurredAt string         `json:"occurred_at"`
	} `json:"events"`
	NextBefore string `json:"next_before"`
}

// US3 scenario 4: an instance that was never paired answers 200 with its state,
// not an error. Absence of a device is information, not a failure.
func TestConnectionStatusOnAnUnpairedInstance(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	var data connectionStatusData
	f.do(request{
		method: http.MethodGet,
		path:   "/instances/" + setup.instanceID + "/connection",
		token:  setup.tenant.token,
	}).data(http.StatusOK, &data)

	assert.Equal(t, "registered", data.State)
	assert.Equal(t, "stopped", data.Intent)
	assert.Nil(t, data.Device)
	assert.Nil(t, data.LastDisconnect)
	assert.Empty(t, data.SharesNumberWith)
}

// US3 scenario 2: after a drop the tenant must be able to see why.
func TestConnectionStatusReportsTheLastDisconnect(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	_, err := f.infra.Pool.Exec(t.Context(), `
		UPDATE instances
		SET connection_state = 'connecting',
		    wa_jid = '5511999999999:11@s.whatsapp.net',
		    phone_number = '5511999999999',
		    push_name = 'Suporte ACME',
		    paired_at = now(),
		    last_disconnect_at = now(),
		    last_disconnect_reason = 'network'
		WHERE id = $1`, setup.instanceID)
	require.NoError(t, err)

	var data connectionStatusData
	f.do(request{
		method: http.MethodGet,
		path:   "/instances/" + setup.instanceID + "/connection",
		apiKey: setup.key,
	}).data(http.StatusOK, &data)

	assert.Equal(t, "connecting", data.State)
	require.NotNil(t, data.Device)
	assert.Equal(t, "5511999999999:11@s.whatsapp.net", data.Device.JID,
		"the companion device suffix must reach the tenant intact")
	require.NotNil(t, data.LastDisconnect)
	assert.Equal(t, "network", data.LastDisconnect.Reason)
}

// FR-018: several companion devices of one number is legitimate, so siblings
// are reported as context rather than flagged as a conflict.
func TestConnectionStatusListsInstancesSharingTheNumber(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")
	sibling := f.createInstance(setup.tenant.token, "vendas-02")

	for id, jid := range map[string]string{
		setup.instanceID: "5511999999999:11@s.whatsapp.net",
		sibling.ID:       "5511999999999:12@s.whatsapp.net",
	} {
		_, err := f.infra.Pool.Exec(t.Context(),
			`UPDATE instances SET wa_jid = $2, phone_number = '5511999999999' WHERE id = $1`, id, jid)
		require.NoError(t, err)
	}

	var data connectionStatusData
	f.do(request{
		method: http.MethodGet,
		path:   "/instances/" + setup.instanceID + "/connection",
		token:  setup.tenant.token,
	}).data(http.StatusOK, &data)

	assert.Equal(t, []string{sibling.ID}, data.SharesNumberWith)
}

func TestConnectionStatusRefusesAnotherTenant(t *testing.T) {
	f := newFixture(t)
	owner := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")
	intruder := f.newTenant(f.platformToken(), "Globex", "bob@globex.com", "senhaBob123")

	f.do(request{
		method: http.MethodGet,
		path:   "/instances/" + owner.instanceID + "/connection",
		token:  intruder.token,
	}).problem(http.StatusNotFound, "RESOURCE_NOT_FOUND")
}

// US3 scenario 3: the trail comes back newest first, with reasons attached.
func TestConnectionTrailIsNewestFirst(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	for _, entry := range []struct{ eventType, reason string }{
		{"pairing_started", ""},
		{"pairing_succeeded", ""},
		{"connected", ""},
		{"disconnected", "network"},
	} {
		var reason any
		if entry.reason != "" {
			reason = entry.reason
		}
		_, err := f.infra.Pool.Exec(t.Context(),
			`INSERT INTO connection_events (instance_id, type, reason) VALUES ($1, $2, $3)`,
			setup.instanceID, entry.eventType, reason)
		require.NoError(t, err)
	}

	var data trailData
	f.do(request{
		method: http.MethodGet,
		path:   "/instances/" + setup.instanceID + "/connection/events",
		token:  setup.tenant.token,
	}).data(http.StatusOK, &data)

	require.Len(t, data.Events, 4)
	assert.Equal(t, "disconnected", data.Events[0].Type, "the newest entry comes first")
	assert.Equal(t, "network", data.Events[0].Reason)
	assert.Equal(t, "pairing_started", data.Events[3].Type)
	assert.Empty(t, data.NextBefore, "a partial page has nothing more to fetch")
}

// The cursor has to be stable when several entries share a timestamp, which is
// exactly what a burst of reconnections produces: ordering then falls back to
// the row id, and pages must not overlap.
func TestConnectionTrailPaginates(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	// Same instant on purpose; the reason is what makes each row identifiable.
	reasons := []string{"network", "keepalive_timeout", "worker_lost", "user_requested", "temporary_ban"}
	for _, reason := range reasons {
		_, err := f.infra.Pool.Exec(t.Context(),
			`INSERT INTO connection_events (instance_id, type, reason, occurred_at)
			 VALUES ($1, 'disconnected', $2, '2026-08-13T10:00:00Z')`,
			setup.instanceID, reason)
		require.NoError(t, err)
	}

	seen := map[string]bool{}
	cursor := ""
	for range 3 {
		path := "/instances/" + setup.instanceID + "/connection/events?limit=2"
		if cursor != "" {
			path += "&before=" + cursor
		}

		var page trailData
		f.do(request{method: http.MethodGet, path: path, token: setup.tenant.token}).
			data(http.StatusOK, &page)

		for _, event := range page.Events {
			require.False(t, seen[event.Reason], "page overlap: %q came back twice", event.Reason)
			seen[event.Reason] = true
		}
		cursor = page.NextBefore
		if cursor == "" {
			break
		}
	}

	assert.Len(t, seen, len(reasons), "paginating must walk the whole trail exactly once")
}

func TestConnectionTrailFiltersByType(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	for _, eventType := range []string{"connected", "disconnected", "connected"} {
		_, err := f.infra.Pool.Exec(t.Context(),
			`INSERT INTO connection_events (instance_id, type) VALUES ($1, $2)`, setup.instanceID, eventType)
		require.NoError(t, err)
	}

	var data trailData
	f.do(request{
		method: http.MethodGet,
		path:   "/instances/" + setup.instanceID + "/connection/events?type=disconnected",
		token:  setup.tenant.token,
	}).data(http.StatusOK, &data)

	require.Len(t, data.Events, 1)
	assert.Equal(t, "disconnected", data.Events[0].Type)
}

// A malformed cursor is a client error. Silently restarting from the top would
// loop a paginating client forever without it ever noticing.
func TestConnectionTrailRejectsABrokenCursor(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	f.do(request{
		method: http.MethodGet,
		path:   "/instances/" + setup.instanceID + "/connection/events?before=not-a-cursor",
		token:  setup.tenant.token,
	}).problem(http.StatusUnprocessableEntity, "VALIDATION_ERROR")
}

func TestConnectionTrailRefusesAnotherTenant(t *testing.T) {
	f := newFixture(t)
	owner := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")
	intruder := f.newTenant(f.platformToken(), "Globex", "bob@globex.com", "senhaBob123")

	f.do(request{
		method: http.MethodGet,
		path:   "/instances/" + owner.instanceID + "/connection/events",
		token:  intruder.token,
	}).problem(http.StatusNotFound, "RESOURCE_NOT_FOUND")
}
