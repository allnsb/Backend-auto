//go:build integration && realtime

package postgres

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Each start creates a fresh native process on the same address. Its memory broker
// state is lost, while a configured shared broker lives independently of this child.
type realtimeProcess struct {
	url, address, binary, configPath, logPath string
	tlsConfig                                 *tls.Config
	run                                       *realtimeProcessRun
}

type realtimeProcessRun struct {
	command *exec.Cmd
	done    chan struct{}
	err     error
	log     *os.File
}

type realtimeSharedBroker struct {
	Address, Password, Prefix string
}

// Load the deployed namespaces, including personal inbox security options.
// Existing forum tests deliberately keep just two events to force fallback.
func realtimeFixtureNamespaces(t *testing.T) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "realtime", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Channel struct {
			Namespaces []map[string]any `json:"namespaces"`
		} `json:"channel"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	for _, namespace := range config.Channel.Namespaces {
		if namespace["name"] != "inbox" {
			namespace["history_size"] = 2
		}
	}
	return config.Channel.Namespaces
}

func startRealtimeProcess(t *testing.T, ctx context.Context, dsn, connectionKey, proxyKey, proxyURL string, broker *realtimeSharedBroker) *realtimeProcess {
	t.Helper()
	binary := os.Getenv("ODYSSEIA_TEST_CENTRIFUGO_BINARY")
	version, err := exec.Command(binary, "version").Output()
	if err != nil || !strings.Contains(string(version), "Centrifugo v6.10.2") {
		t.Fatal("verified Centrifugo 6.10.2 executable is required")
	}
	dir := filepath.Join(os.Getenv("ODYSSEIA_TEST_REALTIME_RUNTIME"), "run-"+uuid.NewString())
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	tlsDir := os.Getenv("ODYSSEIA_TEST_REALTIME_TLS_DIR")
	ca, err := os.ReadFile(filepath.Join(tlsDir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("fixture CA invalid")
	}
	tlsConfig := &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	config := map[string]any{
		"log":         map[string]any{"level": "error"},
		"http_server": map[string]any{"address": "127.0.0.1", "port": port, "tls": map[string]any{"enabled": true, "cert_pem": filepath.Join(tlsDir, "server.crt"), "key_pem": filepath.Join(tlsDir, "server.key")}},
		"client":      map[string]any{"token": map[string]any{"hmac_secret_key": connectionKey}, "disallow_anonymous_connection_tokens": true, "expired_close_delay": "1s", "allowed_origins": []string{"https://localhost"}},
		"channel":     map[string]any{"proxy": map[string]any{"subscribe": map[string]any{"endpoint": proxyURL, "timeout": "2s", "http": map[string]any{"static_headers": map[string]string{"X-Odysseia-Proxy-Key": proxyKey}, "tls": map[string]any{"enabled": true, "server_ca_pem": filepath.Join(tlsDir, "ca.crt")}}}}, "namespaces": realtimeFixtureNamespaces(t)},
		"consumers":   []any{map[string]any{"enabled": true, "name": "odysseia_outbox", "type": "postgresql", "postgresql": map[string]any{"dsn": dsn, "outbox_table_name": "odysseia.realtime_outbox", "num_partitions": 5, "partition_poll_interval": "20ms"}}},
	}
	if broker != nil {
		config["engine"] = map[string]any{"type": "redis", "redis": map[string]any{
			"address": []string{broker.Address}, "user": "odysseia_realtime", "password": broker.Password, "prefix": broker.Prefix,
			"tls": map[string]any{"enabled": true, "server_ca_pem": filepath.Join(tlsDir, "ca.crt")},
		}}
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	p := &realtimeProcess{
		binary: binary, configPath: path, logPath: filepath.Join(dir, "centrifugo.log"),
		address: "localhost:" + strconv.Itoa(port), tlsConfig: tlsConfig,
	}
	p.url = "wss://" + p.address + "/connection/websocket"
	t.Cleanup(func() {
		if err := p.stop(); err != nil {
			t.Errorf("stop owned Centrifugo: %v", err)
		}
	})
	p.start(t, ctx)
	return p
}

func (p *realtimeProcess) start(t *testing.T, ctx context.Context) {
	t.Helper()
	if p.run != nil {
		t.Fatal("owned Centrifugo is already running")
	}
	log, err := os.OpenFile(p.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(p.binary, "-c", p.configPath)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(value), "CENTRIFUGO_") {
			command.Env = append(command.Env, value)
		}
	}
	command.Stdout, command.Stderr = log, log
	if err := command.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	run := &realtimeProcessRun{command: command, done: make(chan struct{}), log: log}
	p.run = run
	go func() { run.err = command.Wait(); close(run.done) }()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()
	for {
		connection, err := tls.DialWithDialer(&net.Dialer{Timeout: 100 * time.Millisecond}, "tcp", p.address, p.tlsConfig)
		if err == nil {
			connection.Close()
			return
		}
		select {
		case <-ticker.C:
		case <-run.done:
			t.Fatalf("Centrifugo exited: %v; private log %s", run.err, p.logPath)
		case <-deadline.C:
			t.Fatalf("Centrifugo did not start; private log %s", p.logPath)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

// stop kills only the child created by this fixture and waits before its port is reused.
func (p *realtimeProcess) stop() error {
	run := p.run
	if run == nil {
		return nil
	}
	if err := run.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill owned process: %w", err)
	}
	<-run.done
	p.run = nil
	return run.log.Close()
}
