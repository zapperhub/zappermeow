package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/zapperhub/zappermeow/internal/api/ws"
	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/events"
	"github.com/zapperhub/zappermeow/internal/store"
)

// wsAuthenticator resolves a WebSocket credential.
//
// The upgrade never reaches huma, so the middleware chain does not apply here;
// this repeats the same authorisation the connection routes get, on purpose.
// Both credentials are accepted, and both must resolve to the instance in the
// URL — a key of another instance is as refused as a token of another tenant.
type wsAuthenticator struct {
	queries *store.Queries
	issuer  *domain.TokenIssuer
}

func (a *wsAuthenticator) Authorize(ctx context.Context, credential string, instanceID domain.ID) (ws.Principal, error) {
	if strings.HasPrefix(credential, domain.APIKeyPrefix) {
		return a.authorizeKey(ctx, credential, instanceID)
	}
	return a.authorizeToken(ctx, credential, instanceID)
}

func (a *wsAuthenticator) authorizeKey(ctx context.Context, secret string, instanceID domain.ID) (ws.Principal, error) {
	row, err := a.queries.GetKeyForAuth(ctx, domain.HashAPIKey(secret))
	if err != nil {
		return ws.Principal{}, ws.ErrNoCredential
	}
	if domain.APIKeyStatus(row.KeyStatus) != domain.APIKeyActive {
		// Revocation takes effect on the next check, which for an open
		// connection is the revalidation tick.
		return ws.Principal{}, ws.ErrForbidden
	}
	if row.InstanceID != instanceID {
		return ws.Principal{}, ws.ErrNotFound
	}
	if domain.TenantStatus(row.TenantStatus) != domain.TenantActive {
		return ws.Principal{}, ws.ErrForbidden
	}

	return ws.Principal{
		TenantID:     row.TenantID,
		RateLimitKey: "rl:conn:key:" + row.KeyID.String(),
	}, nil
}

func (a *wsAuthenticator) authorizeToken(ctx context.Context, raw string, instanceID domain.ID) (ws.Principal, error) {
	claims, err := a.issuer.Parse(raw)
	if err != nil || claims.Audience != domain.AudienceTenant || claims.TenantID == nil {
		return ws.Principal{}, ws.ErrForbidden
	}

	user, err := a.queries.GetUserByID(ctx, claims.Subject)
	if err != nil {
		return ws.Principal{}, ws.ErrForbidden
	}
	if user.TenantStatus == nil || domain.TenantStatus(*user.TenantStatus) != domain.TenantActive {
		return ws.Principal{}, ws.ErrForbidden
	}

	// Scoping by tenant in SQL is what makes another tenant's instance
	// indistinguishable from one that never existed.
	if _, err := a.queries.GetInstanceByIDAndTenant(ctx, store.GetInstanceByIDAndTenantParams{
		ID:       uuid.UUID(instanceID),
		TenantID: *claims.TenantID,
	}); err != nil {
		return ws.Principal{}, ws.ErrNotFound
	}

	return ws.Principal{
		TenantID:     *claims.TenantID,
		RateLimitKey: "rl:conn:tenant:" + claims.TenantID.String(),
	}, nil
}

// wsSnapshotter builds the first frame of a connection.
type wsSnapshotter struct {
	queries   *store.Queries
	publisher *events.Publisher
}

// Snapshot reports the instance state plus any pairing code in flight.
//
// It is stamped with the sequence already allocated for the instance, which is
// what lets the client drop the events the snapshot already reflects: the
// handler subscribes before reading this, so an overlap is expected.
func (s *wsSnapshotter) Snapshot(ctx context.Context, instanceID domain.ID) (events.Envelope, error) {
	row, err := s.queries.GetInstanceConnectionByID(ctx, uuid.UUID(instanceID))
	if err != nil {
		return events.Envelope{}, fmt.Errorf("load instance: %w", err)
	}

	seq, err := s.publisher.CurrentSeq(ctx, instanceID)
	if err != nil {
		return events.Envelope{}, err
	}

	data := map[string]any{
		"state":  row.ConnectionState,
		"intent": row.ConnectionIntent,
		"device": deviceData(row),
	}
	if row.ConnectedAt != nil {
		data["connected_at"] = row.ConnectedAt.UTC()
	} else {
		data["connected_at"] = nil
	}
	if row.LastDisconnectAt != nil {
		last := map[string]any{"at": row.LastDisconnectAt.UTC()}
		if row.LastDisconnectReason != nil {
			last["reason"] = *row.LastDisconnectReason
		}
		data["last_disconnect"] = last
	} else {
		data["last_disconnect"] = nil
	}

	// A client arriving mid-attempt must see the current code rather than wait
	// for the next rotation (FR-032).
	pairing, found, err := s.publisher.Pairing(ctx, instanceID)
	if err != nil {
		return events.Envelope{}, err
	}
	if found {
		data["pairing"] = map[string]any{
			"method":     pairing.Method,
			"code":       pairing.Code,
			"expires_at": pairing.ExpiresAt.UTC(),
		}
	} else {
		data["pairing"] = nil
	}

	return events.Envelope{
		Seq:        seq,
		Type:       events.TypeStateSnapshot,
		InstanceID: instanceID,
		OccurredAt: time.Now().UTC(),
		Data:       data,
	}, nil
}

// deviceData renders the paired device, or nil when the instance has none.
func deviceData(row store.Instance) any {
	if row.WaJid == nil {
		return nil
	}

	device := map[string]any{"jid": *row.WaJid}
	if row.PhoneNumber != nil {
		device["phone_number"] = *row.PhoneNumber
	}
	if row.PushName != nil {
		device["push_name"] = *row.PushName
	}
	if row.Platform != nil {
		device["platform"] = *row.Platform
	}
	if row.PairedAt != nil {
		device["paired_at"] = row.PairedAt.UTC()
	}
	return device
}
