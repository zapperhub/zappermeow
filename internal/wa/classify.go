package wa

import (
	"time"

	"github.com/polymorfa/hypermeow/types/events"

	"github.com/zapperhub/zappermeow/internal/domain"
)

// Classification is the verdict on a HyperMeow event: which state the instance
// moves to, why, and whether the system should keep trying to reconnect.
type Classification struct {
	State  domain.InstanceState
	Reason domain.DisconnectReason
	// Permanent means automatic reconnection must stop until an explicit
	// command from the user clears the reason.
	Permanent bool
	// BanExpiresAt is set only for temporary bans that carry a deadline.
	BanExpiresAt *time.Time
}

// ClassifyDisconnect maps a HyperMeow event onto the platform's connection
// state machine. The second result is false for events that say nothing about
// connectivity, which the caller should ignore.
//
// The library's events.PermanentDisconnect interface already marks every event
// after which reconnecting is pointless, and this table agrees with it on all
// six. The table exists because the interface answers only "should I stop?" —
// it does not say whether the instance is logged out, banned or merely offline,
// nor which reason the tenant should see. The permanence check below is kept as
// a cross-check and as the default for events a future library version may add:
// a new permanent event we have never heard of must stop reconnection rather
// than be silently retried forever.
func ClassifyDisconnect(evt any) (Classification, bool) {
	switch e := evt.(type) {

	// --- transient: keep reconnecting ---

	case *events.Disconnected:
		return Classification{
			State:  domain.InstanceConnecting,
			Reason: domain.ReasonNetwork,
		}, true

	case *events.KeepAliveTimeout:
		// The TCP connection may well recover on its own; this is a warning
		// sign, not a disconnection.
		return Classification{
			State:  domain.InstanceConnecting,
			Reason: domain.ReasonKeepaliveTimeout,
		}, true

	// --- invalidation: stop trying ---

	case *events.LoggedOut:
		// Does not implement PermanentDisconnect, yet it is the most permanent
		// outcome there is: the device was unlinked and the material is dead.
		return Classification{
			State:     domain.InstanceLoggedOut,
			Reason:    domain.ReasonLoggedOutFromPhone,
			Permanent: true,
		}, true

	case *events.TemporaryBan:
		c := Classification{
			State:     domain.InstanceBanned,
			Reason:    domain.ReasonTemporaryBan,
			Permanent: true,
		}
		if e.Expire > 0 {
			expires := time.Now().Add(e.Expire)
			c.BanExpiresAt = &expires
		}
		return c, true

	case *events.StreamReplaced:
		// With the lease working this cannot happen: it means the same device
		// credentials were opened somewhere else. The caller must treat it as
		// an alarm about exclusive ownership, not as routine telemetry.
		return Classification{
			State:     domain.InstanceDisconnected,
			Reason:    domain.ReasonSessionReplaced,
			Permanent: true,
		}, true

	case *events.ClientOutdated:
		// Retrying cannot fix a client the server refuses; the operator has to
		// upgrade the deployment.
		return Classification{
			State:     domain.InstanceDisconnected,
			Reason:    domain.ReasonClientOutdated,
			Permanent: true,
		}, true

	case *events.ConnectFailure:
		return Classification{
			State:     domain.InstanceDisconnected,
			Reason:    domain.ReasonConnectFailure,
			Permanent: true,
		}, true

	case *events.CATRefreshError:
		return Classification{
			State:     domain.InstanceDisconnected,
			Reason:    domain.ReasonCATRefreshFailed,
			Permanent: true,
		}, true

	default:
		// An event the table does not know, but which the library declares
		// permanent, still has to stop reconnection. Failing open here would
		// mean hammering a session WhatsApp has already given up on.
		if _, permanent := evt.(events.PermanentDisconnect); permanent {
			return Classification{
				State:     domain.InstanceDisconnected,
				Reason:    domain.ReasonConnectFailure,
				Permanent: true,
			}, true
		}
		return Classification{}, false
	}
}

// IsAlarm reports whether a classification signals a violation of exclusive
// session ownership (Principle III) rather than an ordinary failure. Callers
// log these at error level and increment a dedicated counter.
func (c Classification) IsAlarm() bool {
	return c.Reason == domain.ReasonSessionReplaced
}
