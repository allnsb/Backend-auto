package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"odysseia.local/backend/internal/blob"
	"odysseia.local/backend/internal/config"
	"odysseia.local/backend/internal/postgres"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		slog.Error("attachment sweep failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: attachment-sweep (configuration via environment)")
	}
	retention, err := config.AttachmentRetention()
	if err != nil {
		return err
	}
	dsn := os.Getenv("ODYSSEIA_DATABASE_URL")
	if dsn == "" {
		return errors.New("ODYSSEIA_DATABASE_URL is required")
	}
	storeCfg, err := config.LoadObjectStore()
	if err != nil {
		return err
	}
	pool, err := postgres.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	objects, err := blob.NewR2(ctx, blob.R2Config{
		AccountID: storeCfg.AccountID, AccessKey: storeCfg.AccessKeyID, SecretKey: storeCfg.SecretAccessKey,
		Bucket: storeCfg.Bucket, Endpoint: storeCfg.Endpoint, Region: storeCfg.Region,
	})
	if err != nil {
		return err
	}
	result, err := postgres.SweepAttachments(ctx, pool, objects, retention)
	slog.Info("attachment sweep complete",
		"removed_objects", result.RemovedObjects,
		"deleted_rows", result.DeletedRows,
		"expired_rows", result.ExpiredRows,
		"retention", retention.String(),
	)
	return err
}
