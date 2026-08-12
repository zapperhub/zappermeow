package services

import (
	"context"
	"net/netip"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/store"
)

// tenantConstraints maps the uniqueness constraints touched by tenant creation
// onto the request members that carry the offending value.
var tenantConstraints = map[string]domain.FieldError{
	"tenants_name_key": {Location: "body.name", Message: "a tenant with this name already exists"},
	"users_email_key":  {Location: "body.admin.email", Message: "a user with this email already exists"},
}

// TenantService implements the platform-plane use cases over tenants.
type TenantService struct {
	pool     *pgxpool.Pool
	queries  *store.Queries
	recorder *EventRecorder
}

// NewTenantService builds the tenant use cases.
func NewTenantService(pool *pgxpool.Pool, queries *store.Queries, recorder *EventRecorder) *TenantService {
	return &TenantService{pool: pool, queries: queries, recorder: recorder}
}

// CreateTenantInput is a tenant plus the admin it is born with.
type CreateTenantInput struct {
	Name          string
	AdminName     string
	AdminEmail    string
	AdminPassword string
	ActorID       domain.ID
	SourceIP      *netip.Addr
}

// TenantWithAdmin pairs a tenant with its administrator.
type TenantWithAdmin struct {
	Tenant domain.Tenant
	Admin  domain.User
}

