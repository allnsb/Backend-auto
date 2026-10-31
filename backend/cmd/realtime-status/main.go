package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"time"

	"odysseia.local/backend/internal/postgres"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	code, err := run(ctx, os.Args[1:], os.Stdout)
	cancel()
	if err != nil {
		slog.Error("realtime backlog check failed", "error", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(ctx context.Context, args []string, output io.Writer) (int, error) {
	if len(args) != 0 {
		return 1, errors.New("usage: realtime-status (configuration via environment)")
	}
	threshold := 30 * time.Second
	if raw := os.Getenv("ODYSSEIA_BACKLOG_WARN_AFTER"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			return 1, errors.New("ODYSSEIA_BACKLOG_WARN_AFTER must be a positive duration")
		}
		threshold = parsed
	}
	dsn := os.Getenv("ODYSSEIA_OBSERVER_DATABASE_URL")
	if dsn == "" {
		return 1, errors.New("ODYSSEIA_OBSERVER_DATABASE_URL is required")
	}
	pool, err := postgres.Open(ctx, dsn)
	if err != nil {
		return 1, err
	}
	defer pool.Close()
	backlog, err := postgres.ReadRealtimeBacklog(ctx, pool)
	if err != nil {
		return 1, err
	}
	status, code := "within_threshold", 0
	if backlog.Pending > 0 && backlog.OldestAgeSeconds >= threshold.Seconds() {
		status, code = "backlog_age_exceeded", 2
	}
	err = json.NewEncoder(output).Encode(struct {
		postgres.RealtimeBacklog
		Status           string  `json:"status"`
		WarnAfterSeconds float64 `json:"warn_after_seconds"`
	}{backlog, status, threshold.Seconds()})
	return code, err
}
