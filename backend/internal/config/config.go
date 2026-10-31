package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
)

type Config struct {
	HTTPSAddr                string
	TLSCertFile              string
	TLSKeyFile               string
	DatabaseURL              string
	AuthIssuer               string
	AuthAudience             string
	AuthJWKSURL              string
	AuthCAFile               string
	RealtimeConnectionSecret string
	RealtimeProxySecret      string
	MaxWriteTransactions     int64
}

func Load() (Config, error) {
	cfg := Config{
		HTTPSAddr:                os.Getenv("ODYSSEIA_HTTPS_ADDR"),
		TLSCertFile:              os.Getenv("ODYSSEIA_TLS_CERT_FILE"),
		TLSKeyFile:               os.Getenv("ODYSSEIA_TLS_KEY_FILE"),
		DatabaseURL:              os.Getenv("ODYSSEIA_DATABASE_URL"),
		AuthIssuer:               os.Getenv("ODYSSEIA_AUTH_ISSUER"),
		AuthAudience:             os.Getenv("ODYSSEIA_AUTH_AUDIENCE"),
		AuthJWKSURL:              os.Getenv("ODYSSEIA_AUTH_JWKS_URL"),
		AuthCAFile:               os.Getenv("ODYSSEIA_AUTH_CA_FILE"),
		RealtimeConnectionSecret: os.Getenv("ODYSSEIA_REALTIME_CONNECTION_SECRET"),
		RealtimeProxySecret:      os.Getenv("ODYSSEIA_REALTIME_PROXY_SECRET"),
	}
	if cfg.HTTPSAddr == "" {
		cfg.HTTPSAddr = "127.0.0.1:8443"
	}
	if _, _, err := net.SplitHostPort(cfg.HTTPSAddr); err != nil {
		return Config{}, fmt.Errorf("ODYSSEIA_HTTPS_ADDR: %w", err)
	}
	if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
		return Config{}, errors.New("ODYSSEIA_TLS_CERT_FILE and ODYSSEIA_TLS_KEY_FILE are required")
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("ODYSSEIA_DATABASE_URL is required")
	}
	if cfg.AuthIssuer == "" || cfg.AuthAudience == "" || cfg.AuthJWKSURL == "" {
		return Config{}, errors.New("ODYSSEIA_AUTH_ISSUER, ODYSSEIA_AUTH_AUDIENCE and ODYSSEIA_AUTH_JWKS_URL are required")
	}
	if cfg.RealtimeConnectionSecret == "" || cfg.RealtimeProxySecret == "" {
		return Config{}, errors.New("ODYSSEIA_REALTIME_CONNECTION_SECRET and ODYSSEIA_REALTIME_PROXY_SECRET are required")
	}
	if cfg.RealtimeConnectionSecret == cfg.RealtimeProxySecret {
		return Config{}, errors.New("connection signing and internal proxy secrets must differ")
	}
	if raw := os.Getenv("ODYSSEIA_MAX_WRITE_TRANSACTIONS"); raw != "" {
		limit, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || limit < 0 {
			return Config{}, errors.New("ODYSSEIA_MAX_WRITE_TRANSACTIONS must be a nonnegative integer; zero selects the automatic budget")
		}
		cfg.MaxWriteTransactions = limit
	}
	return cfg, nil
}
