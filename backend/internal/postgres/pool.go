package postgres

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func Open(ctx context.Context, connectionString string) (*pgxpool.Pool, error) {
	cfg, err := parseConfig(connectionString)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create PostgreSQL pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	return pool, nil
}

func parseConfig(connectionString string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(connectionString)
	if err != nil {
		// Invalid DSNs can contain secrets that driver errors cannot fully redact.
		return nil, errors.New("invalid PostgreSQL connection configuration; check the database environment variable")
	}
	if err := configureTLS(cfg.ConnConfig.TLSConfig); err != nil {
		return nil, err
	}
	for _, fallback := range cfg.ConnConfig.Fallbacks {
		if err := configureTLS(fallback.TLSConfig); err != nil {
			return nil, err
		}
	}
	if cfg.ConnConfig.ConnectTimeout == 0 {
		cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	}
	return cfg, nil
}

func configureTLS(cfg *tls.Config) error {
	if cfg == nil {
		return errors.New("PostgreSQL requires TLS with sslmode=verify-full")
	}
	if cfg.InsecureSkipVerify || cfg.ServerName == "" {
		return errors.New("PostgreSQL requires certificate and hostname verification with sslmode=verify-full")
	}
	cfg.MinVersion = tls.VersionTLS13
	return nil
}
