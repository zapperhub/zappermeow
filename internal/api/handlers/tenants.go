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

// TenantHandler serves the platform plane: management of tenants.
type TenantHandler struct {
	tenants *services.TenantService
}

// NewTenantHandler builds the tenant handlers.
func NewTenantHandler(tenants *services.TenantService) *TenantHandler {
	return &TenantHandler{tenants: tenants}
}

// AdminData is the tenant administrator as exposed by the API. It never carries
// credential material.
type AdminData struct {
	ID    string `json:"id" format:"uuid"`
	Name  string `json:"name"`
	Email string `json:"email" format:"email"`
}

// TenantData is a tenant as exposed by the API.
type TenantData struct {
	ID        string     `json:"id" format:"uuid"`
	Name      string     `json:"name"`
	Status    string     `json:"status" enum:"active,suspended"`
	Admin     *AdminData `json:"admin,omitempty" doc:"The tenant administrator, included on single-tenant reads."`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// TenantListData is the listing payload; an empty platform yields an empty list.
type TenantListData struct {
	Tenants []TenantData `json:"tenants"`
}

func tenantToData(tenant domain.Tenant) TenantData {
	return TenantData{
		ID:        tenant.ID.String(),
		Name:      tenant.Name,
		Status:    string(tenant.Status),
		CreatedAt: tenant.CreatedAt,
		UpdatedAt: tenant.UpdatedAt,
	}
}

func tenantWithAdminToData(result services.TenantWithAdmin) TenantData {
	data := tenantToData(result.Tenant)
	if result.Admin.ID != (domain.ID{}) {
		data.Admin = &AdminData{
			ID:    result.Admin.ID.String(),
			Name:  result.Admin.Name,
			Email: result.Admin.Email,
		}
	}
	return data
}

// CreateTenantInput is the tenant creation request.
type CreateTenantInput struct {
	Body struct {
		Name  string `json:"name" minLength:"1" maxLength:"120" example:"ACME Corp" doc:"Unique tenant name."`
		Admin struct {
			Name     string `json:"name" minLength:"1" maxLength:"120" example:"Alice"`
			Email    string `json:"email" minLength:"3" maxLength:"320" example:"alice@acme.com" doc:"Globally unique login address."`
			Password string `json:"password" minLength:"8" doc:"Initial password, at least 8 characters."`
		} `json:"admin" doc:"The administrator this tenant is created with."`
	}
}

// TenantIDInput addresses a single tenant.
type TenantIDInput struct {
	TenantID string `path:"tenantId" format:"uuid" doc:"Tenant identifier."`
}

// RenameTenantInput is the tenant update request.
type RenameTenantInput struct {
	TenantID string `path:"tenantId" format:"uuid"`
	Body     struct {
		Name string `json:"name" minLength:"1" maxLength:"120" example:"ACME International"`
	}
}

// Register mounts the tenant operations onto the platform-authenticated group.
func (h *TenantHandler) Register(api huma.API) {
	security := []map[string][]string{{"bearerAuth": {}}}

	huma.Register(api, huma.Operation{
		OperationID:   "create-tenant",
		Method:        http.MethodPost,
		Path:          "/admin/tenants",
		Summary:       "Create a tenant with its administrator",
		Description:   "Creates the tenant and its admin atomically. A duplicate tenant name or admin email is rejected with 409 and the offending member in `errors[].location`.",
		Tags:          []string{"tenants"},
		Security:      security,
		DefaultStatus: http.StatusCreated,
	}, h.create)

	huma.Register(api, huma.Operation{
		OperationID:   "list-tenants",
		Method:        http.MethodGet,
		Path:          "/admin/tenants",
		Summary:       "List tenants",
		Tags:          []string{"tenants"},
		Security:      security,
		DefaultStatus: http.StatusOK,
	}, h.list)

	huma.Register(api, huma.Operation{
		OperationID:   "get-tenant",
		Method:        http.MethodGet,
		Path:          "/admin/tenants/{tenantId}",
		Summary:       "Get a tenant",
		Tags:          []string{"tenants"},
		Security:      security,
		DefaultStatus: http.StatusOK,
	}, h.get)

	huma.Register(api, huma.Operation{
		OperationID:   "update-tenant",
		Method:        http.MethodPatch,
		Path:          "/admin/tenants/{tenantId}",
		Summary:       "Rename a tenant",
		Tags:          []string{"tenants"},
		Security:      security,
		DefaultStatus: http.StatusOK,
	}, h.rename)
}

func (h *TenantHandler) create(ctx context.Context, in *CreateTenantInput) (*httperr.Response[TenantData], error) {
	admin, _ := middleware.AdminFrom(ctx)

	result, err := h.tenants.Create(ctx, services.CreateTenantInput{
		Name:          in.Body.Name,
		AdminName:     in.Body.Admin.Name,
		AdminEmail:    in.Body.Admin.Email,
		AdminPassword: in.Body.Admin.Password,
		ActorID:       admin.User.ID,
		SourceIP:      middleware.ClientIPFrom(ctx),
	})
	if err != nil {
		return nil, httperr.From(err)
	}
	return httperr.Created(tenantWithAdminToData(result)), nil
}

func (h *TenantHandler) list(ctx context.Context, _ *struct{}) (*httperr.Response[TenantListData], error) {
	tenants, err := h.tenants.List(ctx)
	if err != nil {
		return nil, httperr.From(err)
	}

	data := make([]TenantData, 0, len(tenants))
	for _, tenant := range tenants {
		data = append(data, tenantToData(tenant))
	}
	return httperr.OK(TenantListData{Tenants: data}), nil
}

func (h *TenantHandler) get(ctx context.Context, in *TenantIDInput) (*httperr.Response[TenantData], error) {
	tenantID, err := domain.ParseID("path.tenantId", in.TenantID)
	if err != nil {
		return nil, httperr.From(err)
	}

	result, err := h.tenants.Get(ctx, tenantID)
	if err != nil {
		return nil, httperr.From(err)
	}
	return httperr.OK(tenantWithAdminToData(result)), nil
}

func (h *TenantHandler) rename(ctx context.Context, in *RenameTenantInput) (*httperr.Response[TenantData], error) {
	tenantID, err := domain.ParseID("path.tenantId", in.TenantID)
	if err != nil {
		return nil, httperr.From(err)
	}
	admin, _ := middleware.AdminFrom(ctx)

	tenant, err := h.tenants.Rename(ctx, services.RenameTenantInput{
		TenantID: tenantID,
		Name:     in.Body.Name,
		ActorID:  admin.User.ID,
		SourceIP: middleware.ClientIPFrom(ctx),
	})
	if err != nil {
		return nil, httperr.From(err)
	}
	return httperr.OK(tenantToData(tenant)), nil
}

// DeleteTenantInput permanently removes a tenant.
type DeleteTenantInput struct {
	TenantID string `path:"tenantId" format:"uuid"`
	Body     struct {
		ConfirmName string `json:"confirm_name" minLength:"1" example:"ACME Corp" doc:"The tenant's exact name. Typing it back is what confirms an irreversible deletion."`
	}
}

// RegisterLifecycle mounts suspension, reactivation and deletion.
func (h *TenantHandler) RegisterLifecycle(api huma.API) {
	security := []map[string][]string{{"bearerAuth": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "suspend-tenant",
		Method:      http.MethodPost,
		Path:        "/admin/tenants/{tenantId}/suspend",
		Summary:     "Suspend a tenant",
		Description: "Reversible block. Logins are refused, tokens already issued stop being accepted and every " +
			"API key of the tenant's instances stops working, all from the next request onwards. Idempotent.",
		Tags:          []string{"tenants"},
		Security:      security,
		DefaultStatus: http.StatusOK,
	}, h.suspend)

	huma.Register(api, huma.Operation{
		OperationID:   "activate-tenant",
		Method:        http.MethodPost,
		Path:          "/admin/tenants/{tenantId}/activate",
		Summary:       "Reactivate a tenant",
		Description:   "Restores logins, tokens and API keys without recreating any credential. Idempotent.",
		Tags:          []string{"tenants"},
		Security:      security,
		DefaultStatus: http.StatusOK,
	}, h.activate)

	huma.Register(api, huma.Operation{
		OperationID: "delete-tenant",
		Method:      http.MethodDelete,
		Path:        "/admin/tenants/{tenantId}",
		Summary:     "Permanently delete a tenant",
		Description: "Irreversible. Removes the tenant with its users, instances and keys. Requires the tenant's " +
			"exact name in `confirm_name`.",
		Tags:          []string{"tenants"},
		Security:      security,
		DefaultStatus: http.StatusNoContent,
	}, h.delete)
}

func (h *TenantHandler) suspend(ctx context.Context, in *TenantIDInput) (*httperr.Response[TenantData], error) {
	return h.setStatus(ctx, in.TenantID, domain.TenantSuspended)
}

func (h *TenantHandler) activate(ctx context.Context, in *TenantIDInput) (*httperr.Response[TenantData], error) {
	return h.setStatus(ctx, in.TenantID, domain.TenantActive)
}

func (h *TenantHandler) setStatus(ctx context.Context, rawID string, status domain.TenantStatus) (*httperr.Response[TenantData], error) {
	admin, _ := middleware.AdminFrom(ctx)
	tenantID, err := domain.ParseID("path.tenantId", rawID)
	if err != nil {
		return nil, httperr.From(err)
	}

	tenant, err := h.tenants.SetStatus(ctx, services.SetStatusInput{
		TenantID: tenantID,
		Status:   status,
		ActorID:  admin.User.ID,
		SourceIP: middleware.ClientIPFrom(ctx),
	})
	if err != nil {
		return nil, httperr.From(err)
	}
	return httperr.OK(tenantToData(tenant)), nil
}

func (h *TenantHandler) delete(ctx context.Context, in *DeleteTenantInput) (*DeleteResponse, error) {
	admin, _ := middleware.AdminFrom(ctx)
	tenantID, err := domain.ParseID("path.tenantId", in.TenantID)
	if err != nil {
		return nil, httperr.From(err)
	}

	if err := h.tenants.Delete(ctx, services.DeleteTenantInput{
		TenantID:    tenantID,
		ConfirmName: in.Body.ConfirmName,
		ActorID:     admin.User.ID,
		SourceIP:    middleware.ClientIPFrom(ctx),
	}); err != nil {
		return nil, httperr.From(err)
	}
	return nil, nil
}
