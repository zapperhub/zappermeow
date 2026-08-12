package services

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zapperhub/zappermeow/internal/domain"
	"github.com/zapperhub/zappermeow/internal/store"
)

// BootstrapCredentials is the initial super-admin defined in the deploy
// configuration. Both fields empty means "not configured".
type BootstrapCredentials struct {
	Email    string
	Password string
}

// Configured reports whether both halves of the credential are present.
func (c BootstrapCredentials) Configured() bool { return c.Email != "" && c.Password != "" }

// BootstrapService creates the very first super-admin at boot.
type BootstrapService struct {
	pool     *pgxpool.Pool
	queries  *store.Queries
	recorder *EventRecorder
	logger   *slog.Logger
}

// NewBootstrapService builds the boot-time bootstrap use case.
func NewBootstrapService(pool *pgxpool.Pool, queries *store.Queries, recorder *EventRecorder, logger *slog.Logger) *BootstrapService {
	return &BootstrapService{pool: pool, queries: queries, recorder: recorder, logger: logger}
}

// Run creates the initial super-admin when none exists and the credential is
// configured. It is idempotent by design: once a super-admin exists the
// configuration is ignored for good, so rotating the bootstrap variables later
// has no effect (spec edge case).
func (s *BootstrapService) Run(ctx context.Context, creds BootstrapCredentials) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.queries.WithTx(tx)

	// Serialise concurrent replicas booting at the same time. The lock is held
	// until this transaction ends, so exactly one of them can create the admin.
	if err := q.LockBootstrap(ctx); err != nil {
		return fmt.Errorf("bootstrap: acquire advisory lock: %w", err)
	}

	existing, err := q.CountSuperAdmins(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap: count super-admins: %w", err)
	}

	if existing > 0 {
		if creds.Configured() {
			s.logger.Info("bootstrap credentials ignored: a super-admin already exists")
		}
		return nil
	}

	if !creds.Configured() {
		s.logger.Warn("no super-admin exists and no bootstrap credentials are configured: " +
			"no administrative access is possible until they are set")
		return nil
	}

	email := domain.NormalizeEmail(creds.Email)
	if err := domain.CollectErrors(
		domain.ValidateEmail("config.bootstrap_email", email),
		domain.ValidatePassword("config.bootstrap_password", creds.Password),
	); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	hash, err := domain.HashPassword(creds.Password)
	if err != nil {
		return fmt.Errorf("bootstrap: hash password: %w", err)
	}

	created, err := q.CreateUser(ctx, store.CreateUserParams{
		ID:                 domain.NewID(),
		Name:               "Super Admin",
		Email:              email,
		PasswordHash:       hash,
		Role:               string(domain.RoleSuperAdmin),
		TenantID:           nil,
		MustChangePassword: false,
	})
	if err != nil {
		return fmt.Errorf("bootstrap: create super-admin: %w", err)
	}

	if err := s.recorder.Record(ctx, q, domain.SecurityEvent{
		Type:        domain.EventBootstrapAdminCreate,
		ActorUserID: &created.ID,
		TargetType:  domain.TargetUser,
		TargetID:    &created.ID,
		Result:      domain.ResultSuccess,
		Metadata:    map[string]any{"role": string(domain.RoleSuperAdmin)},
	}); err != nil {
		return fmt.Errorf("bootstrap: record event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("bootstrap: commit: %w", err)
	}

	s.logger.Info("super-admin created from bootstrap configuration",
		slog.String("user_id", created.ID.String()))
	return nil
}
