package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/zapperhub/zappermeow/internal/api/httperr"
	"github.com/zapperhub/zappermeow/internal/api/middleware"
	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/domain/services"
)

// ConnectionHandler serves the routes that drive a session.
//
// Every one of them accepts either credential — the tenant's token or the
// instance's own API key — because provisioning and monitoring a number must be
// possible without a human logged in (FR-039).
type ConnectionHandler struct {
	connections *services.ConnectionService
}

// NewConnectionHandler builds the handler.
func NewConnectionHandler(connections *services.ConnectionService) *ConnectionHandler {
	return &ConnectionHandler{connections: connections}
}

// connectionSecurity declares both accepted schemes, so the generated OpenAPI
// shows them as alternatives rather than requirements.
var connectionSecurity = []map[string][]string{
	{"bearerAuth": {}},
	{"apiKeyAuth": {}},
}

// InstancePathInput addresses the instance a command acts on.
type InstancePathInput struct {
	InstanceID string `path:"instanceId" format:"uuid"`
}

// PairingData describes an attempt that was just started.
type PairingData struct {
	Method    string  `json:"method" enum:"qr,phone"`
	ExpiresAt *string `json:"expires_at,omitempty"`
}

// ConnectData is the connect payload.
type ConnectData struct {
	InstanceID string       `json:"instance_id" format:"uuid"`
	State      string       `json:"state"`
	Intent     string       `json:"intent" enum:"active,stopped"`
	Pairing    *PairingData `json:"pairing,omitempty"`
}

// DisconnectData is the disconnect payload.
type DisconnectData struct {
	InstanceID string `json:"instance_id" format:"uuid"`
	State      string `json:"state"`
	Intent     string `json:"intent" enum:"active,stopped"`
}

// LogoutData is the logout payload.
type LogoutData struct {
	InstanceID string `json:"instance_id" format:"uuid"`
	State      string `json:"state"`
	Intent     string `json:"intent" enum:"active,stopped"`
	// LogoutMode is honest about what happened: remote means the device was
	// removed on the server, local_only that only our copy was deleted and the
	// handset may still list it.
	LogoutMode string `json:"logout_mode" enum:"remote,local_only"`
}

// PairPhoneInput carries the number to pair.
type PairPhoneInput struct {
	InstanceID string `path:"instanceId" format:"uuid"`
	Body       struct {
		PhoneNumber   string `json:"phone_number" minLength:"7" maxLength:"20" example:"5511999999999"`
		ReplaceActive *bool  `json:"replace_active,omitempty"`
	}
}

// PairPhoneData carries the code to type on the handset.
type PairPhoneData struct {
	PairingCode string `json:"pairing_code" example:"ABCD-2345"`
	ExpiresAt   string `json:"expires_at"`
	State       string `json:"state"`
}

// SetProxyInput carries the egress proxy address.
type SetProxyInput struct {
	InstanceID string `path:"instanceId" format:"uuid"`
	Body       struct {
		// URL includes credentials when the proxy needs them. It is stored as
		// given and never echoed back: responses carry the masked form.
		URL string `json:"url" maxLength:"1024" example:"socks5://user:secret@203.0.113.10:1080"`
	}
}

// ProxyData reports the stored proxy and whether a live session was relinked.
type ProxyData struct {
	// ProxyURL is always masked, and null when the instance connects directly.
	ProxyURL *string `json:"proxy_url" example:"socks5://user:***@203.0.113.10:1080"`
	// Reconnecting is true when a running session was told to relink; false
	// means the setting applies from the next connection.
	Reconnecting bool `json:"reconnecting"`
}

// PasskeyResponseInput carries the authenticator's WebAuthn assertion.
type PasskeyResponseInput struct {
	InstanceID string `path:"instanceId" format:"uuid"`
	Body       struct {
		// Response is the assertion from navigator.credentials.get(), passed
		// through untouched: the platform never inspects it.
		Response json.RawMessage `json:"response"`
	}
}

// PasskeyStepData reports the pairing state after a passkey command.
type PasskeyStepData struct {
	InstanceID string `json:"instance_id" format:"uuid"`
	State      string `json:"state"`
}

// SetPassiveModeInput toggles passive mode.
type SetPassiveModeInput struct {
	InstanceID string `path:"instanceId" format:"uuid"`
	Body       struct {
		Enabled bool `json:"enabled"`
	}
}