// Create provisions a tenant and its admin in a single transaction: either both
// exist afterwards or neither does, so a duplicate email can never leave a
// half-built tenant behind (FR-022).
func (s *TenantService) Create(ctx context.Context, in CreateTenantInput) (TenantWithAdmin, error) {
	name := domain.NormalizeName(in.Name)
	adminName := domain.NormalizeName(in.AdminName)
	adminEmail := domain.NormalizeEmail(in.AdminEmail)

	if err := domain.CollectErrors(
		domain.ValidateTenantName("body.name", name),
		domain.ValidateName("body.admin.name", adminName),
		domain.ValidateEmail("body.admin.email", adminEmail),
		domain.ValidatePassword("body.admin.password", in.AdminPassword),
	); err != nil {
		return TenantWithAdmin{}, err
	}

	hash, err := domain.HashPassword(in.AdminPassword)
	if err != nil {
		return TenantWithAdmin{}, domain.ErrInternal(err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TenantWithAdmin{}, domain.ErrInternal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)

	tenantRow, err := q.CreateTenant(ctx, store.CreateTenantParams{ID: domain.NewID(), Name: name})
	if err != nil {
		if conflict := conflictField(err, tenantConstraints); conflict != nil {
			return TenantWithAdmin{}, conflict
		}
		return TenantWithAdmin{}, domain.ErrInternal(err)
	}

	tenant := tenantFromRow(tenantRow)
	adminRow, err := q.CreateUser(ctx, store.CreateUserParams{
		ID:                 domain.NewID(),
		Name:               adminName,
		Email:              adminEmail,
		PasswordHash:       hash,
		Role:               string(domain.RoleTenantAdmin),
		TenantID:           &tenant.ID,
		MustChangePassword: false,
	})
	if err != nil {
		if conflict := conflictField(err, tenantConstraints); conflict != nil {
			return TenantWithAdmin{}, conflict
		}
		return TenantWithAdmin{}, domain.ErrInternal(err)
	}
	admin := userFromCreateRow(adminRow)

	if err := s.recorder.Record(ctx, q, domain.SecurityEvent{
		Type:        domain.EventTenantCreated,
		ActorUserID: &in.ActorID,
		TargetType:  domain.TargetTenant,
		TargetID:    &tenant.ID,
		Result:      domain.ResultSuccess,
		SourceIP:    in.SourceIP,
		Metadata:    map[string]any{"tenant_name": tenant.Name, "admin_user_id": admin.ID.String()},
	}); err != nil {
		return TenantWithAdmin{}, domain.ErrInternal(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return TenantWithAdmin{}, domain.ErrInternal(err)
	}
	return TenantWithAdmin{Tenant: tenant, Admin: admin}, nil
}

// List returns every tenant, oldest first. An empty platform yields an empty
// slice, never an error.
func (s *TenantService) List(ctx context.Context) ([]domain.Tenant, error) {
	rows, err := s.queries.ListTenants(ctx)
	if err != nil {
		return nil, domain.ErrInternal(err)
	}
	tenants := make([]domain.Tenant, 0, len(rows))
	for _, row := range rows {
		tenants = append(tenants, tenantFromRow(row))
	}
	return tenants, nil
}

// Get returns a tenant with its admin.
func (s *TenantService) Get(ctx context.Context, id domain.ID) (TenantWithAdmin, error) {
	tenantRow, err := s.queries.GetTenantByID(ctx, id)
	if err != nil {
		if isNoRows(err) {
			return TenantWithAdmin{}, domain.ErrNotFound()
		}
		return TenantWithAdmin{}, domain.ErrInternal(err)
	}

	result := TenantWithAdmin{Tenant: tenantFromRow(tenantRow)}

	adminRow, err := s.queries.GetTenantAdminByTenantID(ctx, &result.Tenant.ID)
	switch {
	case err == nil:
		result.Admin = userFromAdminRow(adminRow)
	case isNoRows(err):
		// A tenant without an admin is not reachable through the API, but the
		// read path must not fail if one ever exists.
	default:
		return TenantWithAdmin{}, domain.ErrInternal(err)
	}

	return result, nil
}

// RenameTenantInput carries a rename request.
type RenameTenantInput struct {
	TenantID domain.ID
	Name     string
	ActorID  domain.ID
	SourceIP *netip.Addr
}

// Rename changes a tenant's display name.
func (s *TenantService) Rename(ctx context.Context, in RenameTenantInput) (domain.Tenant, error) {
	name := domain.NormalizeName(in.Name)
	if err := domain.ValidateTenantName("body.name", name); err != nil {
		return domain.Tenant{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Tenant{}, domain.ErrInternal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)

	row, err := q.UpdateTenantName(ctx, store.UpdateTenantNameParams{ID: in.TenantID, Name: name})
	if err != nil {
		if isNoRows(err) {
			return domain.Tenant{}, domain.ErrNotFound()
		}
		if conflict := conflictField(err, tenantConstraints); conflict != nil {
			return domain.Tenant{}, conflict
		}
		return domain.Tenant{}, domain.ErrInternal(err)
	}
	tenant := tenantFromRow(row)

	if err := s.recorder.Record(ctx, q, domain.SecurityEvent{
		Type:        domain.EventTenantUpdated,
		ActorUserID: &in.ActorID,
		TargetType:  domain.TargetTenant,
		TargetID:    &tenant.ID,
		Result:      domain.ResultSuccess,
		SourceIP:    in.SourceIP,
		Metadata:    map[string]any{"tenant_name": tenant.Name},
	}); err != nil {
		return domain.Tenant{}, domain.ErrInternal(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.Tenant{}, domain.ErrInternal(err)
	}
	return tenant, nil
}

// ensure pgx is referenced for its error sentinel used by isNoRows.
var _ = pgx.ErrNoRows

// SetStatusInput suspends or reactivates a tenant.
type SetStatusInput struct {
	TenantID domain.ID
	Status   domain.TenantStatus
	ActorID  domain.ID
	SourceIP *netip.Addr
}

// SetStatus suspends or reactivates a tenant.
//
// Suspension is a reversible block that needs no cascading writes: the tenant's
// status is read on every authenticated request, so logins stop, tokens already
// issued stop being accepted and every API key of every instance stops working
// at once. Reactivation restores all of it without recreating a credential
// (FR-006).
func (s *TenantService) SetStatus(ctx context.Context, in SetStatusInput) (domain.Tenant, error) {
	if !in.Status.Valid() {
		return domain.Tenant{}, domain.ErrValidation("path.status", "must be active or suspended")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Tenant{}, domain.ErrInternal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)

	row, err := q.SetTenantStatus(ctx, store.SetTenantStatusParams{ID: in.TenantID, Status: string(in.Status)})
	if err != nil {
		if isNoRows(err) {
			return domain.Tenant{}, domain.ErrNotFound()
		}
		return domain.Tenant{}, domain.ErrInternal(err)
	}
	tenant := tenantFromRow(row)

	eventType := domain.EventTenantActivated
	if tenant.Status == domain.TenantSuspended {
		eventType = domain.EventTenantSuspended
	}

	if err := s.recorder.Record(ctx, q, domain.SecurityEvent{
		Type:        eventType,
		ActorUserID: &in.ActorID,
		TargetType:  domain.TargetTenant,
		TargetID:    &tenant.ID,
		Result:      domain.ResultSuccess,
		SourceIP:    in.SourceIP,
		Metadata:    map[string]any{"tenant_name": tenant.Name, "status": string(tenant.Status)},
	}); err != nil {
		return domain.Tenant{}, domain.ErrInternal(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.Tenant{}, domain.ErrInternal(err)
	}
	return tenant, nil
}

// DeleteTenantInput permanently removes a tenant.
type DeleteTenantInput struct {
	TenantID domain.ID
	// ConfirmName must match the tenant's name exactly. Requiring the operator
	// to type the name back is what separates an intentional destruction from a
	// mis-clicked identifier.
	ConfirmName string
	ActorID     domain.ID
	SourceIP    *netip.Addr
}

// Delete permanently removes a tenant with its users, instances and keys.
// The removal is irreversible and cascades through foreign keys in a single
// transaction, so nothing of the tenant can survive it (FR-007).
func (s *TenantService) Delete(ctx context.Context, in DeleteTenantInput) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.ErrInternal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)

	existing, err := q.GetTenantByID(ctx, in.TenantID)
	if err != nil {
		if isNoRows(err) {
			return domain.ErrNotFound()
		}
		return domain.ErrInternal(err)
	}

	// Compared exactly, including case: a destructive confirmation should not
	// be satisfied by an approximation of the name.
	if in.ConfirmName != existing.Name {
		return domain.ErrValidation("body.confirm_name", "must match the tenant name exactly")
	}

	counts, err := q.CountTenantCascade(ctx, &in.TenantID)
	if err != nil {
		return domain.ErrInternal(err)
	}

	row, err := q.DeleteTenantByID(ctx, in.TenantID)
	if err != nil {
		if isNoRows(err) {
			return domain.ErrNotFound()
		}
		return domain.ErrInternal(err)
	}
	tenant := tenantFromRow(row)

	if err := s.recorder.Record(ctx, q, domain.SecurityEvent{
		Type:        domain.EventTenantDeleted,
		ActorUserID: &in.ActorID,
		TargetType:  domain.TargetTenant,
		TargetID:    &tenant.ID,
		Result:      domain.ResultSuccess,
		SourceIP:    in.SourceIP,
		Metadata: map[string]any{
			"tenant_name":          tenant.Name,
			"users_deleted":        counts.UserCount,
			"instances_deleted":    counts.InstanceCount,
			"api_keys_revoked":     counts.ApiKeyCount,
			"cascade_irreversible": true,
		},
	}); err != nil {
		return domain.ErrInternal(err)
	}

	return tx.Commit(ctx)
}
