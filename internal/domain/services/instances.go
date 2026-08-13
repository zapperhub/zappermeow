package services

import (
	"context"
	"net/netip"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/store"
)

// instanceConstraints maps the instance uniqueness constraint onto its member.
var instanceConstraints = map[string]domain.FieldError{
	"instances_tenant_id_name_uk": {Location: "body.name", Message: "an instance with this name already exists in this tenant"},
}

// InstanceService implements the tenant-plane use cases over instances.
//
// Every operation takes the tenant from the caller's token and scopes the query
// by it. A resource belonging to another tenant is reported exactly like one
// that never existed, so the API cannot be used to probe for existence (FR-009).
type InstanceService struct {
	pool     *pgxpool.Pool
	queries  *store.Queries
	recorder *EventRecorder
	sessions SessionTerminator
}

// SessionTerminator ends a session before its instance is removed. It is
// optional: a deployment without a worker fleet has no session to end.
type SessionTerminator interface {
	Terminate(ctx context.Context, instanceID domain.ID) error
}

// WithSessions gives the service a way to end sessions on deletion.
func (s *InstanceService) WithSessions(terminator SessionTerminator) *InstanceService {
	s.sessions = terminator
	return s
}

// NewInstanceService builds the instance use cases.
func NewInstanceService(pool *pgxpool.Pool, queries *store.Queries, recorder *EventRecorder) *InstanceService {
	return &InstanceService{pool: pool, queries: queries, recorder: recorder}
}

// CreateInstanceInput registers a new instance for a tenant.
type CreateInstanceInput struct {
	TenantID domain.ID
	Name     string
	ActorID  domain.ID
	SourceIP *netip.Addr
}