// PassiveModeData reports the stored choice and whether it reached the session.
type PassiveModeData struct {
	PassiveMode bool `json:"passive_mode"`
	// Applied is true when a connected session took the change immediately.
	Applied bool `json:"applied"`
}

// Register mounts the connection routes.
func (h *ConnectionHandler) Register(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "set-instance-proxy",
		Method:      http.MethodPut,
		Path:        "/instances/{instanceId}/proxy",
		Summary:     "Set the egress proxy of an instance",
		Description: "All traffic for this instance — websocket and media — goes through the proxy, on every " +
			"connection and reconnection. There is no direct fallback: if the proxy is unreachable the instance " +
			"stays offline and retries through it. A connected instance is relinked immediately.",
		Tags:          []string{"connection"},
		Security:      connectionSecurity,
		DefaultStatus: http.StatusOK,
	}, h.setProxy)

	huma.Register(api, huma.Operation{
		OperationID:   "clear-instance-proxy",
		Method:        http.MethodDelete,
		Path:          "/instances/{instanceId}/proxy",
		Summary:       "Remove the egress proxy of an instance",
		Description:   "Puts the instance back on direct connections, relinking a running session immediately.",
		Tags:          []string{"connection"},
		Security:      connectionSecurity,
		DefaultStatus: http.StatusOK,
	}, h.clearProxy)

	huma.Register(api, huma.Operation{
		OperationID: "submit-instance-passkey-response",
		Method:      http.MethodPost,
		Path:        "/instances/{instanceId}/pairing/passkey/response",
		Summary:     "Answer a passkey challenge",
		Description: "Forwards the authenticator's WebAuthn assertion for the challenge published on the event " +
			"channel. Answers 202: what comes next — a handoff code, or the pairing simply completing — arrives " +
			"over the same channel.",
		Tags:          []string{"connection"},
		Security:      connectionSecurity,
		DefaultStatus: http.StatusAccepted,
	}, h.submitPasskeyResponse)

	huma.Register(api, huma.Operation{
		OperationID: "confirm-instance-passkey",
		Method:      http.MethodPost,
		Path:        "/instances/{instanceId}/pairing/passkey/confirm",
		Summary:     "Confirm a passkey handoff code",
		Description: "Confirms the code published on the event channel was shown to the number's owner and " +
			"matched their handset. Not needed when the confirmation was automatic.",
		Tags:          []string{"connection"},
		Security:      connectionSecurity,
		DefaultStatus: http.StatusAccepted,
	}, h.confirmPasskey)

	huma.Register(api, huma.Operation{
		OperationID: "set-instance-passive-mode",
		Method:      http.MethodPut,
		Path:        "/instances/{instanceId}/passive-mode",
		Summary:     "Toggle passive mode",
		Description: "In passive mode the instance keeps receiving everything but does not announce itself as an " +
			"active device. The choice is stored and reapplied on every connection.",
		Tags:          []string{"connection"},
		Security:      connectionSecurity,
		DefaultStatus: http.StatusOK,
	}, h.setPassiveMode)

	huma.Register(api, huma.Operation{
		OperationID: "connect-instance",
		Method:      http.MethodPost,
		Path:        "/instances/{instanceId}/connect",
		Summary:     "Bring an instance online",
		Description: "Starts pairing when the instance has no saved session, or reconnects when it has. " +
			"Answers 202: the QR code and the final transition arrive over the instance event channel.",
		Tags:          []string{"connection"},
		Security:      connectionSecurity,
		DefaultStatus: http.StatusAccepted,
	}, h.connect)

	huma.Register(api, huma.Operation{
		OperationID:   "pair-instance-by-phone",
		Method:        http.MethodPost,
		Path:          "/instances/{instanceId}/pair-phone",
		Summary:       "Pair an instance with a phone code",
		Description:   "Returns an eight-character code to type on the handset, with no QR involved.",
		Tags:          []string{"connection"},
		Security:      connectionSecurity,
		DefaultStatus: http.StatusOK,
	}, h.pairPhone)

	huma.Register(api, huma.Operation{
		OperationID:   "disconnect-instance",
		Method:        http.MethodPost,
		Path:          "/instances/{instanceId}/disconnect",
		Summary:       "Take an instance offline",
		Description:   "Keeps the session material, so reconnecting later needs no new pairing.",
		Tags:          []string{"connection"},
		Security:      connectionSecurity,
		DefaultStatus: http.StatusAccepted,
	}, h.disconnect)

	huma.Register(api, huma.Operation{
		OperationID: "logout-instance",
		Method:      http.MethodPost,
		Path:        "/instances/{instanceId}/logout",
		Summary:     "End the WhatsApp session",
		Description: "Removes the companion device on WhatsApp and deletes the session material. " +
			"Pairing again is required afterwards.",
		Tags:          []string{"connection"},
		Security:      connectionSecurity,
		DefaultStatus: http.StatusAccepted,
	}, h.logout)
}

