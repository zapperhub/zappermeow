package domain

import "time"

// Role decides which plane a user administers.
type Role string

const (
	// RoleSuperAdmin manages tenants and belongs to no tenant.
	RoleSuperAdmin Role = "super_admin"
	// RoleTenantAdmin manages the instances and keys of exactly one tenant.
	RoleTenantAdmin Role = "tenant_admin"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return r == RoleSuperAdmin || r == RoleTenantAdmin }

// Audience returns the token audience a role authenticates into.
func (r Role) Audience() Audience {
	if r == RoleSuperAdmin {
		return AudiencePlatform
	}
	return AudienceTenant
}

// User is a person who authenticates with email and password.
type User struct {
	ID    ID
	Name  string
	Email string
	Role  Role
	// TenantID is nil exactly when Role is RoleSuperAdmin.
	TenantID *ID
	// MustChangePassword is set after a reset and cleared on the next change.
	// It is authoritative: middlewares read it from the database rather than
	// trusting the claim baked into a token.
	MustChangePassword bool
	FailedLoginCount   int
	// LockedUntil is nil when the account was never locked. A lockout in the
	// past has simply expired — there is no unlock job.
	LockedUntil time.Time
	// PasswordChangedAt invalidates tokens issued before the last password
	// change or reset (SC-004).
	PasswordChangedAt time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// IsLocked reports whether the account is inside an active lockout window.
func (u User) IsLocked(now time.Time) bool {
	return !u.LockedUntil.IsZero() && u.LockedUntil.After(now)
}

// TokenPredatesPasswordChange reports whether a token issued at issuedAt was
// superseded by a password change or reset. Tokens carry second precision, so
// the comparison truncates to avoid rejecting a token minted in the same second
// as the change that produced it.
func (u User) TokenPredatesPasswordChange(issuedAt time.Time) bool {
	return issuedAt.Before(u.PasswordChangedAt.Truncate(time.Second))
}
