package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/zapperhub/zappermeow/internal/api/httperr"
	"github.com/zapperhub/zappermeow/internal/api/middleware"
	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/domain/services"
)

// APIKeyHandler serves issuing, listing and revoking operational keys.
type APIKeyHandler struct {
	keys *services.APIKeyService
}

// NewAPIKeyHandler builds the API key handlers.
func NewAPIKeyHandler(keys *services.APIKeyService) *APIKeyHandler {
	return &APIKeyHandler{keys: keys}
}

// APIKeyData is a key as shown in listings: identifying metadata only, never
// the secret, which cannot be recovered after creation.
type APIKeyData struct {
	ID        string     `json:"id" format:"uuid"`
	Label     *string    `json:"label" doc:"Optional label chosen at creation."`
	KeyPrefix string     `json:"key_prefix" example:"zmk_a1b2c3d4" doc:"First characters of the key, enough to tell two keys apart."`
	Status    string     `json:"status" enum:"active,revoked"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at"`
}

// CreatedAPIKeyData is the creation response — the only place the full secret
// ever appears. It is not stored and cannot be shown again.
type CreatedAPIKeyData struct {
	ID        string    `json:"id" format:"uuid"`
	Label     *string   `json:"label"`
	KeyPrefix string    `json:"key_prefix" example:"zmk_a1b2c3d4"`
	APIKey    string    `json:"api_key" doc:"The full secret. Shown exactly once: store it now, it cannot be retrieved later."`
	CreatedAt time.Time `json:"created_at"`
}

// APIKeyListData is the listing payload.
type APIKeyListData struct {
	Keys []APIKeyData `json:"keys"`
}

func apiKeyToData(key domain.APIKey) APIKeyData {
	return APIKeyData{
		ID:        key.ID.String(),
		Label:     key.Label,
		KeyPrefix: key.KeyPrefix,
		Status:    string(key.Status),
		CreatedAt: key.CreatedAt,
		RevokedAt: key.RevokedAt,
	}
}

// CreateAPIKeyInput issues a key for an instance.
type CreateAPIKeyInput struct {
	InstanceID string `path:"instanceId" format:"uuid"`
	Body       struct {
		Label string `json:"label,omitempty" maxLength:"60" example:"produção" doc:"Optional label to identify this key later."`
	}
}

// ListAPIKeysInput lists the keys of an instance.
type ListAPIKeysInput struct {
	InstanceID string `path:"instanceId" format:"uuid"`
}

// RevokeAPIKeyInput revokes one key of an instance.
type RevokeAPIKeyInput struct {
	InstanceID string `path:"instanceId" format:"uuid"`
	KeyID      string `path:"keyId" format:"uuid"`
}

// Register mounts the key operations onto the tenant-authenticated group.
func (h *APIKeyHandler) Register(api huma.API) {
	security := []map[string][]string{{"bearerAuth": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "create-api-key",
		Method:      http.MethodPost,
		Path:        "/instances/{instanceId}/keys",
		Summary:     "Issue an API key for an instance",
		Description: "Returns the full secret exactly once. Several keys can be active at the same time, " +
			"which is what allows rotation without downtime.",
		Tags:          []string{"api-keys"},
		Security:      security,
		DefaultStatus: http.StatusCreated,
	}, h.create)

	huma.Register(api, huma.Operation{
		OperationID:   "list-api-keys",
		Method:        http.MethodGet,
		Path:          "/instances/{instanceId}/keys",
		Summary:       "List the keys of an instance",
		Description:   "Shows label, prefix, status and dates. The secret is never included.",
		Tags:          []string{"api-keys"},
		Security:      security,
		DefaultStatus: http.StatusOK,
	}, h.list)

	huma.Register(api, huma.Operation{
		OperationID:   "revoke-api-key",
		Method:        http.MethodDelete,
		Path:          "/instances/{instanceId}/keys/{keyId}",
		Summary:       "Revoke an API key",
		Description:   "Takes effect on the next operational request; there is no grace period.",
		Tags:          []string{"api-keys"},
		Security:      security,
		DefaultStatus: http.StatusNoContent,
	}, h.revoke)
}

func (h *APIKeyHandler) create(ctx context.Context, in *CreateAPIKeyInput) (*httperr.Response[CreatedAPIKeyData], error) {
	admin, _ := middleware.AdminFrom(ctx)
	instanceID, err := domain.ParseID("path.instanceId", in.InstanceID)
	if err != nil {
		return nil, httperr.From(err)
	}

	created, err := h.keys.Create(ctx, services.CreateAPIKeyInput{
		TenantID:   admin.TenantID(),
		InstanceID: instanceID,
		Label:      in.Body.Label,
		ActorID:    admin.User.ID,
		SourceIP:   middleware.ClientIPFrom(ctx),
	})
	if err != nil {
		return nil, httperr.From(err)
	}

	return httperr.Created(CreatedAPIKeyData{
		ID:        created.Key.ID.String(),
		Label:     created.Key.Label,
		KeyPrefix: created.Key.KeyPrefix,
		APIKey:    created.Secret,
		CreatedAt: created.Key.CreatedAt,
	}), nil
}

func (h *APIKeyHandler) list(ctx context.Context, in *ListAPIKeysInput) (*httperr.Response[APIKeyListData], error) {
	admin, _ := middleware.AdminFrom(ctx)
	instanceID, err := domain.ParseID("path.instanceId", in.InstanceID)
	if err != nil {
		return nil, httperr.From(err)
	}

	keys, err := h.keys.List(ctx, admin.TenantID(), instanceID)
	if err != nil {
		return nil, httperr.From(err)
	}

	data := make([]APIKeyData, 0, len(keys))
	for _, key := range keys {
		data = append(data, apiKeyToData(key))
	}
	return httperr.OK(APIKeyListData{Keys: data}), nil
}

func (h *APIKeyHandler) revoke(ctx context.Context, in *RevokeAPIKeyInput) (*DeleteResponse, error) {
	admin, _ := middleware.AdminFrom(ctx)
	instanceID, err := domain.ParseID("path.instanceId", in.InstanceID)
	if err != nil {
		return nil, httperr.From(err)
	}
	keyID, err := domain.ParseID("path.keyId", in.KeyID)
	if err != nil {
		return nil, httperr.From(err)
	}

	if err := h.keys.Revoke(ctx, services.RevokeAPIKeyInput{
		TenantID:   admin.TenantID(),
		InstanceID: instanceID,
		KeyID:      keyID,
		ActorID:    admin.User.ID,
		SourceIP:   middleware.ClientIPFrom(ctx),
	}); err != nil {
		return nil, httperr.From(err)
	}
	return nil, nil
}
