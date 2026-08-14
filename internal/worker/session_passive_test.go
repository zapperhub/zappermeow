package worker_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/store"
	"github.com/zapperhub/zappermeow/internal/wa"
)

// The property under test is ordering, not counting: the library restores
// active mode on every connection, before it reports Connected, so the platform
// must reapply after each one. A test that only checked "SetPassive was called"
// would pass against an implementation that applies it once and then loses it
// on the first reconnect (research R6).
func TestPassiveModeIsAppliedAfterConnecting(t *testing.T) {
	h := newHarness(t)
	h.setPassiveMode(t, true)

	session := h.connected(t)

	requirePassiveApplied(t, session)
	assert.True(t, session.Passive())
}

// Reconnecting is where the naive implementation breaks: the session comes back
// active and stays that way unless the mode is pushed again.
func TestPassiveModeIsReappliedOnEveryConnect(t *testing.T) {
	h := newHarness(t)
	h.setPassiveMode(t, true)

	session := h.connected(t)
	requirePassiveApplied(t, session)

	// A transient drop and the reconnection that follows it.
	session.EmitDisconnect(domain.ReasonNetwork)
	h.waitForTrail(t, string(domain.ConnEventDisconnected))

	require.NoError(t, session.Connect(h.ctx))

	require.Eventually(t, func() bool {
		return countCalls(session, wa.CallSetPassive) >= 2
	}, 20*time.Second, 25*time.Millisecond,
		"passive mode must be pushed again after the reconnect (calls: %v)", session.CallNames())
}

// Every SetPassive must come after a Connect, never before: the call is an IQ
// and there is no socket to send it on until the session is up.
func TestPassiveModeIsNeverAppliedBeforeConnecting(t *testing.T) {
	h := newHarness(t)
	h.setPassiveMode(t, true)

	session := h.connected(t)
	requirePassiveApplied(t, session)

	names := session.CallNames()
	seenConnect := false
	for _, name := range names {
		if name == wa.CallConnect {
			seenConnect = true
		}
		if name == wa.CallSetPassive {
			assert.True(t, seenConnect, "SetPassive ran before any Connect (calls: %v)", names)
		}
	}
}

// With passive mode off there is nothing to push: the library already leaves
// every connection active, so an extra call would be noise on the wire.
func TestPassiveModeOffIssuesNoCall(t *testing.T) {
	h := newHarness(t)

	session := h.connected(t)
	h.waitForTrail(t, string(domain.ConnEventConnected))

	assert.Zero(t, countCalls(session, wa.CallSetPassive))
	assert.False(t, session.Passive())
}

// A failover produces a fresh session, and it must come up passive too — the
// stored choice belongs to the instance, not to the process that held it.
func TestPassiveModeSurvivesAFailover(t *testing.T) {
	h := newHarness(t)
	h.setPassiveMode(t, true)

	first := h.connected(t)
	requirePassiveApplied(t, first)

	h.supervisor.Drop(h.ctx, h.instanceID)
	require.NoError(t, h.leases.Release(h.ctx, h.instanceID))

	replacement := h.adopt(t)
	require.NoError(t, replacement.Connect(h.ctx))

	require.Eventually(t, func() bool {
		return replacement.Passive()
	}, 20*time.Second, 25*time.Millisecond,
		"the replacement session must come up passive (calls: %v)", replacement.CallNames())
}

// A failing SetPassive must not take the number offline: the session is up and
// working, and a late setting is a smaller problem than a dropped connection.
func TestPassiveModeFailureDoesNotDropTheSession(t *testing.T) {
	h := newHarness(t)
	h.setPassiveMode(t, true)

	session := h.adopt(t)
	session.SetPassiveErr = assertErr{}

	_, err := h.supervisor.Connect(h.ctx, h.instanceID)
	require.NoError(t, err)
	session.EmitPairSuccess(domain.DeviceIdentity{
		JID: "5511999999999:11@s.whatsapp.net", PhoneNumber: "5511999999999",
	})

	h.waitForState(t, domain.InstanceConnected)
	assert.Equal(t, string(domain.InstanceConnected), h.instance(t).ConnectionState)
}

// FR-014: passive mode changes how the device announces itself, not what it
// receives. Events must keep flowing through the same path afterwards.
func TestPassiveModeKeepsEventsFlowing(t *testing.T) {
	h := newHarness(t)
	h.setPassiveMode(t, true)

	session := h.connected(t)
	requirePassiveApplied(t, session)

	// An event scripted after the mode is in force still reaches the trail and
	// the persisted state, which is the whole processing path.
	session.EmitStreamError("500")

	h.waitForTrail(t, string(domain.ConnEventDisconnected))
	row := h.instance(t)
	require.NotNil(t, row.LastDisconnectReason)
	assert.Equal(t, string(domain.ReasonStreamError), *row.LastDisconnectReason)
	assert.True(t, session.Passive(), "processing an event must not reset the mode")
}

// --- helpers ---

func (h *harness) setPassiveMode(t *testing.T, enabled bool) {
	t.Helper()
	require.NoError(t, h.infra.Queries.SetInstancePassiveMode(h.ctx, store.SetInstancePassiveModeParams{
		ID:          h.instanceID,
		PassiveMode: enabled,
	}))
}

func requirePassiveApplied(t *testing.T, session *wa.FakeSession) {
	t.Helper()
	require.Eventually(t, func() bool {
		return countCalls(session, wa.CallSetPassive) >= 1
	}, 20*time.Second, 25*time.Millisecond,
		"passive mode was never applied (calls: %v)", session.CallNames())
}

func countCalls(session *wa.FakeSession, name string) int {
	count := 0
	for _, call := range session.CallNames() {
		if call == name {
			count++
		}
	}
	return count
}

// assertErr is a stand-in failure for commands that must not be fatal.
type assertErr struct{}

func (assertErr) Error() string { return "scripted failure" }
