package domain

import "time"

// InstanceState is the lifecycle state of an instance.
type InstanceState string

// InstanceRegistered is the only state in this feature: the instance exists as
// a record but is not paired with WhatsApp yet. Pairing and connection states
// arrive in a later feature by widening the column's CHECK constraint, with no
// destructive migration.
const InstanceRegistered InstanceState = "registered"

// Valid reports whether s is a known state.
func (s InstanceState) Valid() bool { return s == InstanceRegistered }

// Instance is the record of a future WhatsApp number. It is the unit of
// isolation: credentials belong to an instance, not to its tenant.
type Instance struct {
	ID        ID
	TenantID  ID
	Name      string
	State     InstanceState
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ValidateInstanceName applies the shared name rules to an instance name.
func ValidateInstanceName(location, name string) error { return ValidateName(location, name) }
