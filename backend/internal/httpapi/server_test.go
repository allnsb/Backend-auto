package httpapi

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(nil)
	server.Config = NewServer("127.0.0.1:0", func(context.Context) error { return nil }, http.NotFoundHandler())
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = server.Config.TLSConfig.Clone()
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func TestHealthUsesTLS13(t *testing.T) {
	server := newTestServer(t)
	response, err := server.Client().Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}
	if response.TLS == nil {
		t.Fatal("health response has no TLS connection")
	}
	if response.TLS.Version != tls.VersionTLS13 {
		t.Fatalf("TLS version = %x, want TLS 1.3", response.TLS.Version)
	}
}

func TestSlowBodyIsReleasedEvenAfterRequestContextExpires(t *testing.T) {
	finished := make(chan error, 1)
	expired := make(chan struct{})
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Millisecond)
		defer cancel()
		context.AfterFunc(ctx, func() { close(expired) })
		_, err := readJSON[map[string]string](w, r.WithContext(ctx))
		if err != nil {
			writeAPIError(ctx, w, err)
		}
		finished <- err
	})
	server := httptest.NewUnstartedServer(nil)
	server.Config = NewServer("127.0.0.1:0", func(context.Context) error { return nil }, api)
	// Exercise the configured policy with a short test deadline; zero stays disabled.
	if server.Config.ReadTimeout > 0 {
		server.Config.ReadTimeout = 100 * time.Millisecond
	}
	server.TLS = server.Config.TLSConfig.Clone()
	server.StartTLS()
	defer server.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig := server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	connection, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", u.Host, tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_, err = io.WriteString(connection, "POST /slow HTTP/1.1\r\nHost: localhost\r\nContent-Length: 100\r\nContent-Type: application/json\r\n\r\n{\"body\":\"")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-expired:
	case <-time.After(time.Second):
		t.Fatal("request context did not expire")
	}
	select {
	case err := <-finished:
		if !errors.Is(err, errRequestTimeout) {
			t.Fatalf("incomplete body should time out, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow request body outlived its context and retained the handler")
	}
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("slow body status=%d, want 408", response.StatusCode)
	}
}

func TestOversizedHeadersAreRejectedBeforeHealth(t *testing.T) {
	server := newTestServer(t)
	r, err := http.NewRequest(http.MethodGet, server.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-Padding", strings.Repeat("a", 64*1024))
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("oversized header status=%d, want 431", response.StatusCode)
	}
}

func TestReadinessTracksDependencyWithoutLeakingErrors(t *testing.T) {
	var unavailable atomic.Bool
	server := httptest.NewUnstartedServer(nil)
	server.Config = NewServer("127.0.0.1:0", func(context.Context) error {
		if unavailable.Load() {
			return errors.New("private database connection details")
		}
		return nil
	}, http.NotFoundHandler())
	server.TLS = server.Config.TLSConfig.Clone()
	server.StartTLS()
	t.Cleanup(server.Close)

	checkStatus := func(path string, want int) {
		t.Helper()
		response, err := server.Client().Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != want || len(body) != 0 {
			t.Fatalf("%s: status=%d, body length=%d", path, response.StatusCode, len(body))
		}
	}
	checkStatus("/readyz", http.StatusNoContent)
	unavailable.Store(true)
	checkStatus("/readyz", http.StatusServiceUnavailable)
	checkStatus("/healthz", http.StatusNoContent)
}

func TestTLS12IsRejected(t *testing.T) {
	server := newTestServer(t)
	probe, err := server.Client().Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	probe.Body.Close()
	if probe.StatusCode != http.StatusNoContent {
		t.Fatalf("TLS 1.3 probe status = %d, want 204", probe.StatusCode)
	}
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.MinVersion = tls.VersionTLS12
	transport.TLSClientConfig.MaxVersion = tls.VersionTLS12
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	response, err := client.Get(server.URL + "/healthz")
	if response != nil {
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("TLS 1.2 client connected to a TLS 1.3-only server")
	}
}

func TestPlainHTTPDoesNotReachHealth(t *testing.T) {
	server := newTestServer(t)
	client := &http.Client{Timeout: 2 * time.Second}
	url := strings.Replace(server.URL, "https://", "http://", 1) + "/healthz"
	response, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("plain HTTP status = %d, want 400", response.StatusCode)
	}
}

func TestMissingCertificateClosesListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	certFile := filepath.Join(t.TempDir(), "missing.crt")
	keyFile := filepath.Join(t.TempDir(), "missing.key")
	err = ServeTLS(context.Background(), NewServer(address, func(context.Context) error { return nil }, http.NotFoundHandler()), listener, certFile, keyFile)
	if err == nil {
		t.Fatal("server started without a certificate")
	}
	_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(time.Second))
	if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("listener after certificate failure: %v, want net.ErrClosed", err)
	}
}

func TestShutdownWaitsForActiveRequest(t *testing.T) {
	certificateSource := newTestServer(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	server := NewServer(listener.Addr().String(), func(context.Context) error { return nil }, http.NotFoundHandler())
	server.TLSConfig.Certificates = certificateSource.TLS.Certificates

	requestStarted := make(chan struct{})
	release := make(chan struct{})
	releaseRequest := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseRequest)
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	shutdownStarted := make(chan struct{})
	server.RegisterOnShutdown(func() { close(shutdownStarted) })
	serveDone := make(chan error, 1)
	go func() { serveDone <- ServeTLS(ctx, server, listener, "", "") }()

	responseDone := make(chan error, 1)
	go func() {
		client := &http.Client{Transport: certificateSource.Client().Transport, Timeout: 3 * time.Second}
		response, err := client.Get("https://" + listener.Addr().String() + "/healthz")
		if err != nil {
			responseDone <- err
			return
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			responseDone <- fmt.Errorf("request status = %d, want 204", response.StatusCode)
			return
		}
		responseDone <- nil
	}()

	select {
	case <-requestStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case <-shutdownStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not start")
	}
	select {
	case err := <-serveDone:
		t.Fatalf("server stopped before request completed: %v", err)
	default:
	}
	releaseRequest()
	select {
	case err := <-responseDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request did not finish")
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop")
	}
}
