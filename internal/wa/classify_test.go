package wa

import (
	"testing"
	"time"

	"github.com/polymorfa/hypermeow/types/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zapperhub/zappermeow/internal/domain"
)

func TestClassifyDisconnect(t *testing.T) {
	tests := []struct {
		name          string
		event         any
		wantState     domain.InstanceState
		wantReason    domain.DisconnectReason
		wantPermanent bool
	}{
		{
			name:       "websocket closed by the server",
			event:      &events.Disconnected{},
			wantState:  domain.InstanceConnecting,
			wantReason: domain.ReasonNetwork,
		},
		{
			name:       "keepalive stopped answering",
			event:      &events.KeepAliveTimeout{ErrorCount: 3},
			wantState:  domain.InstanceConnecting,
			wantReason: domain.ReasonKeepaliveTimeout,
		},
		{
			name:          "unlinked from the handset",
			event:         &events.LoggedOut{OnConnect: false},
			wantState:     domain.InstanceLoggedOut,
			wantReason:    domain.ReasonLoggedOutFromPhone,
			wantPermanent: true,
		},
		{
			name:          "logged out on connect",
			event:         &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut},
			wantState:     domain.InstanceLoggedOut,
			wantReason:    domain.ReasonLoggedOutFromPhone,
			wantPermanent: true,
		},
		{
			name:          "temporary ban",
			event:         &events.TemporaryBan{Code: events.TempBanSentToTooManyPeople, Expire: time.Hour},
			wantState:     domain.InstanceBanned,
			wantReason:    domain.ReasonTemporaryBan,
			wantPermanent: true,
		},
		{
			name:          "session opened elsewhere",
			event:         &events.StreamReplaced{},
			wantState:     domain.InstanceDisconnected,
			wantReason:    domain.ReasonSessionReplaced,
			wantPermanent: true,
		},
		{
			name:          "client version rejected",
			event:         &events.ClientOutdated{},
			wantState:     domain.InstanceDisconnected,
			wantReason:    domain.ReasonClientOutdated,
			wantPermanent: true,
		},
		{
			name:          "unknown connect failure",
			event:         &events.ConnectFailure{Reason: events.ConnectFailureReason(999)},
			wantState:     domain.InstanceDisconnected,
			wantReason:    domain.ReasonConnectFailure,
			wantPermanent: true,
		},
		{
			name:          "CAT refresh failed",
			event:         &events.CATRefreshError{},
			wantState:     domain.InstanceDisconnected,
			wantReason:    domain.ReasonCATRefreshFailed,
			wantPermanent: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ClassifyDisconnect(tc.event)
			require.True(t, ok, "event must be classified")

			assert.Equal(t, tc.wantState, got.State)
			assert.Equal(t, tc.wantReason, got.Reason)
			assert.Equal(t, tc.wantPermanent, got.Permanent)
			assert.Equal(t, tc.wantPermanent, got.Reason.Permanent(),
				"the classification and the reason vocabulary must agree on permanence")
		})
	}
}

// Our table and the library's own PermanentDisconnect interface must never
// disagree: an event the library considers fatal that we keep retrying would
// hammer a dead session, and the reverse would strand a recoverable one.
func TestPermanenceAgreesWithTheLibrary(t *testing.T) {
	permanent := []any{
		&events.LoggedOut{}, &events.StreamReplaced{}, &events.ClientOutdated{},
		&events.CATRefreshError{}, &events.TemporaryBan{}, &events.ConnectFailure{},
	}
	for _, evt := range permanent {
		_, implements := evt.(events.PermanentDisconnect)
		require.True(t, implements, "%T is expected to be a library-permanent event", evt)

		got, ok := ClassifyDisconnect(evt)
		require.True(t, ok, "%T must be classified", evt)
		assert.True(t, got.Permanent, "%T must stop reconnection", evt)
	}

	transient := []any{&events.Disconnected{}, &events.KeepAliveTimeout{}}
	for _, evt := range transient {
		_, implements := evt.(events.PermanentDisconnect)
		require.False(t, implements, "%T is expected to be recoverable", evt)

		got, ok := ClassifyDisconnect(evt)
		require.True(t, ok)
		assert.False(t, got.Permanent, "%T must keep reconnecting", evt)
	}
}

// A permanent event the table has never seen — a future library version, say —
// must still stop reconnection instead of being retried forever.
func TestUnknownPermanentEventStopsReconnection(t *testing.T) {
	got, ok := ClassifyDisconnect(&unknownPermanentEvent{})
	require.True(t, ok, "an unknown permanent event must still be classified")
	assert.True(t, got.Permanent)
	assert.Equal(t, domain.InstanceDisconnected, got.State)
}

type unknownPermanentEvent struct{}

func (*unknownPermanentEvent) PermanentDisconnectDescription() string { return "hypothetical" }

func TestClassifyIgnoresUnrelatedEvents(t *testing.T) {
	// Connected and PairSuccess are handled by the pairing flow, not by the
	// disconnect classifier; anything else is simply not our business.
	for _, evt := range []any{
		&events.Connected{},
		&events.PairSuccess{},
		&events.KeepAliveRestored{},
		&events.Message{},
		nil,
	} {
		_, ok := ClassifyDisconnect(evt)
		assert.False(t, ok, "%T must not be classified as a disconnect", evt)
	}
}

func TestTemporaryBanDeadline(t *testing.T) {
	got, ok := ClassifyDisconnect(&events.TemporaryBan{Code: events.TempBanBlockedByUsers, Expire: 24 * time.Hour})
	require.True(t, ok)
	require.NotNil(t, got.BanExpiresAt, "a ban with a deadline must expose it (FR-030)")
	assert.WithinDuration(t, time.Now().Add(24*time.Hour), *got.BanExpiresAt, time.Minute)

	// A ban without a deadline must not invent one.
	got, ok = ClassifyDisconnect(&events.TemporaryBan{Code: events.TempBanBlockedByUsers})
	require.True(t, ok)
	assert.Nil(t, got.BanExpiresAt)
}

func TestIsAlarm(t *testing.T) {
	replaced, _ := ClassifyDisconnect(&events.StreamReplaced{})
	assert.True(t, replaced.IsAlarm(), "a replaced stream means two owners of one session")

	network, _ := ClassifyDisconnect(&events.Disconnected{})
	assert.False(t, network.IsAlarm())

	loggedOut, _ := ClassifyDisconnect(&events.LoggedOut{})
	assert.False(t, loggedOut.IsAlarm(), "a logout is expected behaviour, not an ownership failure")
}
