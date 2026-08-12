package handlers

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/zapperhub/zappermeow/internal/api/httperr"
	"github.com/zapperhub/zappermeow/internal/api/middleware"
	"github.com/zapperhub/zappermeow/internal/domain"
)

// WhoamiHandler serves the operational verification route.
type WhoamiHandler struct{}

// NewWhoamiHandler builds the whoami handler.
func NewWhoamiHandler() *WhoamiHandler { return &WhoamiHandler{} }

// WhoamiInstanceData describes the instance the key belongs to.
type WhoamiInstanceData struct {
	ID       string `json:"id" format:"uuid"`
	Name     string `json:"name"`
	State    string `json:"state" enum:"registered"`
	TenantID string `json:"tenant_id" format:"uuid"`
}

// WhoamiKeyData identifies the key that authenticated the call, without ever
// repeating the secret.
type WhoamiKeyData struct {
	KeyPrefix string  `json:"key_prefix" example:"zmk_a1b2c3d4"`
	Label     *string `json:"label"`
}

// WhoamiData is the whoami payload.
type WhoamiData struct {
	Instance WhoamiInstanceData `json:"instance"`
	Key      WhoamiKeyData      `json:"key"`
}

// WhoamiInput addresses the instance being verified.
type WhoamiInput struct {
	InstanceID string `path:"instanceId" format:"uuid"`
}

// Register mounts the operational route. It is the smallest possible endpoint
// that exercises the entire credential chain end to end (FR-014), and the
// template every future operational route follows.
func (h *WhoamiHandler) Register(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "whoami",
		Method:      http.MethodGet,
		Path:        "/instances/{instanceId}/whoami",
		Summary:     "Identify the instance behind an API key",
		Description: "Authenticated by API key only. Verifies that the key is active, belongs to the instance " +
			"in the URL and that its tenant is active.",
		Tags:          []string{"operational"},
		Security:      []map[string][]string{{"apiKeyAuth": {}}},
		DefaultStatus: http.StatusOK,
	}, h.whoami)
}

func (h *WhoamiHandler) whoami(ctx context.Context, _ *WhoamiInput) (*httperr.Response[WhoamiData], error) {
	operator, ok := middleware.OperatorFrom(ctx)
	if !ok {
		// Unreachable behind the operational middleware; refuse rather than
		// answer with an empty identity.
		return nil, httperr.From(domain.ErrUnauthenticated("An API key is required"))
	}

	return httperr.OK(WhoamiData{
		Instance: WhoamiInstanceData{
			ID:       operator.InstanceID.String(),
			Name:     operator.InstanceName,
			State:    operator.InstanceState,
			TenantID: operator.TenantID.String(),
		},
		Key: WhoamiKeyData{
			KeyPrefix: operator.KeyPrefix,
			Label:     operator.KeyLabel,
		},
	}), nil
}
