//go:build integration && realtime && cluster && acceptance

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	centrifuge "github.com/centrifugal/centrifuge-go"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgconn"
	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/realtime"
)

// This optional acceptance test uses wall-clock expiry, not a shortened cache or
// fake clock: consumption stops beyond both the 60s history and connection TTLs.
func TestRealtimeBacklogExpiryAndHTTPHandoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 130*time.Second)
	t.Cleanup(cancel)
	f := newClusterFixture(t, ctx)
	observerURL := os.Getenv("ODYSSEIA_TEST_CLUSTER_OBSERVER_URL")
	u, err := url.Parse(observerURL)
	if err != nil || u.Path != "/odysseia_cluster_test" {
		t.Fatal("isolated observer database is required")
	}
	observer, err := Open(ctx, observerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(observer.Close)
	for _, query := range []string{"SELECT payload FROM odysseia.realtime_outbox", "SELECT id FROM odysseia.users", "DELETE FROM odysseia.realtime_outbox"} {
		_, err := observer.Exec(ctx, query)
		var permission *pgconn.PgError
		if !errors.As(err, &permission) || permission.Code != "42501" {
			t.Fatal("observer gained body access or mutation privileges")
		}
	}
	proxyURL := f.server.URL + "/_internal/realtime/subscribe"
	nodeA := startRealtimeProcess(t, ctx, f.consumerURL, f.connectionKey, f.proxyKey, proxyURL, f.broker)
	nodeB := startRealtimeProcess(t, ctx, f.consumerURL, f.connectionKey, f.proxyKey, proxyURL, f.broker)
	waitOutboxEmpty(t, ctx, f.owner)
	checkStatusCommand(t, ctx, observerURL, 0, 0, 0)
	getToken := func(centrifuge.ConnectionTokenEvent) (string, error) {
		var token realtime.Token
		status, err := fixtureHTTPJSON(ctx, f.server, "bob", http.MethodPost, "/v1/realtime/connection-token", &token)
		if err != nil {
			return "", err
		}
		if status != http.StatusOK {
			return "", fmt.Errorf("connection credential status %d", status)
		}
		return token.Token, nil
	}
	initialToken, err := getToken(centrifuge.ConnectionTokenEvent{})
	if err != nil {
		t.Fatal(err)
	}
	client := centrifuge.NewJsonClient(nodeB.url, centrifuge.Config{Token: initialToken, TLSConfig: nodeB.tlsConfig, GetToken: getToken})
	t.Cleanup(func() { client.Close() })
	ready := make(chan centrifuge.SubscribedEvent, 8)
	messages := make(chan centrifuge.PublicationEvent, 512)
	channel := "topic:" + f.topic.Topic.ID
	sub, err := client.NewSubscription(channel, centrifuge.SubscriptionConfig{Recoverable: true, Positioned: true})
	if err != nil {
		t.Fatal(err)
	}
	sub.OnSubscribed(func(e centrifuge.SubscribedEvent) { ready <- e })
	sub.OnPublication(func(e centrifuge.PublicationEvent) { messages <- e })
	if err := sub.Subscribe(); err != nil {
		t.Fatal(err)
	}
	if err := client.Connect(); err != nil {
		t.Fatal(err)
	}
	awaitRealtime(t, ctx, ready, "acceptance subscription")
	post := func(body string) forum.Entry {
		t.Helper()
		entry, err := f.store.Reply(ctx, f.bob.ID, f.topic.Topic.ID, uuid.NewString(), forum.ReplyDraft{Body: body})
		if err != nil {
			t.Fatal(err)
		}
		return entry
	}
	baseline := post("before cache expiry")
	readClusterMessages(t, ctx, &clusterPeer{messages: messages}, []forum.Entry{baseline})
	if err := client.Disconnect(); err != nil {
		t.Fatal(err)
	}
	missingFromCache := post("published before the outage; must expire from cache")
	waitOutboxEmpty(t, ctx, f.owner)

	// Keep Alice's raw connection answering pings but deliberately skip JWT
	// refresh. With the consumer unavailable, the real 60s lease must still end.
	aliceToken := f.connectionToken(t, f.alice)
	raw := connectRawRealtime(t, ctx, nodeA.url, nodeA.tlsConfig, aliceToken)
	if err := raw.WriteJSON(map[string]any{"id": 2, "subscribe": map[string]any{"channel": channel}}); err != nil {
		t.Fatal(err)
	}
	readRawRealtimeReply(t, raw, "subscribe")
	if err := raw.SetReadDeadline(time.Now().Add(75 * time.Second)); err != nil {
		t.Fatal(err)
	}
	expired := make(chan error, 1)
	go func() { expired <- watchUnrefreshedConnection(raw) }()
	failuresA, failuresB := consumerPermissionFailures(t, nodeA.logPath), consumerPermissionFailures(t, nodeB.logPath)
	if _, err := f.owner.Exec(ctx, "REVOKE SELECT ON odysseia.realtime_outbox FROM odysseia_realtime"); err != nil {
		t.Fatal(err)
	}
	pausedAt := time.Now()
	operation := forum.MemberRemoval{OperationID: uuid.NewString(), ForumID: f.board.ID, UserID: f.alice.ID, Operator: "P2 acceptance", Reason: "revocation while consumer is unavailable"}
	if _, err := NewForums(f.owner).RemoveMember(ctx, operation); err != nil {
		t.Fatal(err)
	}
	status, err := fixtureHTTPJSON(ctx, f.server, "alice", http.MethodGet, "/v1/topics/"+f.topic.Topic.ID+"/messages", nil)
	if err != nil || status != http.StatusNotFound {
		t.Fatal("revoked history access remained available during consumer outage")
	}
	queued := make([]forum.Entry, 0, 241)
	for range 240 {
		queued = append(queued, post("synthetic private backlog body"))
	}
	waitConsumerPermissionFailures(t, ctx, nodeA.logPath, failuresA+1)
	waitConsumerPermissionFailures(t, ctx, nodeB.logPath, failuresB+1)
	checkStatusCommand(t, ctx, observerURL, 2, 241, 1)
	timer := time.NewTimer(time.Until(pausedAt.Add(65 * time.Second)))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := awaitRealtime(t, ctx, expired, "unrefreshed 60s connection expires despite blocked disconnect queue"); err != nil {
		t.Fatal(err)
	}
	backlog, err := ReadRealtimeBacklog(ctx, observer)
	if err != nil || backlog.Pending != 241 || backlog.Publications != 240 || backlog.Disconnects != 1 || backlog.OldestAgeSeconds < 60 {
		t.Fatal("outage did not retain the complete aged backlog")
	}
	if err := client.Connect(); err != nil {
		t.Fatal(err)
	}
	recovered := awaitRealtime(t, ctx, ready, "reconnect after real history expiry")
	if !recovered.WasRecovering || recovered.Recovered {
		t.Fatal("expired history was incorrectly treated as continuous")
	}

	// Subscribe before the HTTPS snapshot, buffering publications while walking
	// opaque seek cursors. An insertion between pages must not disappear.
	var history []forum.Entry
	var after string
	pages := 0
	for {
		var page struct {
			Items []forum.Entry `json:"items"`
			Next  string        `json:"next"`
		}
		path := "/v1/topics/" + f.topic.Topic.ID + "/messages?limit=37&after=" + url.QueryEscape(after)
		status, err := fixtureHTTPJSON(ctx, f.server, "bob", http.MethodGet, path, &page)
		if err != nil || status != http.StatusOK {
			t.Fatal("authorized paginated fallback failed")
		}
		history = append(history, page.Items...)
		pages++
		if pages == 1 {
			queued = append(queued, post("inserted between snapshot pages"))
		}
		if page.Next == "" {
			break
		}
		if page.Next == after || pages > 10 {
			t.Fatal("snapshot cursor did not advance")
		}
		after = page.Next
	}
	merged := make(map[string]forum.Entry, len(history))
	var lastSequence int64
	for _, entry := range history {
		if _, duplicate := merged[entry.ID]; duplicate || entry.Sequence <= lastSequence {
			t.Fatal("snapshot duplicated or reordered messages")
		}
		merged[entry.ID] = entry
		lastSequence = entry.Sequence
	}
	if pages < 7 || len(merged) != 244 {
		t.Fatal("fallback did not traverse the complete multi-page snapshot")
	}
	expected := append([]forum.Entry{f.topic.OpeningMessage, baseline, missingFromCache}, queued...)
	for _, want := range expected {
		if merged[want.ID] != want {
			t.Fatal("snapshot lost committed content")
		}
	}
	if _, err := f.owner.Exec(ctx, "GRANT SELECT ON odysseia.realtime_outbox TO odysseia_realtime"); err != nil {
		t.Fatal(err)
	}
	seenEvents := make(map[string]bool, len(queued))
	for _, want := range queued {
		publication := awaitRealtime(t, ctx, messages, "ordered multi-batch backlog delivery")
		event := decodeForumPublication(t, publication, "forum.message.created", f.topic.Topic.ID)
		var entry forum.Entry
		if err := json.Unmarshal(event.Data, &entry); err != nil {
			t.Fatal(err)
		}
		if entry != want || seenEvents[event.ID] || event.Position != want.Sequence {
			t.Fatal("backlog delivery lost, duplicated or reordered an event")
		}
		seenEvents[event.ID] = true
		merged[entry.ID] = entry
	}
	waitOutboxEmpty(t, ctx, f.owner)
	if len(merged) != 244 {
		t.Fatal("HTTPS and buffered events created duplicate messages")
	}
	readClusterMessages(t, ctx, &clusterPeer{messages: messages}, []forum.Entry{post("live after paginated recovery")})
	waitOutboxEmpty(t, ctx, f.owner)
	checkStatusCommand(t, ctx, observerURL, 0, 0, 0)
	t.Logf("consumer paused %s; 241 queued publications and one disconnect recovered; 7-page snapshot merged without duplicate messages", time.Since(pausedAt))
}