// Create registers an instance in the "registered" state. Pairing with WhatsApp
// belongs to a later feature; here the instance is only a record.
func (s *InstanceService) Create(ctx context.Context, in CreateInstanceInput) (domain.Instance, error) {
	name := domain.NormalizeName(in.Name)
	if err := domain.ValidateInstanceName("body.name", name); err != nil {
		return domain.Instance{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Instance{}, domain.ErrInternal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)

	row, err := q.CreateInstance(ctx, store.CreateInstanceParams{
		ID:       domain.NewID(),
		TenantID: in.TenantID,
		Name:     name,
	})
	if err != nil {
		if conflict := conflictField(err, instanceConstraints); conflict != nil {
			return domain.Instance{}, conflict
		}
		return domain.Instance{}, domain.ErrInternal(err)
	}
	instance := instanceFromRow(row)

	if err := s.recorder.Record(ctx, q, domain.SecurityEvent{
		Type:        domain.EventInstanceCreated,
		ActorUserID: &in.ActorID,
		TargetType:  domain.TargetInstance,
		TargetID:    &instance.ID,
		Result:      domain.ResultSuccess,
		SourceIP:    in.SourceIP,
		Metadata:    map[string]any{"instance_name": instance.Name, "tenant_id": in.TenantID.String()},
	}); err != nil {
		return domain.Instance{}, domain.ErrInternal(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.Instance{}, domain.ErrInternal(err)
	}
	return instance, nil
}

// List returns the instances of one tenant, oldest first.
func (s *InstanceService) List(ctx context.Context, tenantID domain.ID) ([]domain.Instance, error) {
	rows, err := s.queries.ListInstancesByTenant(ctx, tenantID)
	if err != nil {
		return nil, domain.ErrInternal(err)
	}
	instances := make([]domain.Instance, 0, len(rows))
	for _, row := range rows {
		instances = append(instances, instanceFromRow(row))
	}
	return instances, nil
}

// Get returns one instance of the caller's tenant.
func (s *InstanceService) Get(ctx context.Context, tenantID, instanceID domain.ID) (domain.Instance, error) {
	row, err := s.queries.GetInstanceByIDAndTenant(ctx, store.GetInstanceByIDAndTenantParams{
		ID:       instanceID,
		TenantID: tenantID,
	})
	if err != nil {
		if isNoRows(err) {
			return domain.Instance{}, domain.ErrNotFound()
		}
		return domain.Instance{}, domain.ErrInternal(err)
	}
	return instanceFromRow(row), nil
}

// RenameInstanceInput renames an instance.
type RenameInstanceInput struct {
	TenantID   domain.ID
	InstanceID domain.ID
	Name       string
	ActorID    domain.ID
	SourceIP   *netip.Addr
}

// Rename changes an instance's friendly name.
func (s *InstanceService) Rename(ctx context.Context, in RenameInstanceInput) (domain.Instance, error) {
	name := domain.NormalizeName(in.Name)
	if err := domain.ValidateInstanceName("body.name", name); err != nil {
		return domain.Instance{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Instance{}, domain.ErrInternal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)

	row, err := q.RenameInstance(ctx, store.RenameInstanceParams{
		ID:       in.InstanceID,
		TenantID: in.TenantID,
		Name:     name,
	})
	if err != nil {
		if isNoRows(err) {
			return domain.Instance{}, domain.ErrNotFound()
		}
		if conflict := conflictField(err, instanceConstraints); conflict != nil {
			return domain.Instance{}, conflict
		}
		return domain.Instance{}, domain.ErrInternal(err)
	}
	instance := instanceFromRow(row)

	if err := s.recorder.Record(ctx, q, domain.SecurityEvent{
		Type:        domain.EventInstanceUpdated,
		ActorUserID: &in.ActorID,
		TargetType:  domain.TargetInstance,
		TargetID:    &instance.ID,
		Result:      domain.ResultSuccess,
		SourceIP:    in.SourceIP,
		Metadata:    map[string]any{"instance_name": instance.Name},
	}); err != nil {
		return domain.Instance{}, domain.ErrInternal(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.Instance{}, domain.ErrInternal(err)
	}
	return instance, nil
}

// DeleteInstanceInput removes an instance.
type DeleteInstanceInput struct {
	TenantID   domain.ID
	InstanceID domain.ID
	ActorID    domain.ID
	SourceIP   *netip.Addr
}

// Delete removes an instance and, through the foreign key cascade, every API
// key issued for it — which is what makes those keys stop working immediately.
// The cascade is audited on the parent event with the number of keys removed;
// there are no per-key events (research R9).
func (s *InstanceService) Delete(ctx context.Context, in DeleteInstanceInput) error {
	// The session goes first: removing the row while a worker still holds the
	// device would leave a companion device linked on the customer's handset
	// and a session nobody owns (FR-007). A failure here is logged by the
	// terminator and does not block the deletion — the tenant asked for the
	// instance to be gone, and the alternative is a record they cannot remove.
	if s.sessions != nil {
		_ = s.sessions.Terminate(ctx, in.InstanceID)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.ErrInternal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)

	// Counted before the delete, inside the same transaction, so the audit
	// record cannot disagree with what actually disappeared.
	keyCount, err := q.CountKeysByInstance(ctx, in.InstanceID)
	if err != nil {
		return domain.ErrInternal(err)
	}

	row, err := q.DeleteInstance(ctx, store.DeleteInstanceParams{
		ID:       in.InstanceID,
		TenantID: in.TenantID,
	})
	if err != nil {
		if isNoRows(err) {
			return domain.ErrNotFound()
		}
		return domain.ErrInternal(err)
	}
	instance := instanceFromRow(row)

	if err := s.recorder.Record(ctx, q, domain.SecurityEvent{
		Type:        domain.EventInstanceDeleted,
		ActorUserID: &in.ActorID,
		TargetType:  domain.TargetInstance,
		TargetID:    &instance.ID,
		Result:      domain.ResultSuccess,
		SourceIP:    in.SourceIP,
		Metadata: map[string]any{
			"instance_name":        instance.Name,
			"api_keys_revoked":     keyCount,
			"cascade_delete_scope": "api_keys",
		},
	}); err != nil {
		return domain.ErrInternal(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.ErrInternal(err)
	}
	return nil
}