func (h *ConnectionHandler) setProxy(ctx context.Context, in *SetProxyInput) (*httperr.Response[ProxyData], error) {
	principal, err := principalFrom(ctx)
	if err != nil {
		return nil, err
	}

	result, err := h.connections.SetProxy(ctx, principal.InstanceID, in.Body.URL)
	if err != nil {
		return nil, httperr.From(err)
	}

	return httperr.OK(ProxyData{
		ProxyURL:     &result.ProxyURL,
		Reconnecting: result.Reconnecting,
	}), nil
}

func (h *ConnectionHandler) clearProxy(ctx context.Context, _ *InstancePathInput) (*httperr.Response[ProxyData], error) {
	principal, err := principalFrom(ctx)
	if err != nil {
		return nil, err
	}

	result, err := h.connections.ClearProxy(ctx, principal.InstanceID)
	if err != nil {
		return nil, httperr.From(err)
	}

	return httperr.OK(ProxyData{Reconnecting: result.Reconnecting}), nil
}

func (h *ConnectionHandler) submitPasskeyResponse(ctx context.Context, in *PasskeyResponseInput) (*httperr.Response[PasskeyStepData], error) {
	principal, err := principalFrom(ctx)
	if err != nil {
		return nil, err
	}

	if len(in.Body.Response) == 0 {
		return nil, httperr.From(domain.ErrValidation("body.response", "must not be empty"))
	}

	if err := h.connections.SubmitPasskeyResponse(ctx, principal.InstanceID, in.Body.Response); err != nil {
		return nil, httperr.From(err)
	}

	return httperr.Respond(http.StatusAccepted, PasskeyStepData{
		InstanceID: principal.InstanceID.String(),
		State:      string(domain.InstancePairing),
	}), nil
}

func (h *ConnectionHandler) confirmPasskey(ctx context.Context, _ *InstancePathInput) (*httperr.Response[PasskeyStepData], error) {
	principal, err := principalFrom(ctx)
	if err != nil {
		return nil, err
	}

	if err := h.connections.ConfirmPasskey(ctx, principal.InstanceID); err != nil {
		return nil, httperr.From(err)
	}

	return httperr.Respond(http.StatusAccepted, PasskeyStepData{
		InstanceID: principal.InstanceID.String(),
		State:      string(domain.InstancePairing),
	}), nil
}

func (h *ConnectionHandler) setPassiveMode(ctx context.Context, in *SetPassiveModeInput) (*httperr.Response[PassiveModeData], error) {
	principal, err := principalFrom(ctx)
	if err != nil {
		return nil, err
	}

	result, err := h.connections.SetPassiveMode(ctx, principal.InstanceID, in.Body.Enabled)
	if err != nil {
		return nil, httperr.From(err)
	}

	return httperr.OK(PassiveModeData{
		PassiveMode: result.PassiveMode,
		Applied:     result.PassiveApplied,
	}), nil
}

func (h *ConnectionHandler) connect(ctx context.Context, _ *InstancePathInput) (*httperr.Response[ConnectData], error) {
	principal, err := principalFrom(ctx)
	if err != nil {
		return nil, err
	}

	result, err := h.connections.Connect(ctx, principal.InstanceID)
	if err != nil {
		return nil, httperr.From(err)
	}

	data := ConnectData{
		InstanceID: principal.InstanceID.String(),
		State:      string(result.State),
		Intent:     string(result.Intent),
	}
	if result.PairingStarted {
		data.Pairing = &PairingData{Method: "qr", ExpiresAt: result.PairingExpires}
	}
	return httperr.Respond(http.StatusAccepted, data), nil
}

