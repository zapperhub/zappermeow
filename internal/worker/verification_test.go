package worker_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zapperhub/zappermeow/internal/wa"
)

const (
	contactLID   = "123456789@lid"
	contactPhone = "5511999998888"
)

func cannedCodes() *wa.VerificationCodes {
	return &wa.VerificationCodes{
		PhoneNumber:    contactPhone,
		Username:       "fulano",
		NumericCode:    "123456789012345678901234567890123456789012345678901234567890",
		DisplayQR:      []byte("display"),
		VerificationQR: []byte("verification"),
	}
}

func TestIdentityVerificationCodesByLID(t *testing.T) {
	h := newHarness(t)
	session := h.connected(t)
	session.VerificationCodesResult = cannedCodes()

	codes, err := h.supervisor.IdentityVerificationCodes(h.ctx, h.instanceID, contactLID)
	require.NoError(t, err)

	assert.Equal(t, contactLID, codes.LID)
	assert.Len(t, codes.NumericCode, 60, "the safety number is 60 digits")
	assert.NotEmpty(t, codes.DisplayQR)
	assert.NotEmpty(t, codes.VerificationQR)
}

// A phone number is resolved through the mappings the session already knows.
// Both spellings of the same contact must produce the same answer, or the two
// sides of a conversation could compare different numbers.
func TestIdentityVerificationCodesResolveAPhoneNumber(t *testing.T) {
	h := newHarness(t)
	session := h.connected(t)
	session.VerificationCodesResult = cannedCodes()
	session.LIDMappings = map[string]string{contactPhone: contactLID}

	byPhone, err := h.supervisor.IdentityVerificationCodes(h.ctx, h.instanceID, contactPhone)
	require.NoError(t, err)
	byLID, err := h.supervisor.IdentityVerificationCodes(h.ctx, h.instanceID, contactLID)
	require.NoError(t, err)

	assert.Equal(t, contactLID, byPhone.LID, "the resolved identity is reported back")
	assert.Equal(t, byLID.NumericCode, byPhone.NumericCode)
}

// Guessing an identity would produce a verification code for the wrong person,
// which is worse than refusing to answer (research R8).
func TestIdentityVerificationCodesRefuseAnUnknownNumber(t *testing.T) {
	h := newHarness(t)
	session := h.connected(t)
	session.VerificationCodesResult = cannedCodes()

	_, err := h.supervisor.IdentityVerificationCodes(h.ctx, h.instanceID, "5511000000000")
	require.ErrorIs(t, err, wa.ErrIdentityNotResolvable)
}

// The codes describe a conversation, so verifying the instance against itself
// is a question with no meaning.
func TestIdentityVerificationCodesRefuseTheInstanceItself(t *testing.T) {
	h := newHarness(t)
	session := h.connected(t)
	session.VerificationCodesResult = cannedCodes()
	session.SelfLID = contactLID

	_, err := h.supervisor.IdentityVerificationCodes(h.ctx, h.instanceID, contactLID)
	require.ErrorIs(t, err, wa.ErrCannotVerifySelf)
}

// The codes are derived from what WhatsApp reports about the contact's devices,
// so they cannot be produced without a live session.
func TestIdentityVerificationCodesRequireAConnectedSession(t *testing.T) {
	h := newHarness(t)
	session := h.adopt(t)
	session.VerificationCodesResult = cannedCodes()

	_, err := h.supervisor.IdentityVerificationCodes(h.ctx, h.instanceID, contactLID)
	require.ErrorIs(t, err, wa.ErrNotConnected)
}

func TestIdentityVerificationCodesRefuseAContactWithoutDevices(t *testing.T) {
	h := newHarness(t)
	h.connected(t)

	// No canned result: the fake stands in for a contact WhatsApp knows nothing
	// about.
	_, err := h.supervisor.IdentityVerificationCodes(h.ctx, h.instanceID, contactLID)
	require.ErrorIs(t, err, wa.ErrContactUnavailable)
}
