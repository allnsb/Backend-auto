package httpapi

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

func NewServer(address string, checkReady func(context.Context) error, api http.Handler) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("/", api)
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := checkReady(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	return &http.Server{
		Addr:              address,
		Handler:           mux,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS13},
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		MaxHeaderBytes:    32 * 1024,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       time.Minute,
	}
}

// ServeTLS owns the listener and waits for active requests on cancellation.
func ServeTLS(ctx context.Context, server *http.Server, listener net.Listener, certFile, keyFile string) error {
	defer listener.Close()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.ServeTLS(listener, certFile, keyFile)
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve HTTPS: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		return fmt.Errorf("shut down HTTPS server: %w", err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve HTTPS: %w", err)
	}
	return nil
}
