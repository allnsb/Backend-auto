package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"odysseia.local/backend/internal/postgres"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		slog.Error("forum provisioning failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: provision-forum (configuration via environment)")
	}
	url := os.Getenv("ODYSSEIA_MIGRATION_DATABASE_URL")
	title := os.Getenv("ODYSSEIA_FORUM_TITLE")
	rawMembers := os.Getenv("ODYSSEIA_FORUM_MEMBER_IDS")
	if url == "" || title == "" || rawMembers == "" {
		return errors.New("ODYSSEIA_MIGRATION_DATABASE_URL, ODYSSEIA_FORUM_TITLE and ODYSSEIA_FORUM_MEMBER_IDS are required")
	}
	members := strings.Split(rawMembers, ",")
	for i, value := range members {
		id, err := uuid.Parse(strings.TrimSpace(value))
		if err != nil {
			return errors.New("ODYSSEIA_FORUM_MEMBER_IDS must contain comma-separated local user UUIDs")
		}
		members[i] = id.String()
	}
	pool, err := postgres.Open(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	result, err := postgres.NewForums(pool).Provision(ctx, title, members)
	if err != nil {
		return err
	}
	slog.Info("forum provisioned", "forum_id", result.ID)
	return nil
}
