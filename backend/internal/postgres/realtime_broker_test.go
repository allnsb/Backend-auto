//go:build integration && realtime && cluster

package postgres

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Only the isolated loopback broker is probed. RESP here covers a handful of
// security assertions; actual broker operations use the unmodified upstream engine.
func verifySharedBrokerSecurity(t *testing.T, ctx context.Context, broker *realtimeSharedBroker) {
	t.Helper()
	host, _, err := net.SplitHostPort(broker.Address)
	if err != nil || host != "localhost" || broker.Password == "" || !strings.HasPrefix(broker.Prefix, "odysseia_") {
		t.Fatal("isolated shared broker fixture is required")
	}
	rootsFrom := func(path string) *x509.CertPool {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			t.Fatal("invalid test CA")
		}
		return roots
	}
	config := &tls.Config{RootCAs: rootsFrom(filepath.Join(os.Getenv("ODYSSEIA_TEST_REALTIME_TLS_DIR"), "ca.crt")), MinVersion: tls.VersionTLS13}
	connect := func(config *tls.Config) (*tls.Conn, error) {
		t.Helper()
		dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: time.Second}, Config: config}
		connection, err := dialer.DialContext(ctx, "tcp", broker.Address)
		if err != nil {
			return nil, err
		}
		return connection.(*tls.Conn), nil
	}
	connection, err := connect(config)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if connection.ConnectionState().Version != tls.VersionTLS13 {
		t.Fatal("broker did not negotiate TLS 1.3")
	}
	if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	command := func(arguments ...string) string {
		t.Helper()
		var request strings.Builder
		fmt.Fprintf(&request, "*%d\r\n", len(arguments))
		for _, argument := range arguments {
			fmt.Fprintf(&request, "$%d\r\n%s\r\n", len(argument), argument)
		}
		if _, err := fmt.Fprint(connection, request.String()); err != nil {
			t.Fatal("broker probe write failed")
		}
		reply, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal("broker probe read failed")
		}
		return strings.TrimSpace(reply)
	}
	if !strings.HasPrefix(command("PING"), "-NOAUTH") {
		t.Fatal("anonymous broker access accepted")
	}
	if !strings.HasPrefix(command("AUTH", "odysseia_realtime", "invalid-fixture-password"), "-WRONGPASS") {
		t.Fatal("wrong broker password accepted")
	}
	if command("AUTH", "odysseia_realtime", broker.Password) != "+OK" || command("PING") != "+PONG" {
		t.Fatal("authenticated broker access failed")
	}
	if !strings.HasPrefix(command("GET", "outside_fixture_prefix"), "-NOPERM") {
		t.Fatal("broker ACL permits unrelated keys")
	}
	if !strings.HasPrefix(command("CONFIG", "GET", "dir"), "-NOPERM") {
		t.Fatal("broker ACL permits administrative configuration")
	}
	wrongCA := config.Clone()
	wrongCA.RootCAs = rootsFrom(os.Getenv("ODYSSEIA_TEST_UNTRUSTED_CA"))
	wrongHost := config.Clone()
	wrongHost.ServerName = "wrong-host.invalid"
	oldTLS := config.Clone()
	oldTLS.MinVersion, oldTLS.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
	for _, rejected := range []*tls.Config{wrongCA, wrongHost, oldTLS} {
		unexpected, err := connect(rejected)
		if err == nil {
			unexpected.Close()
			t.Fatal("broker accepted untrusted CA, wrong hostname or TLS 1.2")
		}
	}
}
