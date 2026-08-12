// Package handlers holds the typed huma operations. Each handler translates a
// request into a service call and a service result into the standard envelope;
// it never builds a response or an error document by hand.
package handlers

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/zapperhub/zappermeow/internal/api/httperr"
	"github.com/zapperhub/zappermeow/internal/api/middleware"
	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/domain/services"
)

// AuthHandler serves the authentication operations.
type AuthHandler struct {
	auth *services.AuthService
}

// NewAuthHandler builds the authentication handlers.
func NewAuthHandler(auth *services.AuthService) *AuthHandler {
	return &AuthHandler{auth: auth}
}

// LoginInput is the login request body.
type LoginInput struct {
	Body struct {
		Email    string `json:"email" minLength:"1" maxLength:"320" example:"admin@acme.com" doc:"Registered email address."`
		Password string `json:"password" minLength:"1" doc:"Account password."`
	}
}

// LoginData is the minted credential.
type LoginData struct {
	AccessToken string `json:"access_token" doc:"Short-lived JWT access token."`
	TokenType   string `json:"token_type" example:"Bearer" doc:"Always \"Bearer\"."`
	ExpiresIn   int    `json:"expires_in" example:"3600" doc:"Token lifetime in seconds."`
	Audience    string `json:"audience" enum:"platform,tenant" doc:"Plane this token authenticates into."`
	// MustChangePassword tells the client to send the user straight to the
	// password change route: no other operation will be accepted until then.
	MustChangePassword bool `json:"must_change_password" doc:"Whether a password change is required before any other operation."`
}

// Register mounts the authentication operations. Login is public by necessity;
// it is protected by the per-origin rate limiter instead of a credential.
func (h *AuthHandler) Register(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "login",
		Method:      http.MethodPost,
		Path:        "/auth/login",
		Summary:     "Authenticate with email and password",
		Description: "Returns a short-lived token whose audience follows the user's role: `platform` for the " +
			"super-admin, `tenant` for a tenant admin. Every failure — unknown email, wrong password or a " +
			"locked account — answers with the same generic 401.",
		Tags:          []string{"auth"},
		DefaultStatus: http.StatusOK,
	}, h.login)
}

func (h *AuthHandler) login(ctx context.Context, in *LoginInput) (*httperr.Response[LoginData], error) {
	result, err := h.auth.Login(ctx, services.LoginInput{
		Email:    in.Body.Email,
		Password: in.Body.Password,
		SourceIP: middleware.ClientIPFrom(ctx),
	})
	if err != nil {
		return nil, httperr.From(err)
	}

	return httperr.OK(LoginData{
		AccessToken:        result.AccessToken,
		TokenType:          "Bearer",
		ExpiresIn:          result.ExpiresIn,
		Audience:           string(result.Audience),
		MustChangePassword: result.MustChangePassword,
	}), nil
}

// ChangePasswordInput is the password change request.
type ChangePasswordInput struct {
	Body struct {
		CurrentPassword string `json:"current_password" minLength:"1" doc:"The password in force, or the temporary one just issued."`
		NewPassword     string `json:"new_password" minLength:"8" doc:"The replacement, at least 8 characters."`
	}
}

// PasswordHandler serves the password operations.
type PasswordHandler struct {
	passwords *services.PasswordService
}

// NewPasswordHandler builds the password handlers.
func NewPasswordHandler(passwords *services.PasswordService) *PasswordHandler {
	return &PasswordHandler{passwords: passwords}
}

// RegisterChange mounts the password change. It is registered on the group that
// accepts either audience and tolerates a pending temporary password, because
// it is the one operation such a session is allowed to perform.
func (h *PasswordHandler) RegisterChange(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "change-password",
		Method:      http.MethodPost,
		Path:        "/auth/password",
		Summary:     "Change your own password",
		Description: "Requires the current password. Succeeding invalidates the previous password and every " +
			"token issued before the change, and clears a pending temporary password.",
		Tags:          []string{"auth"},
		Security:      []map[string][]string{{"bearerAuth": {}}},
		DefaultStatus: http.StatusNoContent,
	}, h.change)
}

func (h *PasswordHandler) change(ctx context.Context, in *ChangePasswordInput) (*DeleteResponse, error) {
	admin, ok := middleware.AdminFrom(ctx)
	if !ok {
		return nil, httperr.From(domain.ErrUnauthenticated("A bearer token is required"))
	}

	if err := h.passwords.Change(ctx, services.ChangePasswordInput{
		UserID:          admin.User.ID,
		CurrentPassword: in.Body.CurrentPassword,
		NewPassword:     in.Body.NewPassword,
		SourceIP:        middleware.ClientIPFrom(ctx),
	}); err != nil {
		return nil, httperr.From(err)
	}
	return nil, nil
}

// ResetPasswordData is the one-shot temporary password.
type ResetPasswordData struct {
	TemporaryPassword  string `json:"temporary_password" doc:"Shown exactly once. Hand it to the administrator; it cannot be retrieved again."`
	MustChangePassword bool   `json:"must_change_password" doc:"Always true: the admin must replace it before any other operation."`
}

// RegisterReset mounts the super-admin reset onto the platform group.
func (h *PasswordHandler) RegisterReset(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "reset-tenant-admin-password",
		Method:      http.MethodPost,
		Path:        "/admin/tenants/{tenantId}/admin/reset-password",
		Summary:     "Reset a tenant administrator's password",
		Description: "Generates a temporary password, shown exactly once. The administrator must replace it on " +
			"the next login before anything else is permitted.",
		Tags:          []string{"tenants"},
		Security:      []map[string][]string{{"bearerAuth": {}}},
		DefaultStatus: http.StatusOK,
	}, h.reset)
}

func (h *PasswordHandler) reset(ctx context.Context, in *TenantIDInput) (*httperr.Response[ResetPasswordData], error) {
	admin, _ := middleware.AdminFrom(ctx)
	tenantID, err := domain.ParseID("path.tenantId", in.TenantID)
	if err != nil {
		return nil, httperr.From(err)
	}

	result, err := h.passwords.ResetTenantAdmin(ctx, services.ResetTenantAdminInput{
		TenantID: tenantID,
		ActorID:  admin.User.ID,
		SourceIP: middleware.ClientIPFrom(ctx),
	})
	if err != nil {
		return nil, httperr.From(err)
	}

	return httperr.OK(ResetPasswordData{
		TemporaryPassword:  result.TemporaryPassword,
		MustChangePassword: true,
	}), nil
}
