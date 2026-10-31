package postgres

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrations embed.FS

// MigrateUp is the explicit schema-changing entry point, separate from API startup.
func MigrateUp(ctx context.Context, connectionString string) (int, error) {
	cfg, err := parseConfig(connectionString)
	if err != nil {
		return 0, err
	}
	db := stdlib.OpenDB(*cfg.ConnConfig)
	defer db.Close()
	db.SetMaxOpenConns(1)

	files, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return 0, fmt.Errorf("open embedded migrations: %w", err)
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return 0, fmt.Errorf("configure migration lock: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, files,
		goose.WithSessionLocker(locker),
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		return 0, fmt.Errorf("configure migrations: %w", err)
	}
	applied, err := provider.Up(ctx)
	if err != nil {
		return 0, fmt.Errorf("apply migrations: %w", err)
	}
	return len(applied), nil
}
