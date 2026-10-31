package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"odysseia.local/backend/internal/httpapi"
	"odysseia.local/backend/internal/mockidp"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("mock IdP stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	addr := envOr("ODYSSEIA_MOCK_IDP_ADDR", "127.0.0.1:9443")
	issuer := envOr("ODYSSEIA_MOCK_IDP_ISSUER", "https://127.0.0.1:9443")
	audience := envOr("ODYSSEIA_MOCK_IDP_AUDIENCE", "odysseia-api")
	certFile := os.Getenv("ODYSSEIA_MOCK_IDP_TLS_CERT_FILE")
	keyFile := os.Getenv("ODYSSEIA_MOCK_IDP_TLS_KEY_FILE")
	if certFile == "" || keyFile == "" {
		return errors.New("ODYSSEIA_MOCK_IDP_TLS_CERT_FILE and ODYSSEIA_MOCK_IDP_TLS_KEY_FILE are required")
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("ODYSSEIA_MOCK_IDP_ADDR: %w", err)
	}
	idp, err := mockidp.New(issuer)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen for mock IdP: %w", err)
	}
	slog.Info("mock IdP listening", "issuer", idp.Origin(), "jwks", idp.JWKSURL(), "audience", audience)
	server := &http.Server{
		Addr:              addr,
		Handler:           idp.Handler(audience),
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS13},
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		MaxHeaderBytes:    32 * 1024,
	}
	return httpapi.ServeTLS(ctx, server, listener, certFile, keyFile)
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
