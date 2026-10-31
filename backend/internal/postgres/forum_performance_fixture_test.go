//go:build integration && performance

package postgres

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/httpapi"
	"odysseia.local/backend/internal/identity"
)

type performanceFixture struct {
	owner, pool *pgxpool.Pool
	client      *http.Client
	url, token  string
	actor, body string
	topics      []forum.CreatedTopic
	timings     *mixedAPITimings
}

// Both performance profiles use the same real API, privileges and seeded data.
// Cleanup closes API clients before pools; no production setup is changed.
func newPerformanceFixture(t *testing.T, ctx context.Context) *performanceFixture {
	t.Helper()
	ownerURL := fixtureURL(t, "ODYSSEIA_TEST_MIGRATION_DATABASE_URL")
	if _, err := MigrateUp(ctx, ownerURL); err != nil {
		t.Fatal(err)
	}
	owner, err := Open(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	_, err = owner.Exec(ctx, `GRANT USAGE ON SCHEMA odysseia TO odysseia_runtime;
		GRANT SELECT ON odysseia.users,odysseia.forums,odysseia.forum_members,odysseia.message_streams,odysseia.topics,odysseia.messages,odysseia.forum_entries TO odysseia_runtime;
		GRANT INSERT ON odysseia.messages,odysseia.forum_entries TO odysseia_runtime;
		GRANT UPDATE(last_position) ON odysseia.message_streams TO odysseia_runtime;
		GRANT INSERT(method,payload) ON odysseia.realtime_outbox TO odysseia_runtime;
		GRANT USAGE ON SEQUENCE odysseia.realtime_outbox_id_seq TO odysseia_runtime;`)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := parseConfig(fixtureURL(t, "ODYSSEIA_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = "odysseia_performance_api"
	timings := &mixedAPITimings{}
	timings.reset()
	cfg.ConnConfig.Tracer = timings
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	auth, verifier, sign := performanceIdentity(t, ctx)
	actor, err := NewUsers(owner).Provision(ctx, identity.ExternalID{Issuer: auth.Issuer, Subject: "performance-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	board, err := NewForums(owner).Provision(ctx, "isolated performance fixture", []string{actor.ID})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &performanceFixture{owner: owner, pool: pool, actor: actor.ID, body: strings.Repeat("x", 1024), timings: timings}
	for index := 0; index < 33; index++ {
		topic, err := NewForums(owner).CreateTopic(ctx, actor.ID, board.ID, uuid.NewString(), forum.TopicDraft{Title: "synthetic topic", Body: fixture.body, Mode: "threaded"})
		if err != nil {
			t.Fatal(err)
		}
		count := 256
		if index == 0 {
			count = 20000
		}
		seedPerformanceMessages(t, ctx, owner, topic.Topic.ID, actor.ID, fixture.body, count)
		fixture.topics = append(fixture.topics, topic)
	}
	if _, err := owner.Exec(ctx, `ANALYZE odysseia.users,odysseia.forums,odysseia.forum_members,odysseia.topics,odysseia.messages,odysseia.forum_entries`); err != nil {
		t.Fatal(err)
	}
	api := httpapi.NewAPI(verifier.Authenticate, NewUsers(pool).Find)
	api.RegisterForums(newCachedForumFixture(t, pool))
	server := httptest.NewUnstartedServer(nil)
	server.Config = httpapi.NewServer("127.0.0.1:0", pool.Ping, timings.wrap(api))
	// httptest calls Serve rather than production ServeTLS. Both configs must
	// advertise h2 so the server installs HTTP/2 before accepting connections.
	server.Config.TLSConfig.NextProtos = []string{"h2", "http/1.1"}
	server.TLS = server.Config.TLSConfig.Clone()
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	fixture.client, fixture.url, fixture.token = server.Client(), server.URL, sign("performance-fixture")
	return fixture
}
