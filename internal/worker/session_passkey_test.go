package worker_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/events"
	"github.com/zapperhub/zappermeow/internal/wa"
)

const testChallenge = `{"challenge":"Y2hhbGxlbmdl","rpId":"whatsapp.com","userVerification":"required"}`

// The whole passkey step, in the order WhatsApp drives it: the challenge lands
// mid-attempt, the tenant answers, a handoff code comes back, the tenant
// confirms, and the pairing completes.
func TestPasskeyStepCompletesAPairing(t *testing.T) {
	h := newHarness(t)
	session := h.pairing(t)

	session.EmitPasskeyChallenge(json.RawMessage(testChallenge))
	h.waitForTrail(t, string(domain.ConnEventPasskeyChallenge))

	// The attempt stays alive: the challenge is a step inside pairing, not a
	// failure of it.
	assert.Equal(t, string(domain.InstancePairing), h.instance(t).ConnectionState)

	_, err := h.supervisor.SubmitPasskeyResponse(h.ctx, h.instanceID, []byte(`{"id":"cred"}`))
	require.NoError(t, err)

	session.EmitPasskeyCode("1ABC-2DEF")
	h.waitForTrail(t, string(domain.ConnEventPasskeyResponded))

	_, err = h.supervisor.ConfirmPasskey(h.ctx, h.instanceID)
	require.NoError(t, err)
	h.waitForTrail(t, string(domain.ConnEventPasskeyConfirmed))

	session.EmitPairSuccess(domain.DeviceIdentity{
		JID: "5511999999999:11@s.whatsapp.net", PhoneNumber: "5511999999999",
	})
	h.waitForState(t, domain.InstanceConnected)
}

// A client that opens the channel during the passkey step must see the step,
// not the QR code that stopped working when it began (research R7, R10).
func TestSnapshotMovesToThePasskeyPhase(t *testing.T) {
	h := newHarness(t)
	session := h.pairing(t)

	session.EmitPairingCode("qr-code-1", true)
	require.Eventually(t, func() bool {
		snapshot, found, err := h.publisher.Pairing(h.ctx, h.instanceID)
		return err == nil && found && snapshot.CurrentPhase() == events.PhaseQR
	}, 20*time.Second, 25*time.Millisecond, "the QR phase never landed")

	session.EmitPasskeyChallenge(json.RawMessage(testChallenge))
	require.Eventually(t, func() bool {
		snapshot, found, err := h.publisher.Pairing(h.ctx, h.instanceID)
		return err == nil && found && snapshot.CurrentPhase() == events.PhasePasskeyChallenge
	}, 20*time.Second, 25*time.Millisecond, "the snapshot never moved to the challenge phase")

	snapshot, _, err := h.publisher.Pairing(h.ctx, h.instanceID)
	require.NoError(t, err)
	assert.JSONEq(t, testChallenge, string(snapshot.Challenge))
	assert.Empty(t, snapshot.Code, "the dead QR code must not be served alongside the challenge")

	_, err = h.supervisor.SubmitPasskeyResponse(h.ctx, h.instanceID, []byte(`{"id":"cred"}`))
	require.NoError(t, err)

	session.EmitPasskeyCode("1ABC-2DEF")
	require.Eventually(t, func() bool {
		snapshot, found, err := h.publisher.Pairing(h.ctx, h.instanceID)
		return err == nil && found && snapshot.CurrentPhase() == events.PhasePasskeyCode &&
			snapshot.Code == "1ABC-2DEF"
	}, 20*time.Second, 25*time.Millisecond, "the snapshot never moved to the code phase")
}

// With a valid handoff proof the library confirms on its own and publishes no
// code. The platform must not sit waiting for a confirmation that will never be
// asked for.
func TestPasskeyAutomaticConfirmationNeedsNoTenantAction(t *testing.T) {
	h := newHarness(t)
	session := h.pairing(t)

	session.EmitPasskeyChallenge(json.RawMessage(testChallenge))
	h.waitForTrail(t, string(domain.ConnEventPasskeyChallenge))

	_, err := h.supervisor.SubmitPasskeyResponse(h.ctx, h.instanceID, []byte(`{"id":"cred"}`))
	require.NoError(t, err)

	session.EmitPasskeyAutoConfirmed()
	session.EmitPairSuccess(domain.DeviceIdentity{
		JID: "5511999999999:11@s.whatsapp.net", PhoneNumber: "5511999999999",
	})

	h.waitForState(t, domain.InstanceConnected)
	assert.NotContains(t, h.trail(t), string(domain.ConnEventPasskeyConfirmed),
		"an automatic confirmation is not the tenant's action to record")
}

