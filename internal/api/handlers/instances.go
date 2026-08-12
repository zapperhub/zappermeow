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

// InstanceHandler serves the tenant plane: management of instances.
type InstanceHandler struct {
	instances *services.InstanceService
}

// NewInstanceHandler builds the instance handlers.
func NewInstanceHandler(instances *services.InstanceService) *InstanceHandler {
	return &InstanceHandler{instances: instances}
}

// InstanceData is an instance as exposed by the API.
type InstanceData struct {
	ID        string    `json:"id" format:"uuid"`
	Name      string    `json:"name"`
	State     string    `json:"state" enum:"registered" doc:"Lifecycle state. Pairing states arrive in a later feature."`
	TenantID  string    `json:"tenant_id" format:"uuid"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// InstanceListData is the listing payload.
type InstanceListData struct {
	Instances []InstanceData `json:"instances"`
}

func instanceToData(instance domain.Instance) InstanceData {
	return InstanceData{
		ID:        instance.ID.String(),
		Name:      instance.Name,
		State:     string(instance.State),
		TenantID:  instance.TenantID.String(),
		CreatedAt: instance.CreatedAt,
		UpdatedAt: instance.UpdatedAt,
	}
}

// CreateInstanceInput registers an instance.
type CreateInstanceInput struct {
	Body struct {
		Name string `json:"name" minLength:"1" maxLength:"120" example:"vendas-01" doc:"Friendly name, unique within the tenant."`
	}
}

// InstanceIDInput addresses a single instance.
type InstanceIDInput struct {
	InstanceID string `path:"instanceId" format:"uuid" doc:"Instance identifier."`
}

// RenameInstanceInput renames an instance.
type RenameInstanceInput struct {
	InstanceID string `path:"instanceId" format:"uuid"`
	Body       struct {
		Name string `json:"name" minLength:"1" maxLength:"120" example:"vendas-sp"`
	}
}

// DeleteResponse is the empty body of a 204.
type DeleteResponse struct{}

// Register mounts the instance operations onto the tenant-authenticated group.
func (h *InstanceHandler) Register(api huma.API) {
	security := []map[string][]string{{"bearerAuth": {}}}

	huma.Register(api, huma.Operation{
		OperationID:   "create-instance",
		Method:        http.MethodPost,
		Path:          "/instances",
		Summary:       "Register an instance",
		Description:   "Creates an instance record for the caller's tenant. It is not paired with WhatsApp yet.",
		Tags:          []string{"instances"},
		Security:      security,
		DefaultStatus: http.StatusCreated,
	}, h.create)

	huma.Register(api, huma.Operation{
		OperationID:   "list-instances",
		Method:        http.MethodGet,
		Path:          "/instances",
		Summary:       "List the instances of the caller's tenant",
		Tags:          []string{"instances"},
		Security:      security,
		DefaultStatus: http.StatusOK,
	}, h.list)

	huma.Register(api, huma.Operation{
		OperationID:   "get-instance",
		Method:        http.MethodGet,
		Path:          "/instances/{instanceId}",
		Summary:       "Get an instance",
		Description:   "An instance of another tenant answers exactly like one that does not exist.",
		Tags:          []string{"instances"},
		Security:      security,
		DefaultStatus: http.StatusOK,
	}, h.get)

	huma.Register(api, huma.Operation{
		OperationID:   "update-instance",
		Method:        http.MethodPatch,
		Path:          "/instances/{instanceId}",
		Summary:       "Rename an instance",
		Tags:          []string{"instances"},
		Security:      security,
		DefaultStatus: http.StatusOK,
	}, h.rename)

	huma.Register(api, huma.Operation{
		OperationID:   "delete-instance",
		Method:        http.MethodDelete,
		Path:          "/instances/{instanceId}",
		Summary:       "Delete an instance",
		Description:   "Removes the instance and every API key issued for it, which stop working immediately.",
		Tags:          []string{"instances"},
		Security:      security,
		DefaultStatus: http.StatusNoContent,
	}, h.delete)
}

func (h *InstanceHandler) create(ctx context.Context, in *CreateInstanceInput) (*httperr.Response[InstanceData], error) {
	admin, _ := middleware.AdminFrom(ctx)

	instance, err := h.instances.Create(ctx, services.CreateInstanceInput{
		TenantID: admin.TenantID(),
		Name:     in.Body.Name,
		ActorID:  admin.User.ID,
		SourceIP: middleware.ClientIPFrom(ctx),
	})
	if err != nil {
		return nil, httperr.From(err)
	}
	return httperr.Created(instanceToData(instance)), nil
}

func (h *InstanceHandler) list(ctx context.Context, _ *struct{}) (*httperr.Response[InstanceListData], error) {
	admin, _ := middleware.AdminFrom(ctx)

	instances, err := h.instances.List(ctx, admin.TenantID())
	if err != nil {
		return nil, httperr.From(err)
	}

	data := make([]InstanceData, 0, len(instances))
	for _, instance := range instances {
		data = append(data, instanceToData(instance))
	}
	return httperr.OK(InstanceListData{Instances: data}), nil
}

func (h *InstanceHandler) get(ctx context.Context, in *InstanceIDInput) (*httperr.Response[InstanceData], error) {
	admin, _ := middleware.AdminFrom(ctx)
	instanceID, err := domain.ParseID("path.instanceId", in.InstanceID)
	if err != nil {
		return nil, httperr.From(err)
	}

	instance, err := h.instances.Get(ctx, admin.TenantID(), instanceID)
	if err != nil {
		return nil, httperr.From(err)
	}
	return httperr.OK(instanceToData(instance)), nil
}

func (h *InstanceHandler) rename(ctx context.Context, in *RenameInstanceInput) (*httperr.Response[InstanceData], error) {
	admin, _ := middleware.AdminFrom(ctx)
	instanceID, err := domain.ParseID("path.instanceId", in.InstanceID)
	if err != nil {
		return nil, httperr.From(err)
	}

	instance, err := h.instances.Rename(ctx, services.RenameInstanceInput{
		TenantID:   admin.TenantID(),
		InstanceID: instanceID,
		Name:       in.Body.Name,
		ActorID:    admin.User.ID,
		SourceIP:   middleware.ClientIPFrom(ctx),
	})
	if err != nil {
		return nil, httperr.From(err)
	}
	return httperr.OK(instanceToData(instance)), nil
}

func (h *InstanceHandler) delete(ctx context.Context, in *InstanceIDInput) (*DeleteResponse, error) {
	admin, _ := middleware.AdminFrom(ctx)
	instanceID, err := domain.ParseID("path.instanceId", in.InstanceID)
	if err != nil {
		return nil, httperr.From(err)
	}

	if err := h.instances.Delete(ctx, services.DeleteInstanceInput{
		TenantID:   admin.TenantID(),
		InstanceID: instanceID,
		ActorID:    admin.User.ID,
		SourceIP:   middleware.ClientIPFrom(ctx),
	}); err != nil {
		return nil, httperr.From(err)
	}
	return nil, nil
}
