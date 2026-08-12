package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx/v5" database/sql driver

	"github.com/zapperhub/zappermeow/migrations"
)

// MigrateResult reports what Migrate did, so the caller can log it.
type MigrateResult struct {
	// Version is the schema version after migrating.
	Version uint
	// Applied is false when the schema was already up to date.
	Applied bool
}

// Migrate applies every pending migration embedded in the binary. It runs at
// boot, before the super-admin bootstrap, so the schema is guaranteed to exist.
func Migrate(databaseURL string) (MigrateResult, error) {
	source, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return MigrateResult{}, fmt.Errorf("open embedded migrations: %w", err)
	}
	defer func() { _ = source.Close() }()

	db, err := sql.Open("pgx/v5", databaseURL)
	if err != nil {
		return MigrateResult{}, fmt.Errorf("open migration connection: %w", err)
	}
	defer func() { _ = db.Close() }()

	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{})
	if err != nil {
		return MigrateResult{}, fmt.Errorf("create migration driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		return MigrateResult{}, fmt.Errorf("create migrator: %w", err)
	}

	applied := true
	if err := m.Up(); err != nil {
		if !errors.Is(err, migrate.ErrNoChange) {
			return MigrateResult{}, fmt.Errorf("apply migrations: %w", err)
		}
		applied = false
	}

	version, dirty, err := m.Version()
	if err != nil {
		return MigrateResult{}, fmt.Errorf("read schema version: %w", err)
	}
	if dirty {
		return MigrateResult{}, fmt.Errorf("schema version %d is dirty: manual intervention required", version)
	}
	return MigrateResult{Version: version, Applied: applied}, nil
}
