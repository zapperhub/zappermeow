package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// proxyData mirrors the payload of the proxy endpoints.
type proxyData struct {
	ProxyURL     *string `json:"proxy_url"`
	Reconnecting bool    `json:"reconnecting"`
}

type passiveModeData struct {
	PassiveMode bool `json:"passive_mode"`
	Applied     bool `json:"applied"`
}

func TestSetProxyStoresAndMasksTheCredentials(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	var data proxyData
	f.do(request{
		method: http.MethodPut,
		path:   "/instances/" + setup.instanceID + "/proxy",
		token:  setup.tenant.token,
		body:   map[string]any{"url": "socks5://user:s3cret@203.0.113.10:1080"},
	}).data(http.StatusOK, &data)

	require.NotNil(t, data.ProxyURL)
	assert.Equal(t, "socks5://user:***@203.0.113.10:1080", *data.ProxyURL)
	// No worker is running in these tests, so nothing could be relinked.
	assert.False(t, data.Reconnecting)

	// The raw value is what the worker needs, so it is what gets stored.
	var stored string
	require.NoError(t, f.infra.Pool.QueryRow(t.Context(),
		`SELECT proxy_url FROM instances WHERE id = $1`, setup.instanceID).Scan(&stored))
	assert.Equal(t, "socks5://user:s3cret@203.0.113.10:1080", stored)
}

// SC-007: the password must not appear in any byte the platform sends back —
// not in the write, not in the read, not in the trail.
func TestProxyPasswordNeverLeavesThePlatform(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	const password = "sup3rs3cret"

	write := f.do(request{
		method: http.MethodPut,
		path:   "/instances/" + setup.instanceID + "/proxy",
		token:  setup.tenant.token,
		body:   map[string]any{"url": "socks5://user:" + password + "@203.0.113.10:1080"},
	})
	require.Equal(t, http.StatusOK, write.Status)
	assert.NotContains(t, string(write.Body), password)

	read := f.do(request{
		method: http.MethodGet,
		path:   "/instances/" + setup.instanceID + "/connection",
		token:  setup.tenant.token,
	})
	require.Equal(t, http.StatusOK, read.Status)
	assert.NotContains(t, string(read.Body), password)
	assert.Contains(t, string(read.Body), "***")

	trail := f.do(request{
		method: http.MethodGet,
		path:   "/instances/" + setup.instanceID + "/connection/events",
		token:  setup.tenant.token,
	})
	require.Equal(t, http.StatusOK, trail.Status)
	assert.NotContains(t, string(trail.Body), password)
	assert.Contains(t, string(trail.Body), "proxy_updated")
}

func TestSetProxyRejectsMalformedAddresses(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	tests := []struct {
		name string
		url  string
		code string
	}{
		{name: "unsupported scheme", url: "ftp://proxy.internal:21", code: "UNSUPPORTED_PROXY_SCHEME"},
		{name: "socks4 is not dialable", url: "socks4://203.0.113.10:1080", code: "UNSUPPORTED_PROXY_SCHEME"},
		{name: "no scheme", url: "proxy.internal:3128", code: "INVALID_PROXY_URL"},
		{name: "no host", url: "http://", code: "INVALID_PROXY_URL"},
		{name: "empty", url: "", code: "VALIDATION_ERROR"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f.do(request{
				method: http.MethodPut,
				path:   "/instances/" + setup.instanceID + "/proxy",
				token:  setup.tenant.token,
				body:   map[string]any{"url": tc.url},
			}).problem(http.StatusUnprocessableEntity, tc.code)
		})
	}

	// A rejected write leaves the stored configuration untouched.
	var stored *string
	require.NoError(t, f.infra.Pool.QueryRow(t.Context(),
		`SELECT proxy_url FROM instances WHERE id = $1`, setup.instanceID).Scan(&stored))
	assert.Nil(t, stored)
}

func TestClearProxyIsIdempotent(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	for range 2 {
		var data proxyData
		f.do(request{
			method: http.MethodDelete,
			path:   "/instances/" + setup.instanceID + "/proxy",
			token:  setup.tenant.token,
		}).data(http.StatusOK, &data)
		assert.Nil(t, data.ProxyURL)
	}

	var stored *string
	require.NoError(t, f.infra.Pool.QueryRow(t.Context(),
		`SELECT proxy_url FROM instances WHERE id = $1`, setup.instanceID).Scan(&stored))
	assert.Nil(t, stored)
}

// The proxy routes are connection routes: an API key drives them just like the
// tenant token, and neither opens another tenant's instance (FR-025).
func TestProxyRoutesFollowConnectionIsolation(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")
	other := f.newConnectionSetup(t, "Globex", "bob@globex.com")

	var data proxyData
	f.do(request{
		method: http.MethodPut,
		path:   "/instances/" + setup.instanceID + "/proxy",
		apiKey: setup.key,
		body:   map[string]any{"url": "http://proxy.internal:3128"},
	}).data(http.StatusOK, &data)
	require.NotNil(t, data.ProxyURL)

	// Another tenant's instance is not confirmed to exist.
	f.do(request{
		method: http.MethodPut,
		path:   "/instances/" + other.instanceID + "/proxy",
		token:  setup.tenant.token,
		body:   map[string]any{"url": "http://proxy.internal:3128"},
	}).problem(http.StatusNotFound, "RESOURCE_NOT_FOUND")

	// A key belongs to one instance and opens nothing else.
	f.do(request{
		method: http.MethodPut,
		path:   "/instances/" + other.instanceID + "/proxy",
		apiKey: setup.key,
		body:   map[string]any{"url": "http://proxy.internal:3128"},
	}).problem(http.StatusNotFound, "RESOURCE_NOT_FOUND")
}

func TestPassiveModeIsStoredAndReported(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	var data passiveModeData
	f.do(request{
		method: http.MethodPut,
		path:   "/instances/" + setup.instanceID + "/passive-mode",
		token:  setup.tenant.token,
		body:   map[string]any{"enabled": true},
	}).data(http.StatusOK, &data)

	assert.True(t, data.PassiveMode)
	// Nothing is connected, so nothing was applied — the value waits for the
	// next connection rather than failing (research R6).
	assert.False(t, data.Applied)

	status := f.do(request{
		method: http.MethodGet,
		path:   "/instances/" + setup.instanceID + "/connection",
		token:  setup.tenant.token,
	})
	require.Equal(t, http.StatusOK, status.Status)
	assert.True(t, strings.Contains(string(status.Body), `"passive_mode":true`))
}

func TestPassiveModeDefaultsToOff(t *testing.T) {
	f := newFixture(t)
	setup := f.newConnectionSetup(t, "ACME Corp", "alice@acme.com")

	status := f.do(request{
		method: http.MethodGet,
		path:   "/instances/" + setup.instanceID + "/connection",
		token:  setup.tenant.token,
	})
	require.Equal(t, http.StatusOK, status.Status)
	assert.Contains(t, string(status.Body), `"passive_mode":false`)
	assert.Contains(t, string(status.Body), `"proxy_url":null`)
}
