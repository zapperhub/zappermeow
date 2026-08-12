package services

import (
	"context"
	"net/netip"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/store"
)

// PasswordService implements changing one's own password and the super-admin
// reset that hands out a temporary one. Neither path needs SMTP: the platform
// deliberately has no mail dependency.
type PasswordService struct {
	pool     *pgxpool.Pool
	queries  *store.Queries
	recorder *EventRecorder
}

// NewPasswordService builds the password use cases.
func NewPasswordService(pool *pgxpool.Pool, queries *store.Queries, recorder *EventRecorder) *PasswordService {
	return &PasswordService{pool: pool, queries: queries, recorder: recorder}
}

// ChangePasswordInput changes the caller's own password.
type ChangePasswordInput struct {
	UserID          domain.ID
	CurrentPassword string
	NewPassword     string
	SourceIP        *netip.Addr
}

// Change replaces the caller's password after proving they know the current
// one. It clears any pending temporary password and stamps password_changed_at,
// which invalidates the previous password and every token issued before now.
func (s *PasswordService) Change(ctx context.Context, in ChangePasswordInput) error {
	if err := domain.ValidatePassword("body.new_password", in.NewPassword); err != nil {
		return err
	}

	currentHash, err := s.queries.GetUserPasswordHash(ctx, in.UserID)
	if err != nil {
		if isNoRows(err) {
			return domain.ErrUnauthenticated("The token is invalid or has expired")
		}
		return domain.ErrInternal(err)
	}

	matches, err := domain.VerifyPassword(currentHash, in.CurrentPassword)
	if err != nil {
		return domain.ErrInternal(err)
	}
	if !matches {
		return domain.ErrInvalidCurrentPassword()
	}

	newHash, err := domain.HashPassword(in.NewPassword)
	if err != nil {
		return domain.ErrInternal(err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.ErrInternal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)

	if err := q.UpdatePassword(ctx, store.UpdatePasswordParams{
		ID:                 in.UserID,
		PasswordHash:       newHash,
		MustChangePassword: false,
	}); err != nil {
		return domain.ErrInternal(err)
	}

	if err := s.recorder.Record(ctx, q, domain.SecurityEvent{
		Type:        domain.EventPasswordChanged,
		ActorUserID: &in.UserID,
		TargetType:  domain.TargetUser,
		TargetID:    &in.UserID,
		Result:      domain.ResultSuccess,
		SourceIP:    in.SourceIP,
	}); err != nil {
		return domain.ErrInternal(err)
	}

	return tx.Commit(ctx)
}

// ResetTenantAdminInput resets the password of a tenant's administrator.
type ResetTenantAdminInput struct {
	TenantID domain.ID
	ActorID  domain.ID
	SourceIP *netip.Addr
}

// ResetResult carries the one-shot temporary password.
type ResetResult struct {
	TemporaryPassword string
	AdminUserID       domain.ID
}

// ResetTenantAdmin generates a temporary password for a tenant's admin. It is
// returned to the super-admin exactly once and stored only as a hash; the admin
// must replace it before doing anything else (FR-016).
func (s *PasswordService) ResetTenantAdmin(ctx context.Context, in ResetTenantAdminInput) (ResetResult, error) {
	if _, err := s.queries.GetTenantByID(ctx, in.TenantID); err != nil {
		if isNoRows(err) {
			return ResetResult{}, domain.ErrNotFound()
		}
		return ResetResult{}, domain.ErrInternal(err)
	}

	adminRow, err := s.queries.GetTenantAdminByTenantID(ctx, &in.TenantID)
	if err != nil {
		if isNoRows(err) {
			return ResetResult{}, domain.ErrNotFound()
		}
		return ResetResult{}, domain.ErrInternal(err)
	}

	temporary, err := domain.GenerateTemporaryPassword()
	if err != nil {
		return ResetResult{}, domain.ErrInternal(err)
	}
	hash, err := domain.HashPassword(temporary)
	if err != nil {
		return ResetResult{}, domain.ErrInternal(err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ResetResult{}, domain.ErrInternal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)

	// must_change_password forces the next session into the password change
	// route; the same statement clears any lockout so the admin can actually
	// use the temporary password they were just given.
	if err := q.UpdatePassword(ctx, store.UpdatePasswordParams{
		ID:                 adminRow.ID,
		PasswordHash:       hash,
		MustChangePassword: true,
	}); err != nil {
		return ResetResult{}, domain.ErrInternal(err)
	}

	if err := s.recorder.Record(ctx, q, domain.SecurityEvent{
		Type:        domain.EventPasswordReset,
		ActorUserID: &in.ActorID,
		TargetType:  domain.TargetUser,
		TargetID:    &adminRow.ID,
		Result:      domain.ResultSuccess,
		SourceIP:    in.SourceIP,
		Metadata:    map[string]any{"tenant_id": in.TenantID.String()},
	}); err != nil {
		return ResetResult{}, domain.ErrInternal(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return ResetResult{}, domain.ErrInternal(err)
	}

	return ResetResult{TemporaryPassword: temporary, AdminUserID: adminRow.ID}, nil
}
