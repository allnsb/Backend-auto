package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/postgres"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		slog.Error("forum member removal failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: remove-forum-member (configuration via environment)")
	}
	command := forum.MemberRemoval{
		OperationID: os.Getenv("ODYSSEIA_OPERATION_ID"), ForumID: os.Getenv("ODYSSEIA_FORUM_ID"),
		UserID: os.Getenv("ODYSSEIA_FORUM_MEMBER_ID"), Operator: os.Getenv("ODYSSEIA_OPERATOR"), Reason: os.Getenv("ODYSSEIA_CHANGE_REASON"),
	}
	if err := command.Validate(); err != nil {
		return errors.New("provide canonical operation/forum/member UUIDs, operator label and change reason in environment")
	}
	url := os.Getenv("ODYSSEIA_MIGRATION_DATABASE_URL")
	if url == "" {
		return errors.New("ODYSSEIA_MIGRATION_DATABASE_URL is required")
	}
	pool, err := postgres.Open(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	result, err := postgres.NewForums(pool).RemoveMember(ctx, command)
	if err != nil {
		return err
	}
	slog.Info("membership removed; realtime disconnect queued", "operation_id", result.OperationID, "forum_id", result.ForumID, "removed", result.Removed)
	return nil
}