func (h *ConnectionHandler) pairPhone(ctx context.Context, in *PairPhoneInput) (*httperr.Response[PairPhoneData], error) {
	principal, err := principalFrom(ctx)
	if err != nil {
		return nil, err
	}

	// Replacing an attempt in flight is the friendly default: a caller asking
	// for a code almost always means "this one, now".
	replaceActive := true
	if in.Body.ReplaceActive != nil {
		replaceActive = *in.Body.ReplaceActive
	}

	result, err := h.connections.PairPhone(ctx, principal.InstanceID, in.Body.PhoneNumber, replaceActive)
	if err != nil {
		return nil, httperr.From(err)
	}

	return httperr.OK(PairPhoneData{
		PairingCode: result.Code,
		ExpiresAt:   result.ExpiresAt,
		State:       string(domain.InstancePairing),
	}), nil
}

func (h *ConnectionHandler) disconnect(ctx context.Context, _ *InstancePathInput) (*httperr.Response[DisconnectData], error) {
	principal, err := principalFrom(ctx)
	if err != nil {
		return nil, err
	}

	state, err := h.connections.Disconnect(ctx, principal.InstanceID)
	if err != nil {
		return nil, httperr.From(err)
	}

	return httperr.Respond(http.StatusAccepted, DisconnectData{
		InstanceID: principal.InstanceID.String(),
		State:      string(state),
		Intent:     string(domain.IntentStopped),
	}), nil
}

func (h *ConnectionHandler) logout(ctx context.Context, _ *InstancePathInput) (*httperr.Response[LogoutData], error) {
	principal, err := principalFrom(ctx)
	if err != nil {
		return nil, err
	}

	result, err := h.connections.Logout(ctx, principal.InstanceID)
	if err != nil {
		return nil, httperr.From(err)
	}

	mode := "local_only"
	if result.RemoteRemoved {
		mode = "remote"
	}
	return httperr.Respond(http.StatusAccepted, LogoutData{
		InstanceID: principal.InstanceID.String(),
		State:      string(result.State),
		Intent:     string(domain.IntentStopped),
		LogoutMode: mode,
	}), nil
}

// principalFrom reads who authenticated. The middleware guarantees it is
// present and that it resolves to the instance in the URL, so its absence is a
// wiring bug rather than a client error.
func principalFrom(ctx context.Context) (middleware.ConnectionPrincipal, error) {
	principal, ok := middleware.ConnectionPrincipalFrom(ctx)
	if !ok {
		return middleware.ConnectionPrincipal{}, httperr.From(domain.ErrUnauthenticated("A credential is required"))
	}
	return principal, nil
}

// DeviceData is the paired companion device, or absent when there is none.
type DeviceData struct {
	JID          string  `json:"jid" example:"5511999999999:11@s.whatsapp.net"`
	LID          string  `json:"lid,omitempty"`
	PhoneNumber  string  `json:"phone_number"`
	PushName     string  `json:"push_name,omitempty"`
	Platform     string  `json:"platform,omitempty"`
	BusinessName string  `json:"business_name,omitempty"`
	PairedAt     *string `json:"paired_at,omitempty"`
}

// LastDisconnectData explains why the number went offline last.
type LastDisconnectData struct {
	At     string `json:"at"`
	Reason string `json:"reason,omitempty"`
}

// ConnectionStatusData is the full state of a session.
type ConnectionStatusData struct {
	InstanceID     string              `json:"instance_id" format:"uuid"`
	State          string              `json:"state"`
	Intent         string              `json:"intent" enum:"active,stopped"`
	ConnectedAt    *string             `json:"connected_at"`
	Device         *DeviceData         `json:"device"`
	LastDisconnect *LastDisconnectData `json:"last_disconnect"`
	BanExpiresAt   *string             `json:"ban_expires_at"`
	// SharesNumberWith names sibling instances paired to the same number.
	// Legitimate under multi-device, so it is context rather than a warning.
	SharesNumberWith []string `json:"shares_number_with"`
	// ProxyURL is the egress proxy with its password masked, null for a direct
	// connection. The stored credentials are never returned (FR-007).
	ProxyURL *string `json:"proxy_url" example:"socks5://user:***@203.0.113.10:1080"`
	// PassiveMode is the stored choice; it is reapplied on every connection.
	PassiveMode bool `json:"passive_mode"`
}

// ConnectionEventData is one entry of the trail.
type ConnectionEventData struct {
	Type       string         `json:"type"`
	Reason     string         `json:"reason,omitempty"`
	Detail     map[string]any `json:"detail,omitempty"`
	OccurredAt string         `json:"occurred_at"`
}