// The fixture access tokens are synthetic; the real HTTPS routes, local-user
// lookup, permission checks and short-lived realtime credentials are exercised.
func fixtureHTTPJSON(ctx context.Context, server *httptest.Server, actor, method, path string, result any) (int, error) {
	request, err := http.NewRequestWithContext(ctx, method, server.URL+path, nil)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "Bearer "+actor)
	response, err := server.Client().Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || result == nil {
		return response.StatusCode, nil
	}
	return response.StatusCode, json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(result)
}

func checkStatusCommand(t *testing.T, ctx context.Context, dsn string, expectedCode int, pending, disconnects int64) {
	t.Helper()
	command := exec.CommandContext(ctx, os.Getenv("ODYSSEIA_TEST_STATUS_BINARY"))
	command.Env = append(os.Environ(), "ODYSSEIA_OBSERVER_DATABASE_URL="+dsn, "ODYSSEIA_BACKLOG_WARN_AFTER=1ms")
	output, err := command.Output()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal("status command did not execute")
	}
	if code != expectedCode {
		t.Fatalf("status command exit=%d, want %d", code, expectedCode)
	}
	var result struct {
		RealtimeBacklog
		Status string `json:"status"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal("status command returned invalid JSON")
	}
	if result.Pending != pending || result.Disconnects != disconnects || result.Publications != pending-disconnects {
		t.Fatal("status command returned incorrect aggregates")
	}
	if pending > 0 && result.Status != "backlog_age_exceeded" {
		t.Fatal("aged backlog did not produce an actionable warning")
	}
	if pending == 0 && result.Status != "within_threshold" {
		t.Fatal("empty queue did not clear the backlog warning")
	}
	if strings.Contains(string(output), "body") || strings.Contains(string(output), "password") || strings.Contains(string(output), "payload") {
		t.Fatal("status command exposed private details")
	}
}

func watchUnrefreshedConnection(connection *websocket.Conn) error {
	for {
		var reply map[string]json.RawMessage
		err := connection.ReadJSON(&reply)
		var closed *websocket.CloseError
		if errors.As(err, &closed) && closed.Code == 3005 && strings.Contains(closed.Text, "expir") {
			return nil
		}
		if err != nil {
			return fmt.Errorf("unrefreshed connection closed without expiry: %w", err)
		}
		if len(reply) == 0 {
			if err := connection.WriteJSON(struct{}{}); err != nil {
				return err
			}
			continue
		}
		var push struct {
			Pub        json.RawMessage `json:"pub"`
			Disconnect *struct {
				Code   int    `json:"code"`
				Reason string `json:"reason"`
			} `json:"disconnect"`
		}
		if err := json.Unmarshal(reply["push"], &push); err != nil {
			return errors.New("unexpected raw realtime frame")
		}
		if push.Pub != nil {
			return errors.New("revoked connection received a queued publication")
		}
		if push.Disconnect == nil || push.Disconnect.Code != 3005 || !strings.Contains(push.Disconnect.Reason, "expir") {
			return errors.New("unrefreshed connection did not receive expiry")
		}
		return nil
	}
}
