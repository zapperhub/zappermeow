package handlers

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/zapperhub/zappermeow/internal/api/httperr"
	"github.com/zapperhub/zappermeow/internal/api/middleware"
	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/domain/services"
)

// VerificationHandler serves the identity verification codes.
//
// Unlike the rest of the connection plane this route takes only the instance's
// own API key: reading the safety numbers of a conversation is an operational
// action performed by the tenant's software on behalf of an end user, not
// something an administrator does from a console (FR-025).
type VerificationHandler struct {
	connections *services.ConnectionService
}

// NewVerificationHandler builds the handler.
func NewVerificationHandler(connections *services.ConnectionService) *VerificationHandler {
	return &VerificationHandler{connections: connections}
}

// apiKeyOnlySecurity declares the single accepted scheme.
var apiKeyOnlySecurity = []map[string][]string{{"apiKeyAuth": {}}}

// VerificationCodesInput addresses the contact to verify.
type VerificationCodesInput struct {
	InstanceID string `path:"instanceId" format:"uuid"`
	// Contact is a LID or a phone number in international format without a
	// leading plus. A phone number is resolved through the mappings this
	// session already knows.
	Contact string `query:"contact" required:"true" example:"5511999998888"`
}

// VerificationContactData identifies who the codes describe.
type VerificationContactData struct {
	LID         string  `json:"lid" example:"123456789@lid"`
	PhoneNumber *string `json:"phone_number"`
	Username    *string `json:"username"`
}

// VerificationCodesData is the safety-number material for one conversation.
type VerificationCodesData struct {
	Contact VerificationContactData `json:"contact"`
	// NumericCode is the 60-digit safety number shown in the handset under
	// the conversation's encryption details.
	NumericCode string `json:"numeric_code"`
	// The QR payloads are raw fingerprint material in base64 for the tenant to
	// render; the platform does not produce images.
	DisplayQR      []byte `json:"display_qr"`
	VerificationQR []byte `json:"verification_qr"`
}

// Register mounts the verification route.
func (h *VerificationHandler) Register(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-instance-identity-verification-codes",
		Method:      http.MethodGet,
		Path:        "/instances/{instanceId}/identity-verification-codes",
		Summary:     "Read the identity verification codes for a contact",
		Description: "Returns the safety numbers of the conversation between this instance and one contact, so " +
			"the two sides can compare them. Requires a connected instance: the codes are derived from the " +
			"identities WhatsApp reports. Identities learned while answering are kept by the session, which is " +
			"how the Signal protocol works.",
		Tags:          []string{"connection"},
		Security:      apiKeyOnlySecurity,
		DefaultStatus: http.StatusOK,
	}, h.codes)
}

func (h *VerificationHandler) codes(ctx context.Context, in *VerificationCodesInput) (*httperr.Response[VerificationCodesData], error) {
	principal, err := principalFrom(ctx)
	if err != nil {
		return nil, err
	}
	// A tenant token authenticates, but it is not what this route takes. The
	// answer is the same one an unknown instance gets, so refusing here reveals
	// nothing about which instances exist.
	if principal.Kind != middleware.PrincipalAPIKey {
		return nil, httperr.From(domain.ErrNotFound())
	}

	codes, err := h.connections.IdentityVerificationCodes(ctx, principal.InstanceID, in.Contact)
	if err != nil {
		return nil, httperr.From(err)
	}

	data := VerificationCodesData{
		Contact:        VerificationContactData{LID: codes.LID},
		NumericCode:    codes.NumericCode,
		DisplayQR:      codes.DisplayQR,
		VerificationQR: codes.VerificationQR,
	}
	if codes.PhoneNumber != "" {
		data.Contact.PhoneNumber = &codes.PhoneNumber
	}
	if codes.Username != "" {
		data.Contact.Username = &codes.Username
	}
	return httperr.OK(data), nil
}
