package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"odysseia.local/backend/internal/identity"
	"odysseia.local/backend/internal/postgres"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		slog.Error("user provisioning failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: provision-user (configuration via environment)")
	}
	external := identity.ExternalID{
		Issuer: os.Getenv("ODYSSEIA_AUTH_ISSUER"), Subject: os.Getenv("ODYSSEIA_PROVISION_SUBJECT"),
	}
	if external.Issuer == "" || external.Subject == "" {
		return errors.New("ODYSSEIA_AUTH_ISSUER and ODYSSEIA_PROVISION_SUBJECT are required")
	}
	connectionString := os.Getenv("ODYSSEIA_MIGRATION_DATABASE_URL")
	if connectionString == "" {
		return errors.New("ODYSSEIA_MIGRATION_DATABASE_URL is required")
	}
	pool, err := postgres.Open(ctx, connectionString)
	if err != nil {
		return err
	}
	defer pool.Close()
	user, err := postgres.NewUsers(pool).Provision(ctx, external)
	if err != nil {
		return err
	}
	slog.Info("user provisioned", "user_id", user.ID)
	return nil
}
