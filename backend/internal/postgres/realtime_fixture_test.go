//go:build integration && realtime

package postgres

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"odysseia.local/backend/internal/httpapi"
)

func grantRealtimeFixtureRoles(t *testing.T, ctx context.Context, owner *pgxpool.Pool) {
	t.Helper()
	_, err := owner.Exec(ctx, `GRANT USAGE ON SCHEMA odysseia TO odysseia_runtime, odysseia_realtime;
		GRANT SELECT ON odysseia.users, odysseia.forums, odysseia.forum_members, odysseia.message_streams, odysseia.topics, odysseia.messages, odysseia.forum_entries, odysseia.channels TO odysseia_runtime;
		GRANT INSERT ON odysseia.message_streams, odysseia.topics, odysseia.messages, odysseia.forum_entries, odysseia.channels TO odysseia_runtime;
		GRANT UPDATE(last_topic_position) ON odysseia.forums TO odysseia_runtime;
		GRANT UPDATE(last_position) ON odysseia.message_streams TO odysseia_runtime;
		GRANT INSERT(method,payload) ON odysseia.realtime_outbox TO odysseia_runtime;
		GRANT USAGE ON SEQUENCE odysseia.realtime_outbox_id_seq TO odysseia_runtime;
		GRANT SELECT, DELETE ON odysseia.realtime_outbox TO odysseia_realtime;
		GRANT USAGE ON SCHEMA odysseia TO odysseia_observer;
		GRANT SELECT ON odysseia.realtime_backlog TO odysseia_observer;`)
	if err != nil {
		t.Fatal(err)
	}
}

func newRealtimeSecret(t *testing.T) string {
	t.Helper()
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(data)
}

// Keep the fixture CA and hostname checks on the service callback, just as for WSS.
func newRealtimeFixtureServer(t *testing.T, ready func(context.Context) error, handler http.Handler) *httptest.Server {
	t.Helper()
	certificate, err := tls.LoadX509KeyPair(filepath.Join(os.Getenv("ODYSSEIA_TEST_REALTIME_TLS_DIR"), "server.crt"), filepath.Join(os.Getenv("ODYSSEIA_TEST_REALTIME_TLS_DIR"), "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	t.Cleanup(server.Close)
	server.Config = httpapi.NewServer("127.0.0.1:0", ready, handler)
	// Match production ServeTLS negotiation even though httptest calls Serve.
	server.Config.TLSConfig.NextProtos = []string{"h2", "http/1.1"}
	server.TLS = server.Config.TLSConfig.Clone()
	server.TLS.Certificates = []tls.Certificate{certificate}
	server.EnableHTTP2 = true
	server.StartTLS()
	server.URL = strings.Replace(server.URL, "127.0.0.1", "localhost", 1)
	return server
}
