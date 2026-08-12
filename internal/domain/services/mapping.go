package services

import (
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/store"
)

// uniqueViolation is the SQLSTATE raised by a violated UNIQUE constraint.
const uniqueViolation = "23505"

// isNoRows reports whether err means "no row matched".
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// conflictField maps a database constraint onto the request member that caused
// it, so a 409 tells the client exactly which value collided (FR-005).
func conflictField(err error, constraints map[string]domain.FieldError) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != uniqueViolation {
		return nil
	}
	if field, ok := constraints[pgErr.ConstraintName]; ok {
		return domain.ErrConflict(field.Location, field.Message)
	}
	return domain.ErrConflict("body", "conflicts with an existing resource")
}

func tenantFromRow(row store.Tenant) domain.Tenant {
	return domain.Tenant{
		ID:        row.ID,
		Name:      row.Name,
		Status:    domain.TenantStatus(row.Status),
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
}

// derefTime turns a nullable timestamp into its zero value when absent.
func derefTime(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}

func userFromCreateRow(row store.CreateUserRow) domain.User {
	return domain.User{
		ID:                 row.ID,
		Name:               row.Name,
		Email:              row.Email,
		Role:               domain.Role(row.Role),
		TenantID:           row.TenantID,
		MustChangePassword: row.MustChangePassword,
		FailedLoginCount:   int(row.FailedLoginCount),
		LockedUntil:        derefTime(row.LockedUntil),
		PasswordChangedAt:  row.PasswordChangedAt,
		CreatedAt:          row.CreatedAt,
		UpdatedAt:          row.UpdatedAt,
	}
}

func userFromAdminRow(row store.GetTenantAdminByTenantIDRow) domain.User {
	return domain.User{
		ID:                 row.ID,
		Name:               row.Name,
		Email:              row.Email,
		Role:               domain.Role(row.Role),
		TenantID:           row.TenantID,
		MustChangePassword: row.MustChangePassword,
		FailedLoginCount:   int(row.FailedLoginCount),
		LockedUntil:        derefTime(row.LockedUntil),
		PasswordChangedAt:  row.PasswordChangedAt,
		CreatedAt:          row.CreatedAt,
		UpdatedAt:          row.UpdatedAt,
	}
}

func userFromCredentialRow(row store.GetUserCredentialByEmailRow) domain.User {
	return domain.User{
		ID:                 row.ID,
		Name:               row.Name,
		Email:              row.Email,
		Role:               domain.Role(row.Role),
		TenantID:           row.TenantID,
		MustChangePassword: row.MustChangePassword,
		FailedLoginCount:   int(row.FailedLoginCount),
		LockedUntil:        derefTime(row.LockedUntil),
		PasswordChangedAt:  row.PasswordChangedAt,
		CreatedAt:          row.CreatedAt,
		UpdatedAt:          row.UpdatedAt,
	}
}

// tenantStatusOf reads the joined tenant status. A super-admin has no tenant,
// which counts as active for authorisation purposes.
func tenantStatusOf(status *string) domain.TenantStatus {
	if status == nil {
		return domain.TenantActive
	}
	return domain.TenantStatus(*status)
}

func instanceFromRow(row store.Instance) domain.Instance {
	return domain.Instance{
		ID:        row.ID,
		TenantID:  row.TenantID,
		Name:      row.Name,
		State:     domain.InstanceState(row.State),
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
}