// ConnectionEventsData is a page of the trail.
type ConnectionEventsData struct {
	Events     []ConnectionEventData `json:"events"`
	NextBefore string                `json:"next_before,omitempty"`
}

// ConnectionEventsInput paginates the trail.
type ConnectionEventsInput struct {
	InstanceID string   `path:"instanceId" format:"uuid"`
	Limit      int32    `query:"limit" minimum:"1" maximum:"200" default:"50"`
	Before     string   `query:"before"`
	Type       []string `query:"type"`
}

// RegisterQueries mounts the read-only routes.
func (h *ConnectionHandler) RegisterQueries(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-instance-connection",
		Method:      http.MethodGet,
		Path:        "/instances/{instanceId}/connection",
		Summary:     "Read the connection state of an instance",
		Description: "Answers 200 even for an instance that was never paired: absence of a device is a " +
			"state, not an error.",
		Tags:          []string{"connection"},
		Security:      connectionSecurity,
		DefaultStatus: http.StatusOK,
	}, h.status)

	huma.Register(api, huma.Operation{
		OperationID:   "list-instance-connection-events",
		Method:        http.MethodGet,
		Path:          "/instances/{instanceId}/connection/events",
		Summary:       "Read the connection trail of an instance",
		Description:   "Newest first, keyset paginated. Entries older than the retention window are gone.",
		Tags:          []string{"connection"},
		Security:      connectionSecurity,
		DefaultStatus: http.StatusOK,
	}, h.events)
}

func (h *ConnectionHandler) status(ctx context.Context, _ *InstancePathInput) (*httperr.Response[ConnectionStatusData], error) {
	principal, err := principalFrom(ctx)
	if err != nil {
		return nil, err
	}

	status, err := h.connections.Status(ctx, principal.TenantID, principal.InstanceID)
	if err != nil {
		return nil, httperr.From(err)
	}

	data := ConnectionStatusData{
		InstanceID:       status.InstanceID.String(),
		State:            string(status.State),
		Intent:           string(status.Intent),
		ConnectedAt:      formatTime(status.ConnectedAt),
		BanExpiresAt:     formatTime(status.BanExpiresAt),
		SharesNumberWith: []string{},
		PassiveMode:      status.PassiveMode,
	}
	if status.ProxyURL != "" {
		data.ProxyURL = &status.ProxyURL
	}
	for _, id := range status.SharesNumberWith {
		data.SharesNumberWith = append(data.SharesNumberWith, id.String())
	}
	if status.Device != nil {
		data.Device = &DeviceData{
			JID:          status.Device.JID,
			LID:          status.Device.LID,
			PhoneNumber:  status.Device.PhoneNumber,
			PushName:     status.Device.PushName,
			Platform:     status.Device.Platform,
			BusinessName: status.Device.BusinessName,
		}
		if !status.Device.PairedAt.IsZero() {
			data.Device.PairedAt = formatTime(&status.Device.PairedAt)
		}
	}
	if status.LastDisconnectAt != nil {
		data.LastDisconnect = &LastDisconnectData{
			At:     status.LastDisconnectAt.UTC().Format(time.RFC3339),
			Reason: string(status.LastReason),
		}
	}
	return httperr.OK(data), nil
}

func (h *ConnectionHandler) events(ctx context.Context, in *ConnectionEventsInput) (*httperr.Response[ConnectionEventsData], error) {
	principal, err := principalFrom(ctx)
	if err != nil {
		return nil, err
	}

	var before *int64
	if in.Before != "" {
		id, err := services.DecodeCursor(in.Before)
		if err != nil {
			return nil, httperr.From(err)
		}
		before = &id
	}

	page, err := h.connections.Events(ctx, principal.TenantID, principal.InstanceID, in.Limit, before, in.Type)
	if err != nil {
		return nil, httperr.From(err)
	}

	data := ConnectionEventsData{
		Events:     make([]ConnectionEventData, 0, len(page.Events)),
		NextBefore: page.NextBefore,
	}
	for _, event := range page.Events {
		data.Events = append(data.Events, ConnectionEventData{
			Type:       string(event.Type),
			Reason:     string(event.Reason),
			Detail:     event.Detail,
			OccurredAt: event.OccurredAt.UTC().Format(time.RFC3339),
		})
	}
	return httperr.OK(data), nil
}

func formatTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339)
	return &formatted
}
