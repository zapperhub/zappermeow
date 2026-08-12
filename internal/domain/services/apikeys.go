package services

import (
	"context"
	"net/netip"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/metrics"
	"github.com/zapperhub/zappermeow/internal/store"
)

// APIKeyService implements issuing, listing and revoking operational keys.
type APIKeyService struct {
	pool     *pgxpool.Pool
	queries  *store.Queries
	recorder *EventRecorder
}

// NewAPIKeyService builds the API key use cases.
func NewAPIKeyService(pool *pgxpool.Pool, queries *store.Queries, recorder *EventRecorder) *APIKeyService {
	return &APIKeyService{pool: pool, queries: queries, recorder: recorder}
}

// CreateAPIKeyInput issues a key for one instance of the caller's tenant.
type CreateAPIKeyInput struct {
	TenantID   domain.ID
	InstanceID domain.ID
	Label      string
	ActorID    domain.ID
	SourceIP   *netip.Addr
}

// CreatedAPIKey is the result of issuing a key. Secret is populated exactly
// once, here, and is not stored anywhere.
type CreatedAPIKey struct {
	Key    domain.APIKey
	Secret string
}

// Create issues a new key for an instance. Several keys may be active at once,
// which is what makes rotation possible without downtime.
func (s *APIKeyService) Create(ctx context.Context, in CreateAPIKeyInput) (CreatedAPIKey, error) {
	label := strings.TrimSpace(in.Label)
	if err := domain.ValidateLabel("body.label", label); err != nil {
		return CreatedAPIKey{}, err
	}

	// Ownership is verified first so a key is never minted for an instance the
	// caller cannot see; an instance of another tenant is simply "not found".
	if _, err := s.queries.GetInstanceByIDAndTenant(ctx, store.GetInstanceByIDAndTenantParams{
		ID:       in.InstanceID,
		TenantID: in.TenantID,
	}); err != nil {
		if isNoRows(err) {
			return CreatedAPIKey{}, domain.ErrNotFound()
		}
		return CreatedAPIKey{}, domain.ErrInternal(err)
	}

	generated, err := domain.GenerateAPIKey()
	if err != nil {
		return CreatedAPIKey{}, domain.ErrInternal(err)
	}

	var labelPtr *string
	if label != "" {
		labelPtr = &label
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CreatedAPIKey{}, domain.ErrInternal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)

	row, err := q.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		ID:         domain.NewID(),
		InstanceID: in.InstanceID,
		Label:      labelPtr,
		KeyPrefix:  generated.Prefix,
		SecretHash: generated.Hash,
	})
	if err != nil {
		return CreatedAPIKey{}, domain.ErrInternal(err)
	}
	key := apiKeyFromRow(row)

	// The prefix identifies the key in the trail; the secret never appears.
	if err := s.recorder.Record(ctx, q, domain.SecurityEvent{
		Type:        domain.EventAPIKeyCreated,
		ActorUserID: &in.ActorID,
		TargetType:  domain.TargetAPIKey,
		TargetID:    &key.ID,
		Result:      domain.ResultSuccess,
		SourceIP:    in.SourceIP,
		Metadata:    map[string]any{"key_prefix": key.KeyPrefix, "instance_id": in.InstanceID.String()},
	}); err != nil {
		return CreatedAPIKey{}, domain.ErrInternal(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return CreatedAPIKey{}, domain.ErrInternal(err)
	}

	s.RefreshActiveKeyGauge(ctx)
	return CreatedAPIKey{Key: key, Secret: generated.Secret}, nil
}

// List returns the keys of an instance owned by the caller's tenant.
func (s *APIKeyService) List(ctx context.Context, tenantID, instanceID domain.ID) ([]domain.APIKey, error) {
	if _, err := s.queries.GetInstanceByIDAndTenant(ctx, store.GetInstanceByIDAndTenantParams{
		ID:       instanceID,
		TenantID: tenantID,
	}); err != nil {
		if isNoRows(err) {
			return nil, domain.ErrNotFound()
		}
		return nil, domain.ErrInternal(err)
	}

	rows, err := s.queries.ListKeysByInstance(ctx, instanceID)
	if err != nil {
		return nil, domain.ErrInternal(err)
	}

	keys := make([]domain.APIKey, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, apiKeyFromListRow(row))
	}
	return keys, nil
}

// RevokeAPIKeyInput revokes one key.
type RevokeAPIKeyInput struct {
	TenantID   domain.ID
	InstanceID domain.ID
	KeyID      domain.ID
	ActorID    domain.ID
	SourceIP   *netip.Addr
}

// Revoke disables a key. The effect is immediate: the next operational request
// carrying it is refused, with no grace period (FR-012, SC-004).
func (s *APIKeyService) Revoke(ctx context.Context, in RevokeAPIKeyInput) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.ErrInternal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)

	row, err := q.RevokeAPIKey(ctx, store.RevokeAPIKeyParams{
		ID:       in.KeyID,
		ID_2:     in.InstanceID,
		TenantID: in.TenantID,
	})
	if err != nil {
		// No row matched: unknown key, key of another instance, or an instance
		// of another tenant. All three answer identically.
		if isNoRows(err) {
			return domain.ErrNotFound()
		}
		return domain.ErrInternal(err)
	}
	key := apiKeyFromRevokeRow(row)

	if err := s.recorder.Record(ctx, q, domain.SecurityEvent{
		Type:        domain.EventAPIKeyRevoked,
		ActorUserID: &in.ActorID,
		TargetType:  domain.TargetAPIKey,
		TargetID:    &key.ID,
		Result:      domain.ResultSuccess,
		SourceIP:    in.SourceIP,
		Metadata:    map[string]any{"key_prefix": key.KeyPrefix, "instance_id": in.InstanceID.String()},
	}); err != nil {
		return domain.ErrInternal(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.ErrInternal(err)
	}

	s.RefreshActiveKeyGauge(ctx)
	return nil
}

// RefreshActiveKeyGauge republishes how many keys are usable right now. It runs
// after every issue and revoke, and once at boot so a restart does not leave
// the gauge reading zero.
func (s *APIKeyService) RefreshActiveKeyGauge(ctx context.Context) {
	count, err := s.queries.CountActiveAPIKeys(ctx)
	if err != nil {
		// A metric is never worth failing a request over.
		return
	}
	metrics.APIKeysActive.Set(float64(count))
}

func apiKeyFromRow(row store.CreateAPIKeyRow) domain.APIKey {
	return domain.APIKey{
		ID:         row.ID,
		InstanceID: row.InstanceID,
		Label:      row.Label,
		KeyPrefix:  row.KeyPrefix,
		Status:     domain.APIKeyStatus(row.Status),
		CreatedAt:  row.CreatedAt,
		RevokedAt:  row.RevokedAt,
	}
}

func apiKeyFromListRow(row store.ListKeysByInstanceRow) domain.APIKey {
	return domain.APIKey{
		ID:         row.ID,
		InstanceID: row.InstanceID,
		Label:      row.Label,
		KeyPrefix:  row.KeyPrefix,
		Status:     domain.APIKeyStatus(row.Status),
		CreatedAt:  row.CreatedAt,
		RevokedAt:  row.RevokedAt,
	}
}

func apiKeyFromRevokeRow(row store.RevokeAPIKeyRow) domain.APIKey {
	return domain.APIKey{
		ID:         row.ID,
		InstanceID: row.InstanceID,
		Label:      row.Label,
		KeyPrefix:  row.KeyPrefix,
		Status:     domain.APIKeyStatus(row.Status),
		CreatedAt:  row.CreatedAt,
		RevokedAt:  row.RevokedAt,
	}
}
