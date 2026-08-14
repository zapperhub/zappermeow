package worker_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/lease"
	"github.com/zapperhub/zappermeow/internal/metrics"
	"github.com/zapperhub/zappermeow/internal/store"
)

// US5: every invalidation lands on its own state with its own reason, and none
// of them keeps the system retrying.
func TestInvalidationReachesTheRightTerminalState(t *testing.T) {
	tests := []struct {
		name      string
		reason    domain.DisconnectReason
		wantState domain.InstanceState
		wantTrail string
	}{
		{
			name:      "unlinked from the handset",
			reason:    domain.ReasonLoggedOutFromPhone,
			wantState: domain.InstanceLoggedOut,
			wantTrail: "logged_out",
		},
		{
			name:      "session opened elsewhere",
			reason:    domain.ReasonSessionReplaced,
			wantState: domain.InstanceDisconnected,
			wantTrail: "disconnected",
		},
		{
			name:      "client version rejected",
			reason:    domain.ReasonClientOutdated,
			wantState: domain.InstanceDisconnected,
			wantTrail: "disconnected",
		},
		{
			name:      "unknown connect failure",
			reason:    domain.ReasonConnectFailure,
			wantState: domain.InstanceDisconnected,
			wantTrail: "disconnected",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			session := h.connected(t)

			session.EmitDisconnect(tc.reason)

			row := h.waitForState(t, tc.wantState)
			require.NotNil(t, row.LastDisconnectReason)
			assert.Equal(t, string(tc.reason), *row.LastDisconnectReason,
				"the tenant must see which failure it was")
			h.waitForTrail(t, tc.wantTrail)

			// The session is released and, above all, the instance stops being
			// adoptable: retrying a dead session is what US5 forbids.
			assert.Eventually(t, func() bool { return !h.supervisor.Owns(h.instanceID) },
				10*time.Second, 25*time.Millisecond)

			adoptable, err := h.leases.Adoptable(h.ctx, 10)
			require.NoError(t, err)
			assert.NotContains(t, adoptable, h.instanceID)
		})
	}
}

// A network drop is not an invalidation: the system keeps the session and keeps
// trying, which is the whole point of telling the two apart.
func TestTransientDropKeepsRetrying(t *testing.T) {
	h := newHarness(t)
	session := h.connected(t)

	session.EmitDisconnect(domain.ReasonNetwork)

	h.waitForState(t, domain.InstanceConnecting)
	assert.True(t, h.supervisor.Owns(h.instanceID), "a transient drop must not release the session")

	adoptable, err := h.leases.Adoptable(h.ctx, 10)
	require.NoError(t, err)
	assert.NotContains(t, adoptable, h.instanceID, "still owned, so nobody else should take it")
}

// FR-030: a ban with a deadline must expose it, and one without must not have
// a deadline invented for it.
func TestBanDeadlineIsRecordedOnlyWhenGiven(t *testing.T) {
	h := newHarness(t)
	session := h.connected(t)

	session.EmitBan(6 * time.Hour)
	row := h.waitForState(t, domain.InstanceBanned)
	require.NotNil(t, row.BanExpiresAt)
	assert.WithinDuration(t, time.Now().Add(6*time.Hour), *row.BanExpiresAt, time.Minute)

	other := newHarness(t)
	otherSession := other.connected(t)
	otherSession.EmitBan(0)

	row = other.waitForState(t, domain.InstanceBanned)
	assert.Nil(t, row.BanExpiresAt, "no deadline reported means no deadline stored")
}

