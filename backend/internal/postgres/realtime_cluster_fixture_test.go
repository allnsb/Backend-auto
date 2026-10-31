//go:build integration && realtime && cluster

package postgres

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/httpapi"
	"odysseia.local/backend/internal/identity"
	"odysseia.local/backend/internal/realtime"
)

type clusterFixture struct {
	owner, pool                          *pgxpool.Pool
	store                                *Forums
	alice, bob                           identity.User
	board                                forum.Forum
	topic                                forum.CreatedTopic
	signer                               *realtime.Signer
	server                               *httptest.Server
	broker                               *realtimeSharedBroker
	connectionKey, proxyKey, consumerURL string
}

// Own the synthetic identities, grants, HTTPS endpoint and separate test database;
// individual tests choose their native processes and fault schedules.
func newClusterFixture(t *testing.T, ctx context.Context) *clusterFixture {
	t.Helper()
	fixtureURL := func(name string) string {
		t.Helper()
		value := os.Getenv(name)
		u, err := url.Parse(value)
		if err != nil || u.Path != "/odysseia_cluster_test" {
			t.Fatalf("%s must select the isolated cluster fixture", name)
		}
		return value
	}
	ownerURL := fixtureURL("ODYSSEIA_TEST_CLUSTER_MIGRATION_URL")
	if _, err := MigrateUp(ctx, ownerURL); err != nil {
		t.Fatal(err)
	}
	owner, err := Open(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	grantRealtimeFixtureRoles(t, ctx, owner)
	pool, err := Open(ctx, fixtureURL("ODYSSEIA_TEST_CLUSTER_RUNTIME_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	users := NewUsers(owner)
	alice, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://cluster-fixture.example", Subject: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://cluster-fixture.example", Subject: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	board, err := NewForums(owner).Provision(ctx, "two-node fixture", []string{alice.ID, bob.ID})
	if err != nil {
		t.Fatal(err)
	}
	connectionKey, proxyKey := newRealtimeSecret(t), newRealtimeSecret(t)
	signer, err := realtime.NewSigner(connectionKey, realtime.TokenTTL)
	if err != nil {
		t.Fatal(err)
	}
	store := newCachedForumFixture(t, pool)
	api := httpapi.NewAPI(func(_ context.Context, raw string) (identity.Authentication, error) {
		if raw != "alice" && raw != "bob" {
			return identity.Authentication{}, errors.New("invalid synthetic access token")
		}
		return identity.Authentication{ExternalID: identity.ExternalID{Issuer: "https://cluster-fixture.example", Subject: raw}, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}, NewUsers(pool).Find)
	api.RegisterForums(store)
	api.RegisterChannels(store)
	if err := api.RegisterRealtime(signer, store, proxyKey); err != nil {
		t.Fatal(err)
	}
	server := newRealtimeFixtureServer(t, pool.Ping, api)
	topic, err := store.CreateTopic(ctx, alice.ID, board.ID, uuid.NewString(), forum.TopicDraft{Title: "shared history", Body: "opening", Mode: "threaded"})
	if err != nil {
		t.Fatal(err)
	}

	broker := &realtimeSharedBroker{Address: os.Getenv("ODYSSEIA_TEST_VALKEY_ADDRESS"), Password: os.Getenv("ODYSSEIA_TEST_VALKEY_PASSWORD"), Prefix: os.Getenv("ODYSSEIA_TEST_VALKEY_PREFIX")}
	verifySharedBrokerSecurity(t, ctx, broker)
	return &clusterFixture{owner: owner, pool: pool, store: store, alice: alice, bob: bob, board: board, topic: topic, signer: signer, server: server, broker: broker, connectionKey: connectionKey, proxyKey: proxyKey, consumerURL: fixtureURL("ODYSSEIA_TEST_CLUSTER_CONSUMER_URL")}
}

func (f *clusterFixture) connectionToken(t *testing.T, user identity.User) string {
	t.Helper()
	token, err := f.signer.Connection(identity.Actor{User: user, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return token.Token
}
