package db

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrator applies the embedded schema migrations.
type Migrator struct {
	m *migrate.Migrate
}

// NewMigrator binds the embedded migrations to an open database.
func NewMigrator(sqlDB *sql.DB) (*Migrator, error) {
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("migrations source: %w", err)
	}
	driver, err := postgres.WithInstance(sqlDB, &postgres.Config{})
	if err != nil {
		return nil, fmt.Errorf("migrations driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", driver)
	if err != nil {
		return nil, fmt.Errorf("migrator: %w", err)
	}
	return &Migrator{m: m}, nil
}

// Up applies every pending migration.
func (m *Migrator) Up() error {
	if err := m.m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// Down rolls back n migrations.
func (m *Migrator) Down(n int) error {
	if n <= 0 {
		return errors.New("migrate down: n must be positive")
	}
	if err := m.m.Steps(-n); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate down: %w", err)
	}
	return nil
}

// Version returns the current schema version and whether it is dirty.
func (m *Migrator) Version() (version uint, dirty bool, err error) {
	version, dirty, err = m.m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("migrate version: %w", err)
	}
	return version, dirty, nil
}

// RunMigrations applies all pending migrations.
func RunMigrations(sqlDB *sql.DB) error {
	m, err := NewMigrator(sqlDB)
	if err != nil {
		return err
	}
	return m.Up()
}
