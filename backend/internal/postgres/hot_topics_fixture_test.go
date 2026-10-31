//go:build integration && realtime && performance

package postgres

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/identity"
)

const fanoutHotTopics = 16
const fanoutOrdinaryTopics = 32
const fanoutUsers = 1024
const fanoutBodyBytes = 1024

type fanoutActor struct{ id, token string }
type fanoutTopic struct {
	id, root, child                    string
	kind, path, publication, eventType string
	drafts                             [3][]byte
}

type fanoutFixture struct {
	owner, probe, observer *pgxpool.Pool
	client                 *http.Client
	url, body              string
	users                  []fanoutActor
	topics                 []fanoutTopic
	process                *realtimeProcess
	api                    *fanoutAPIProcess
}

func fanoutDatabaseURL(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	u, err := url.Parse(value)
	if err != nil || u.Path != "/odysseia_realtime_test" {
		t.Fatalf("%s must select the isolated realtime fixture", name)
	}
	return value
}

// Only setup provisions identities and seed rows. Timed requests use the real
// RS256/JWKS API, runtime role, membership checks and native outbox consumer.
func newFanoutFixture(t *testing.T, ctx context.Context) *fanoutFixture {
	return newFanoutFixtureWithModes(t, ctx, false)
}

func newFanoutFixtureWithModes(t *testing.T, ctx context.Context, mixedModes bool) *fanoutFixture {
	t.Helper()
	ownerURL := fanoutDatabaseURL(t, "ODYSSEIA_TEST_REALTIME_MIGRATION_URL")
	if _, err := MigrateUp(ctx, ownerURL); err != nil {
		t.Fatal(err)
	}
	owner, err := Open(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	grantRealtimeFixtureRoles(t, ctx, owner)
	cfg, err := parseConfig(fanoutDatabaseURL(t, "ODYSSEIA_TEST_REALTIME_RUNTIME_URL"))
	if err != nil {
		t.Fatal(err)
	}
	probeConfig := cfg.Copy()
	probeConfig.MaxConns, probeConfig.ConnConfig.Tracer = 1, nil
	probeConfig.ConnConfig.RuntimeParams["application_name"] = "odysseia_performance_probe"
	probe, err := pgxpool.NewWithConfig(ctx, probeConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(probe.Close)
	observerConfig, err := parseConfig(fanoutDatabaseURL(t, "ODYSSEIA_TEST_REALTIME_OBSERVER_URL"))
	if err != nil {
		t.Fatal(err)
	}
	observerConfig.MaxConns = 1
	observer, err := pgxpool.NewWithConfig(ctx, observerConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(observer.Close)
	f := &fanoutFixture{owner: owner, probe: probe, observer: observer, body: strings.Repeat("x", fanoutBodyBytes)}
	auth, _, sign := performanceIdentity(t, ctx)
	var members []string
	for index := range fanoutUsers + 1 {
		subject := fmt.Sprintf("fanout-%04d", index)
		user, err := NewUsers(owner).Provision(ctx, identity.ExternalID{Issuer: auth.Issuer, Subject: subject})
		if err != nil {
			t.Fatal(err)
		}
		f.users = append(f.users, fanoutActor{user.ID, sign(subject)})
		members = append(members, user.ID)
	}
	board, err := NewForums(owner).Provision(ctx, "multi-hot threaded fixture", members)
	if err != nil {
		t.Fatal(err)
	}
	for index := range fanoutHotTopics + fanoutOrdinaryTopics {
		kind := "threaded"
		if mixedModes {
			kind = []string{"threaded", "chat", "channel"}[index%3]
		}
		topic := seedFanoutDiscussion(t, ctx, owner, members[0], board.ID, f.body, kind)
		root := topic.root
		if kind == "channel" {
			root = ""
		}
		for index, draft := range []forum.ReplyDraft{{Body: f.body}, {Body: f.body, RootID: root, QuoteID: topic.root}, {Body: f.body, RootID: root, QuoteID: topic.child}} {
			topic.drafts[index], err = json.Marshal(draft)
			if err != nil {
				t.Fatal(err)
			}
		}
		f.topics = append(f.topics, topic)
	}
	if _, err := owner.Exec(ctx, `ANALYZE odysseia.users,odysseia.forum_members,odysseia.topics,odysseia.channels,odysseia.messages,odysseia.forum_entries`); err != nil {
		t.Fatal(err)
	}
	connectionKey, proxyKey := newRealtimeSecret(t), newRealtimeSecret(t)
	readConnections := int32(0)
	switch os.Getenv("ODYSSEIA_TEST_POOL_LAYOUT") {
	case "", "shared16":
	case "split4_12":
		readConnections = 4
	case "split8_8":
		readConnections = 8
	default:
		t.Fatal("unknown experimental pool layout")
	}
	f.api = startFanoutAPI(t, ctx, fanoutAPIConfig{Auth: auth, ConnectionKey: connectionKey, ProxyKey: proxyKey, ProfileDir: os.Getenv("ODYSSEIA_TEST_API_PROFILE_DIR"), ReadConnections: readConnections})
	if readConnections > 0 && (f.api.ready.ReadPoolMax != readConnections || f.api.ready.PoolMax != 16) {
		t.Fatal("API did not apply the requested fixed total pool budget")
	}
	f.url, f.client = f.api.ready.URL, fanoutHTTPClient(t)
	f.process = startRealtimeProcess(t, ctx, fanoutDatabaseURL(t, "ODYSSEIA_TEST_REALTIME_CONSUMER_URL"), connectionKey, proxyKey, f.url+"/_internal/realtime/subscribe", nil)
	if path := os.Getenv("ODYSSEIA_TEST_PROCESS_OUTPUT"); path != "" {
		data, err := json.Marshal(map[string]int{"api": f.api.ready.PID, "generator": os.Getpid(), "centrifugo": f.process.run.command.Process.Pid})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	waitOutboxEmpty(t, ctx, owner)
	return f
}

func seedFanoutDiscussion(t *testing.T, ctx context.Context, owner *pgxpool.Pool, actor, forumID, body, kind string) fanoutTopic {
	t.Helper()
	store := NewForums(owner)
	if kind == "channel" {
		channel, err := store.CreateChannel(ctx, actor, forumID, uuid.NewString(), forum.ChannelDraft{Title: "fixed channel fixture"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = owner.Exec(ctx, `INSERT INTO odysseia.messages(id,stream_id,position,author_id,body,request_id,request_hash)
			SELECT gen_random_uuid(),$1,position,$2,$3,gen_random_uuid(),decode(repeat('ab',32),'hex') FROM generate_series(1,257) AS position`, channel.ID, actor, body)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(ctx, `UPDATE odysseia.message_streams SET last_position=257 WHERE id=$1`, channel.ID); err != nil {
			t.Fatal(err)
		}
		var first string
		if err := owner.QueryRow(ctx, `SELECT id::text FROM odysseia.messages WHERE stream_id=$1 AND position=1`, channel.ID).Scan(&first); err != nil {
			t.Fatal(err)
		}
		quoted, err := store.SendChannelMessage(ctx, actor, channel.ID, uuid.NewString(), forum.ChannelMessageDraft{Body: body, QuoteID: first})
		if err != nil {
			t.Fatal(err)
		}
		return fanoutTopic{id: channel.ID, root: first, child: quoted.ID, kind: kind, path: "/v1/channels/" + channel.ID, publication: "channel:" + channel.ID, eventType: "channel.message.created"}
	}
	created, err := store.CreateTopic(ctx, actor, forumID, uuid.NewString(), forum.TopicDraft{Title: kind + " fixture", Body: body, Mode: kind})
	if err != nil {
		t.Fatal(err)
	}
	seedPerformanceMessages(t, ctx, owner, created.Topic.ID, actor, body, 256)
	child, err := store.Reply(ctx, actor, created.Topic.ID, uuid.NewString(), forum.ReplyDraft{Body: body, RootID: created.OpeningMessage.ID, QuoteID: created.OpeningMessage.ID})
	if err != nil {
		t.Fatal(err)
	}
	return fanoutTopic{id: created.Topic.ID, root: created.OpeningMessage.ID, child: child.ID, kind: kind, path: "/v1/topics/" + created.Topic.ID, publication: "topic:" + created.Topic.ID, eventType: "forum.message.created"}
}

// Each caller supplies its own stable idempotency key. Transport/protocol
// failures are classified without retaining tokens, URLs or response bodies.
func (f *fanoutFixture) call(ctx context.Context, actor int, method, path, key string, body []byte) (int, []byte, string) {
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(requestCtx, method, f.url+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, "request_construction"
	}
	r.Header.Set("Authorization", "Bearer "+f.users[actor].token)
	if method == http.MethodPost && body != nil {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", key)
		lane := "hot_write"
		if actor == fanoutUsers {
			lane = "ordinary_write"
		}
		r.Header.Set("X-Odysseia-Performance-Lane", lane)
	}
	response, err := f.client.Do(r)
	if err != nil {
		if requestCtx.Err() != nil {
			return 0, nil, "timeout_or_canceled"
		}
		return 0, nil, "transport"
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return response.StatusCode, nil, "body_read"
	}
	if response.TLS == nil || response.TLS.Version != tls.VersionTLS13 || response.ProtoMajor != 2 {
		return response.StatusCode, nil, "protocol"
	}
	if response.StatusCode == http.StatusServiceUnavailable && method == http.MethodPost && body != nil {
		var problem struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(data, &problem); err != nil || problem.Code != "write_capacity_exceeded" || response.Header.Get("Retry-After") != "1" {
			return response.StatusCode, nil, "unexpected_503_contract"
		}
	}
	return response.StatusCode, data, ""
}

func (f *fanoutFixture) connectionToken(ctx context.Context, actor int) (string, error) {
	status, data, failure := f.call(ctx, actor, http.MethodPost, "/v1/realtime/connection-token", "", nil)
	if failure != "" || status != http.StatusOK {
		return "", fmt.Errorf("connection token: status %d, failure %s", status, failure)
	}
	var access struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &access); err != nil {
		return "", err
	}
	return access.Token, nil
}
