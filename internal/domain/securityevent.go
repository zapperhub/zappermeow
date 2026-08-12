package domain

import "net/netip"

// EventType enumerates the sensitive actions that must stay traceable (FR-021).
type EventType string

const (
	EventLoginSucceeded       EventType = "login_succeeded"
	EventLoginFailed          EventType = "login_failed"
	EventAccountLocked        EventType = "account_locked"
	EventAccountUnlocked      EventType = "account_unlocked"
	EventPasswordChanged      EventType = "password_changed"
	EventPasswordReset        EventType = "password_reset"
	EventTenantCreated        EventType = "tenant_created"
	EventTenantUpdated        EventType = "tenant_updated"
	EventTenantSuspended      EventType = "tenant_suspended"
	EventTenantActivated      EventType = "tenant_activated"
	EventTenantDeleted        EventType = "tenant_deleted"
	EventInstanceCreated      EventType = "instance_created"
	EventInstanceUpdated      EventType = "instance_updated"
	EventInstanceDeleted      EventType = "instance_deleted"
	EventAPIKeyCreated        EventType = "api_key_created"
	EventAPIKeyRevoked        EventType = "api_key_revoked"
	EventBootstrapAdminCreate EventType = "bootstrap_admin_created"
)

// EventResult is the outcome recorded alongside an event.
type EventResult string

const (
	ResultSuccess EventResult = "success"
	ResultFailure EventResult = "failure"
	ResultDenied  EventResult = "denied"
)

// Target types recorded in security events.
const (
	TargetTenant   = "tenant"
	TargetUser     = "user"
	TargetInstance = "instance"
	TargetAPIKey   = "api_key"
)

// SecurityEvent is one append-only audit record. Metadata carries per-type
// details such as a key prefix or a cascade count and must never hold secrets.
type SecurityEvent struct {
	Type EventType
	// ActorUserID is nil when the actor is unknown, as in a failed login for an
	// email that does not exist.
	ActorUserID *ID
	TargetType  string
	TargetID    *ID
	Result      EventResult
	SourceIP    *netip.Addr
	Metadata    map[string]any
}
