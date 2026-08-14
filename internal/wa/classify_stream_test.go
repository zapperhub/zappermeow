package wa_test

import (
	"testing"

	"github.com/polymorfa/hypermeow/types/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/wa"
)

// Only codes the library does not recognise reach StreamError: 515, the 401
// conflict, replaced, 503 and the CAT refresh failures are all handled inside
// the library and surface as their own events, which the 002 table already
// classifies (research R9).
func TestClassifyStreamError(t *testing.T) {
	t.Parallel()

	classification, ok := wa.ClassifyDisconnect(&events.StreamError{Code: "999"})

	require.True(t, ok, "an unknown stream error must be classified, not ignored")
	assert.Equal(t, domain.ReasonStreamError, classification.Reason)
	assert.Equal(t, "999", classification.StreamErrorCode)

	// Not permanent, and that follows the library rather than being our call:
	// its handler neither expects the disconnect nor stops the automatic
	// reconnection, and StreamError does not implement PermanentDisconnect.
	// Parking a healthy session on a transient server hiccup would take a
	// working number offline until a human noticed.
	assert.False(t, classification.Permanent)
	assert.Equal(t, domain.InstanceConnecting, classification.State)
	assert.False(t, classification.ManualReconnect)
}

// A stream error is not a logout: nothing about it invalidates the session
// material, so the reason must stay outside the permanent set that blocks
// automatic adoption.
func TestStreamErrorIsNotPermanent(t *testing.T) {
	t.Parallel()

	assert.False(t, domain.ReasonStreamError.Permanent())
	assert.True(t, domain.ReasonStreamError.Valid())
	assert.NotContains(t, domain.PermanentReasonList(), string(domain.ReasonStreamError))
}

// The empty code is what a malformed node produces. It must still classify:
// dropping the event because a field is missing is how a disconnection goes
// unnoticed.
func TestClassifyStreamErrorWithoutACode(t *testing.T) {
	t.Parallel()

	classification, ok := wa.ClassifyDisconnect(&events.StreamError{})

	require.True(t, ok)
	assert.Equal(t, domain.ReasonStreamError, classification.Reason)
	assert.Empty(t, classification.StreamErrorCode)
}

// ManualLoginReconnect is an instruction, not a failure report: the platform
// must reconnect rather than record a drop and wait.
func TestClassifyManualLoginReconnect(t *testing.T) {
	t.Parallel()

	classification, ok := wa.ClassifyDisconnect(&events.ManualLoginReconnect{})

	require.True(t, ok, "the event must be classified even though the current flags never raise it")
	assert.True(t, classification.ManualReconnect)
	assert.False(t, classification.Permanent)
	assert.Equal(t, domain.InstanceConnecting, classification.State)
}

// The two new cases must not disturb the 002 table. This is the cross-check
// that a widened switch did not swallow an event that used to be classified.
func TestExistingClassificationsAreUnchanged(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		event     any
		reason    domain.DisconnectReason
		permanent bool
	}{
		{name: "disconnected", event: &events.Disconnected{}, reason: domain.ReasonNetwork},
		{name: "keepalive timeout", event: &events.KeepAliveTimeout{}, reason: domain.ReasonKeepaliveTimeout},
		{name: "logged out", event: &events.LoggedOut{}, reason: domain.ReasonLoggedOutFromPhone, permanent: true},
		{name: "stream replaced", event: &events.StreamReplaced{}, reason: domain.ReasonSessionReplaced, permanent: true},
		{name: "client outdated", event: &events.ClientOutdated{}, reason: domain.ReasonClientOutdated, permanent: true},
		{name: "connect failure", event: &events.ConnectFailure{}, reason: domain.ReasonConnectFailure, permanent: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			classification, ok := wa.ClassifyDisconnect(tc.event)
			require.True(t, ok)
			assert.Equal(t, tc.reason, classification.Reason)
			assert.Equal(t, tc.permanent, classification.Permanent)
			assert.Empty(t, classification.StreamErrorCode, "only stream errors carry a code")
			assert.False(t, classification.ManualReconnect)
		})
	}
}

// An event that says nothing about connectivity is still ignored: widening the
// switch must not turn every message into a disconnection.
func TestUnrelatedEventsAreStillIgnored(t *testing.T) {
	t.Parallel()

	_, ok := wa.ClassifyDisconnect(&events.PairSuccess{})
	assert.False(t, ok)
}
