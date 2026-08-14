package worker_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/worker"
)

// The proxy reaches the client only through the session factory, so this is
// where "the stored value is the one used" can actually be proved. Everything
// else about the proxy — masking, validation, the HTTP contract — is downstream
// of this one fact.
func TestSessionIsBuiltWithTheStoredProxy(t *testing.T) {
	h := newHarness(t)
	const proxyURL = "socks5://user:s3cret@203.0.113.10:1080"
	h.setProxy(t, proxyURL)

	h.adopt(t)

	configs := h.factory.Configs()
	require.Len(t, configs, 1)
	// The raw URL, credentials included: the worker is the one place that needs
	// them, and it gets them from the database rather than from a command.
	assert.Equal(t, proxyURL, configs[0].ProxyURL)
}

// An instance with no proxy must be built with an empty one rather than with
// "whatever the environment says". The library resolves https_proxy on its own
// when left alone, so a worker started behind a proxy would otherwise route
// every tenant through it (FR-006, research R1).
func TestSessionWithoutProxyIsBuiltWithoutOne(t *testing.T) {
	h := newHarness(t)
	h.adopt(t)

	configs := h.factory.Configs()
	require.Len(t, configs, 1)
	assert.Empty(t, configs[0].ProxyURL)
}

// Rebuilding after a proxy change must pick up the new value, and it must do so
// by rereading the database — which is also what a failover does.
func TestRelinkRebuildsTheSessionWithTheNewProxy(t *testing.T) {
	h := newHarness(t)
	h.setProxy(t, "http://old.proxy.internal:3128")
	h.connected(t)

	h.setProxy(t, "socks5://new.proxy.internal:1080")
	result, err := h.supervisor.ApplySettings(h.ctx, h.instanceID, true, false)
	require.NoError(t, err)
	assert.True(t, result.Reconnecting)

	configs := h.factory.Configs()
	require.Len(t, configs, 2, "the session must be rebuilt, not mutated")
	assert.Equal(t, "http://old.proxy.internal:3128", configs[0].ProxyURL)
	assert.Equal(t, "socks5://new.proxy.internal:1080", configs[1].ProxyURL)

	// The tenant sees the transition rather than a silent gap.
	h.waitForTrail(t, string(domain.ConnEventDisconnected))
}

// Removing the proxy is the same path in reverse: the rebuilt session must come
// up direct.
func TestRelinkRebuildsWithoutProxyWhenItIsCleared(t *testing.T) {
	h := newHarness(t)
	h.setProxy(t, "http://proxy.internal:3128")
	h.connected(t)

	h.clearProxy(t)
	_, err := h.supervisor.ApplySettings(h.ctx, h.instanceID, true, false)
	require.NoError(t, err)

	configs := h.factory.Configs()
	require.Len(t, configs, 2)
	assert.Empty(t, configs[1].ProxyURL, "a cleared proxy must rebuild as a direct connection")
}

// A pairing attempt cannot survive a relink: the QR on screen was minted by the
// socket that is about to be dropped. Ending it explicitly is what tells the
// tenant to start again, instead of leaving them staring at a code that
// silently stopped working (research R2).
func TestRelinkEndsAPairingAttemptInFlight(t *testing.T) {
	h := newHarness(t)
	session := h.adopt(t)

	_, err := h.supervisor.Connect(h.ctx, h.instanceID)
	require.NoError(t, err)
	session.EmitPairingCode("qr-code-1", true)
	h.waitForState(t, domain.InstancePairing)

	h.setProxy(t, "socks5://203.0.113.10:1080")
	_, err = h.supervisor.ApplySettings(h.ctx, h.instanceID, true, false)
	require.NoError(t, err)

	h.waitForTrail(t, string(domain.ConnEventPairingExpired))
}

// ApplySettings acts on a session this worker holds. Asking about one it does
// not know is not a silent no-op: the API needs to hear that the command went
// nowhere so it can report the setting as stored-but-not-applied.
func TestApplySettingsRefusesAnUnownedInstance(t *testing.T) {
	h := newHarness(t)

	_, err := h.supervisor.ApplySettings(h.ctx, h.instanceID, true, false)
	require.ErrorIs(t, err, worker.ErrUnknownInstance)
}

// A failover must not quietly drop the proxy: the replacement session is built
// from the same stored configuration as the one it replaces.
func TestAdoptionAfterDropKeepsTheProxy(t *testing.T) {
	h := newHarness(t)
	const proxyURL = "socks5://203.0.113.10:1080"
	h.setProxy(t, proxyURL)

	h.adopt(t)
	// Losing and reacquiring the lease is what a failover looks like from this
	// process: the replacement session is built from scratch.
	h.supervisor.Drop(h.ctx, h.instanceID)
	require.NoError(t, h.leases.Release(h.ctx, h.instanceID))
	h.adopt(t)

	configs := h.factory.Configs()
	require.Len(t, configs, 2)
	assert.Equal(t, proxyURL, configs[1].ProxyURL)
}

// A failing connection with a proxy in force is reported as a proxy failure and
// never retried without it. The reason is what the tenant needs in order to
// know which of the two things to go and fix.
func TestProxyConnectFailureIsRecordedAsSuch(t *testing.T) {
	h := newHarness(t)
	h.setProxy(t, "socks5://unreachable.internal:1080")
	session := h.connected(t)

	session.EmitDisconnect(domain.ReasonProxyConnectFailed)

	h.waitForTrail(t, string(domain.ConnEventDisconnected))
	row := h.instance(t)
	require.NotNil(t, row.LastDisconnectReason)
	assert.Equal(t, string(domain.ReasonProxyConnectFailed), *row.LastDisconnectReason)

	// Not permanent: the instance keeps trying, and it keeps trying through the
	// proxy. A direct fallback would leak the platform's own address (FR-005).
	assert.False(t, domain.ReasonProxyConnectFailed.Permanent())
	assert.Equal(t, proxyURL(t, h), *row.ProxyUrl, "a failure must never clear the configured proxy")
}

func proxyURL(t *testing.T, h *harness) string {
	t.Helper()
	row := h.instance(t)
	require.NotNil(t, row.ProxyUrl)
	return *row.ProxyUrl
}
