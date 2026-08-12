package services

import (
	"context"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/metrics"
	"github.com/zapperhub/zappermeow/internal/store"
)

// AuthService authenticates users and mints access tokens.
type AuthService struct {
	pool     *pgxpool.Pool
	queries  *store.Queries
	recorder *EventRecorder
	issuer   *domain.TokenIssuer

	maxFailures int
	lockWindow  time.Duration
}

// NewAuthService builds the authentication use case with the configured
// lockout policy.
func NewAuthService(
	pool *pgxpool.Pool,
	queries *store.Queries,
	recorder *EventRecorder,
	issuer *domain.TokenIssuer,
	maxFailures int,
	lockWindow time.Duration,
) *AuthService {
	return &AuthService{
		pool:        pool,
		queries:     queries,
		recorder:    recorder,
		issuer:      issuer,
		maxFailures: maxFailures,
		lockWindow:  lockWindow,
	}
}

// LoginInput is one authentication attempt.
type LoginInput struct {
	Email    string
	Password string
	SourceIP *netip.Addr
}

// LoginResult is the minted credential handed back to the client.
type LoginResult struct {
	AccessToken        string
	ExpiresIn          int
	Audience           domain.Audience
	MustChangePassword bool
}

// Login authenticates an email/password pair.
//
// The order of the checks is a security requirement, not an implementation
// detail: every failure before the password is verified answers with the exact
// same generic error (FR-019), and the only state ever revealed — that a tenant
// is suspended — is revealed after the caller has proven it holds the password.
func (s *AuthService) Login(ctx context.Context, in LoginInput) (LoginResult, error) {
	email := domain.NormalizeEmail(in.Email)

	row, err := s.queries.GetUserCredentialByEmail(ctx, email)
	if err != nil {
		if isNoRows(err) {
			// No actor to attribute this to; the email does not exist.
			s.recordLoginFailure(ctx, nil, in.SourceIP, "unknown_email")
			return LoginResult{}, domain.ErrInvalidCredentials()
		}
		return LoginResult{}, domain.ErrInternal(err)
	}

	user := userFromCredentialRow(row)
	now := time.Now().UTC()

	// A locked account is refused without even looking at the password, and
	// with the same answer as a wrong one — the lock stays invisible to an
	// attacker probing the account.
	if user.IsLocked(now) {
		metrics.LoginAttempts.WithLabelValues(metrics.OutcomeLocked).Inc()
		s.recordLoginFailure(ctx, &user.ID, in.SourceIP, "account_locked")
		return LoginResult{}, domain.ErrInvalidCredentials()
	}

	matches, err := domain.VerifyPassword(row.PasswordHash, in.Password)
	if err != nil {
		return LoginResult{}, domain.ErrInternal(err)
	}
	if !matches {
		s.registerFailedAttempt(ctx, user, in.SourceIP)
		return LoginResult{}, domain.ErrInvalidCredentials()
	}

	// The password is correct, so telling this caller that their tenant is
	// suspended leaks nothing an attacker could use for enumeration.
	if tenantStatusOf(row.TenantStatus) != domain.TenantActive {
		metrics.LoginAttempts.WithLabelValues(metrics.OutcomeFailed).Inc()
		s.recordEvent(ctx, domain.SecurityEvent{
			Type:        domain.EventLoginFailed,
			ActorUserID: &user.ID,
			TargetType:  domain.TargetUser,
			TargetID:    &user.ID,
			Result:      domain.ResultDenied,
			SourceIP:    in.SourceIP,
			Metadata:    map[string]any{"reason": "tenant_suspended"},
		})
		return LoginResult{}, domain.ErrTenantSuspended()
	}

	// A lockout that has simply expired is reported here, on the first
	// successful login after the window closed — there is no unlock job.
	unlocked := !user.LockedUntil.IsZero()

	if err := s.queries.ClearLoginFailures(ctx, user.ID); err != nil {
		return LoginResult{}, domain.ErrInternal(err)
	}
	if unlocked {
		s.recordEvent(ctx, domain.SecurityEvent{
			Type:        domain.EventAccountUnlocked,
			ActorUserID: &user.ID,
			TargetType:  domain.TargetUser,
			TargetID:    &user.ID,
			Result:      domain.ResultSuccess,
			SourceIP:    in.SourceIP,
			Metadata:    map[string]any{"reason": "lockout_expired"},
		})
	}

	token, claims, err := s.issuer.Issue(user.ID, user.Role.Audience(), user.TenantID, user.MustChangePassword)
	if err != nil {
		return LoginResult{}, domain.ErrInternal(err)
	}

	metrics.LoginAttempts.WithLabelValues(metrics.OutcomeSucceeded).Inc()
	s.recordEvent(ctx, domain.SecurityEvent{
		Type:        domain.EventLoginSucceeded,
		ActorUserID: &user.ID,
		TargetType:  domain.TargetUser,
		TargetID:    &user.ID,
		Result:      domain.ResultSuccess,
		SourceIP:    in.SourceIP,
		Metadata:    map[string]any{"audience": string(claims.Audience)},
	})

	return LoginResult{
		AccessToken:        token,
		ExpiresIn:          int(s.issuer.TTL().Seconds()),
		Audience:           claims.Audience,
		MustChangePassword: user.MustChangePassword,
	}, nil
}

// registerFailedAttempt increments the durable failure counter and locks the
// account once the configured threshold is reached.
func (s *AuthService) registerFailedAttempt(ctx context.Context, user domain.User, sourceIP *netip.Addr) {
	metrics.LoginAttempts.WithLabelValues(metrics.OutcomeFailed).Inc()

	result, err := s.queries.RegisterFailedLogin(ctx, store.RegisterFailedLoginParams{
		ID:          user.ID,
		MaxFailures: int32(s.maxFailures),
		LockSeconds: s.lockWindow.Seconds(),
	})
	if err != nil {
		s.recordLoginFailure(ctx, &user.ID, sourceIP, "bad_password")
		return
	}

	s.recordLoginFailure(ctx, &user.ID, sourceIP, "bad_password")

	// The account crossed the threshold on this attempt.
	if result.LockedUntil != nil && result.LockedUntil.After(time.Now().UTC()) {
		metrics.AccountLockouts.Inc()
		s.recordEvent(ctx, domain.SecurityEvent{
			Type:        domain.EventAccountLocked,
			ActorUserID: &user.ID,
			TargetType:  domain.TargetUser,
			TargetID:    &user.ID,
			Result:      domain.ResultDenied,
			SourceIP:    sourceIP,
			Metadata: map[string]any{
				"locked_until":  result.LockedUntil.UTC().Format(time.RFC3339),
				"max_failures":  s.maxFailures,
				"window_second": int(s.lockWindow.Seconds()),
			},
		})
	}
}

func (s *AuthService) recordLoginFailure(ctx context.Context, actor *domain.ID, sourceIP *netip.Addr, reason string) {
	event := domain.SecurityEvent{
		Type:     domain.EventLoginFailed,
		Result:   domain.ResultFailure,
		SourceIP: sourceIP,
		Metadata: map[string]any{"reason": reason},
	}
	if actor != nil {
		event.ActorUserID = actor
		event.TargetType = domain.TargetUser
		event.TargetID = actor
	}
	s.recordEvent(ctx, event)
}

// recordEvent writes an audit record on a read path, where there is no
// surrounding transaction to join. A failure to audit must not mask the
// authentication outcome, so it is swallowed after being logged by the recorder.
func (s *AuthService) recordEvent(ctx context.Context, event domain.SecurityEvent) {
	_ = s.recorder.Record(ctx, s.queries, event)
}