// FR-031: an explicit command is what brings a terminal instance back — the
// reason is cleared, and only then does reconciliation see it again.
func TestExplicitConnectReenablesAnInvalidatedInstance(t *testing.T) {
	h := newHarness(t)
	session := h.connected(t)

	session.EmitDisconnect(domain.ReasonLoggedOutFromPhone)
	h.waitForState(t, domain.InstanceLoggedOut)

	// The handler stops the lease and releases the session after writing the
	// state; acting before that finishes would race its own cleanup.
	require.Eventually(t, func() bool { return !h.supervisor.Owns(h.instanceID) },
		10*time.Second, 25*time.Millisecond)

	adoptable, err := h.leases.Adoptable(h.ctx, 10)
	require.NoError(t, err)
	require.NotContains(t, adoptable, h.instanceID)

	// This is what the API does on connect: clear the reason, then ask for the
	// session to run again.
	_, err = h.infra.Queries.SetConnectionIntent(h.ctx, store.SetConnectionIntentParams{
		ID:               h.instanceID,
		ConnectionIntent: string(domain.IntentActive),
		ClearReason:      true,
	})
	require.NoError(t, err)
	require.NoError(t, h.leases.SetDesired(h.ctx, h.instanceID, lease.DesiredRunning))

	adoptable, err = h.leases.Adoptable(h.ctx, 10)
	require.NoError(t, err)
	assert.Contains(t, adoptable, h.instanceID,
		"clearing the reason is what lets the fleet pick the instance up again")
}

// StreamReplaced means the same device credentials were opened twice. With the
// lease working it cannot happen, so it is an alarm rather than a statistic —
// and the counter has to move when it does.
func TestStreamReplacedRaisesTheAlarm(t *testing.T) {
	h := newHarness(t)
	session := h.connected(t)

	before := counterValue(t, "zappermeow_sessions_stream_replaced_total")

	session.EmitDisconnect(domain.ReasonSessionReplaced)
	h.waitForState(t, domain.InstanceDisconnected)

	assert.Eventually(t, func() bool {
		return counterValue(t, "zappermeow_sessions_stream_replaced_total") > before
	}, 10*time.Second, 25*time.Millisecond,
		"a replaced stream must be counted: it is a violation of exclusive ownership")
}

// An invalidation on one instance must not touch another companion device of
// the same number.
func TestInvalidationDoesNotAffectSiblingInstances(t *testing.T) {
	h := newHarness(t)
	session := h.connected(t)

	sibling := uuid.New()
	_, err := h.infra.Pool.Exec(h.ctx, `
		INSERT INTO instances (id, tenant_id, name, connection_state, wa_jid, phone_number)
		SELECT $1, tenant_id, 'vendas-02', 'connected', '5511999999999:12@s.whatsapp.net', '5511999999999'
		FROM instances WHERE id = $2`, sibling, h.instanceID)
	require.NoError(t, err)

	session.EmitDisconnect(domain.ReasonLoggedOutFromPhone)
	h.waitForState(t, domain.InstanceLoggedOut)

	var siblingState string
	require.NoError(t, h.infra.Pool.QueryRow(h.ctx,
		`SELECT connection_state FROM instances WHERE id = $1`, sibling).Scan(&siblingState))
	assert.Equal(t, "connected", siblingState,
		"companion devices of one number are independent sessions")
}

// counterValue reads a counter straight from the registry, which is what a
// scrape would see.
func counterValue(t *testing.T, name string) float64 {
	t.Helper()

	families, err := metrics.Registry.Gather()
	require.NoError(t, err)

	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		var total float64
		for _, metric := range family.GetMetric() {
			total += metric.GetCounter().GetValue()
		}
		return total
	}
	return 0
}

// A remote logout destroys the material on both sides. If the row keeps
// pointing at the dead device, the next pairing loads nothing and the instance
// is stranded forever — so the identity has to go with it.
func TestRemoteLogoutClearsTheDevicePointer(t *testing.T) {
	h := newHarness(t)
	session := h.connected(t)

	require.NotNil(t, h.instance(t).WaJid)

	session.EmitDisconnect(domain.ReasonLoggedOutFromPhone)
	h.waitForState(t, domain.InstanceLoggedOut)

	require.Eventually(t, func() bool { return h.instance(t).WaJid == nil },
		10*time.Second, 25*time.Millisecond,
		"the row still points at material the server already destroyed")

	row := h.instance(t)
	assert.Nil(t, row.PairedAt)
	assert.Nil(t, row.ConnectedAt)
	assert.Equal(t, string(domain.InstanceLoggedOut), row.ConnectionState,
		"the state stays logged_out: the tenant must see why it went down")
	require.NotNil(t, row.LastDisconnectReason)
	assert.Equal(t, string(domain.ReasonLoggedOutFromPhone), *row.LastDisconnectReason)
}
