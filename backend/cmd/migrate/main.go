package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"odysseia.local/backend/internal/postgres"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		slog.Error("migration failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) != 1 || args[0] != "up" {
		return errors.New("usage: migrate up")
	}
	connectionString := os.Getenv("ODYSSEIA_MIGRATION_DATABASE_URL")
	if connectionString == "" {
		return errors.New("ODYSSEIA_MIGRATION_DATABASE_URL is required")
	}
	applied, err := postgres.MigrateUp(ctx, connectionString)
	if err != nil {
		return err
	}
	slog.Info("migrations complete", "applied", applied)
	return nil
}