// Commands that arrive out of order are refused with the reason, not swallowed
// and not allowed to corrupt the attempt. The library is not reentrant here, so
// the check has to happen before it is reached (research R7).
func TestPasskeyCommandsOutOfOrderAreRefused(t *testing.T) {
	h := newHarness(t)
	session := h.pairing(t)

	// Nothing pending yet.
	_, err := h.supervisor.SubmitPasskeyResponse(h.ctx, h.instanceID, []byte(`{"id":"cred"}`))
	require.ErrorIs(t, err, wa.ErrNoPasskeyChallenge)
	_, err = h.supervisor.ConfirmPasskey(h.ctx, h.instanceID)
	require.ErrorIs(t, err, wa.ErrNoPasskeyCode)

	session.EmitPasskeyChallenge(json.RawMessage(testChallenge))
	h.waitForTrail(t, string(domain.ConnEventPasskeyChallenge))

	// Confirming before the code exists.
	_, err = h.supervisor.ConfirmPasskey(h.ctx, h.instanceID)
	require.ErrorIs(t, err, wa.ErrNoPasskeyCode)

	_, err = h.supervisor.SubmitPasskeyResponse(h.ctx, h.instanceID, []byte(`{"id":"cred"}`))
	require.NoError(t, err)
	// Answering the same challenge twice.
	_, err = h.supervisor.SubmitPasskeyResponse(h.ctx, h.instanceID, []byte(`{"id":"cred"}`))
	require.ErrorIs(t, err, wa.ErrNoPasskeyChallenge)

	session.EmitPasskeyCode("1ABC-2DEF")
	h.waitForTrail(t, string(domain.ConnEventPasskeyResponded))

	_, err = h.supervisor.ConfirmPasskey(h.ctx, h.instanceID)
	require.NoError(t, err)
	// Confirming twice: the library clears its linking cache on success, so a
	// second call would fail with an opaque message deep inside it.
	_, err = h.supervisor.ConfirmPasskey(h.ctx, h.instanceID)
	require.ErrorIs(t, err, wa.ErrNoPasskeyCode)
}

// A failing passkey step is a pairing failure, told apart from the others so
// the tenant knows which thing to retry.
func TestPasskeyFailureEndsTheAttempt(t *testing.T) {
	h := newHarness(t)
	session := h.pairing(t)

	session.EmitPasskeyChallenge(json.RawMessage(testChallenge))
	h.waitForTrail(t, string(domain.ConnEventPasskeyChallenge))

	session.EmitPasskeyFailure()

	h.waitForTrail(t, string(domain.ConnEventPairingFailed))
	detail := h.trailDetail(t, string(domain.ConnEventPairingFailed))
	assert.Equal(t, string(wa.FailurePasskeyError), detail["reason"])
}

// The two channels — the pairing stream and the client's own event handler —
// are not ordered against each other. Both orders must reach the same place, or
// a race in production hides behind a stable order in the suite (constitution
// v2.5.0-b).
func TestPasskeyChallengeAndConnectedInEitherOrder(t *testing.T) {
	t.Run("challenge first", func(t *testing.T) {
		h := newHarness(t)
		session := h.pairing(t)

		session.EmitPasskeyChallenge(json.RawMessage(testChallenge))
		h.waitForTrail(t, string(domain.ConnEventPasskeyChallenge))
		session.EmitPairSuccess(domain.DeviceIdentity{
			JID: "5511999999999:11@s.whatsapp.net", PhoneNumber: "5511999999999",
		})

		h.waitForState(t, domain.InstanceConnected)
	})

	t.Run("pair success first", func(t *testing.T) {
		h := newHarness(t)
		session := h.pairing(t)

		session.EmitPairSuccess(domain.DeviceIdentity{
			JID: "5511999999999:11@s.whatsapp.net", PhoneNumber: "5511999999999",
		})
		h.waitForState(t, domain.InstanceConnected)

		// A challenge arriving after the attempt already ended is dropped with
		// the pairing context, and that is the right outcome: reopening a
		// finished attempt would park a connected instance back in pairing and
		// wait for an assertion nobody is going to send.
		session.EmitPasskeyChallenge(json.RawMessage(testChallenge))

		assert.Never(t, func() bool {
			return h.instance(t).ConnectionState != string(domain.InstanceConnected)
		}, 2*time.Second, 100*time.Millisecond,
			"a late challenge must not move a connected instance back to pairing")
		assert.NotContains(t, h.trail(t), string(domain.ConnEventPasskeyChallenge))
	})
}

// pairing brings the harness instance to an open QR attempt, which is where the
// passkey step happens.
func (h *harness) pairing(t *testing.T) *wa.FakeSession {
	t.Helper()

	session := h.adopt(t)
	_, err := h.supervisor.Connect(h.ctx, h.instanceID)
	require.NoError(t, err)
	h.waitForState(t, domain.InstancePairing)
	return session
}
