package domain

import "time"

// TenantStatus is the lifecycle state of a customer account.
type TenantStatus string

const (
	// TenantActive is the normal state: logins, tokens and API keys all work.
	TenantActive TenantStatus = "active"
	// TenantSuspended is a reversible block. It refuses logins, rejects tokens
	// already issued and disables every API key of the tenant's instances,
	// without destroying a single credential.
	TenantSuspended TenantStatus = "suspended"
)

// Valid reports whether s is a known status.
func (s TenantStatus) Valid() bool {
	return s == TenantActive || s == TenantSuspended
}

// Tenant is a customer of the platform.
type Tenant struct {
	ID        ID
	Name      string
	Status    TenantStatus
	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsActive reports whether the tenant may currently authenticate and operate.
func (t Tenant) IsActive() bool { return t.Status == TenantActive }

// ValidateTenantName applies the shared name rules to a tenant name.
func ValidateTenantName(location, name string) error { return ValidateName(location, name) }
