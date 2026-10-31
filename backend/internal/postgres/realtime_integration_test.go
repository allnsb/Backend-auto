//go:build integration && realtime

package postgres

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	centrifuge "github.com/centrifugal/centrifuge-go"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/httpapi"
	"odysseia.local/backend/internal/identity"
	"odysseia.local/backend/internal/realtime"
)

func TestRealtimeOutboxWSSRecoveryAndRevocation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	fixtureURL := func(name string) string {
		t.Helper()
		value := os.Getenv(name)
		u, err := url.Parse(value)
		if err != nil || u.Path != "/odysseia_realtime_test" {
			t.Fatalf("%s must select the isolated realtime fixture", name)
		}
		return value
	}
	ownerURL := fixtureURL("ODYSSEIA_TEST_REALTIME_MIGRATION_URL")
	if _, err := MigrateUp(ctx, ownerURL); err != nil {
		t.Fatal(err)
	}
	owner, err := Open(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	grantRealtimeFixtureRoles(t, ctx, owner)
	pool, err := Open(ctx, fixtureURL("ODYSSEIA_TEST_REALTIME_RUNTIME_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	users := NewUsers(owner)
	alice, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://realtime-fixture.example", Subject: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://realtime-fixture.example", Subject: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	board, err := NewForums(owner).Provision(ctx, "WSS fixture", []string{alice.ID, bob.ID})
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
		return identity.Authentication{ExternalID: identity.ExternalID{Issuer: "https://realtime-fixture.example", Subject: raw}, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}, NewUsers(pool).Find)
	api.RegisterForums(store)
	if err := api.RegisterRealtime(signer, store, proxyKey); err != nil {
		t.Fatal(err)
	}
	var gateArmed atomic.Bool
	gateReady, gateRelease := make(chan struct{}), make(chan struct{})
	releaseGate := sync.OnceFunc(func() { close(gateRelease) })
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_internal/realtime/subscribe" || !gateArmed.CompareAndSwap(true, false) {
			api.ServeHTTP(w, r)
			return
		}
		recorder := httptest.NewRecorder()
		api.ServeHTTP(recorder, r)
		close(gateReady)
		<-gateRelease
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	})
	server := newRealtimeFixtureServer(t, pool.Ping, handler)
	defer releaseGate()
	type response struct {
		status int
		data   []byte
		header http.Header
	}
	call := func(actor, method, path, key string, body any) response {
		t.Helper()
		var data []byte
		if body != nil {
			data, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		r, err := http.NewRequestWithContext(ctx, method, server.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+actor)
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		if body != nil {
			r.Header.Set("Content-Type", "application/json")
		}
		res, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, err = io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response{res.StatusCode, data, res.Header}
	}
	contract := call("alice", "GET", "/openapi.json", "", nil)
	doc, err := openapi3.NewLoader().LoadFromData(contract.data)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	checkTokenResponse := func(got response, path string, status int) {
		t.Helper()
		if got.status != status || got.header.Get("Cache-Control") != "no-store" {
			t.Fatalf("token response status=%d, want=%d, private caching required", got.status, status)
		}
		definition := doc.Paths.Value(path).Post.Responses.Value(strconv.Itoa(status)).Value
		media := definition.Content[got.header.Get("Content-Type")]
		if media == nil {
			t.Fatal("token response content type is absent from OpenAPI")
		}
		var value any
		if err := json.Unmarshal(got.data, &value); err != nil {
			t.Fatal(err)
		}
		if err := media.Schema.Value.VisitJSON(value); err != nil {
			t.Fatalf("token response violates OpenAPI: %v", err)
		}
	}
	type access struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
		Channel   string `json:"channel"`
	}
	getAccess := func(actor, path string) access {
		t.Helper()
		res := call(actor, "POST", path, "", nil)
		contractPath := "/v1/realtime/connection-token"
		if strings.HasPrefix(path, "/v1/forums/") {
			contractPath = "/v1/forums/{forum_id}/subscription"
		}
		if strings.HasPrefix(path, "/v1/topics/") {
			contractPath = "/v1/topics/{topic_id}/subscription"
		}
		checkTokenResponse(res, contractPath, 200)
		var token access
		if err := json.Unmarshal(res.data, &token); err != nil {
			t.Fatal(err)
		}
		return token
	}
	connectionToken := getAccess("alice", "/v1/realtime/connection-token")
	boardToken := getAccess("alice", "/v1/forums/"+board.ID+"/subscription")
	checkTokenResponse(call("unknown", "POST", "/v1/realtime/connection-token", "", nil), "/v1/realtime/connection-token", 401)
	checkTokenResponse(call("alice", "POST", "/v1/forums/"+board.ID+"/subscription", "", map[string]string{"channel": "topic:forged", "user": bob.ID}), "/v1/forums/{forum_id}/subscription", 400)
	process := startRealtimeProcess(t, ctx, fixtureURL("ODYSSEIA_TEST_REALTIME_CONSUMER_URL"), connectionKey, proxyKey, server.URL+"/_internal/realtime/subscribe", nil)
	wss, tlsConfig, logPath := process.url, process.tlsConfig, process.logPath
	client := centrifuge.NewJsonClient(wss, centrifuge.Config{Token: connectionToken.Token, TLSConfig: tlsConfig})
	defer client.Close()
	disconnected := make(chan centrifuge.ConnectingEvent, 4)
	transportLost := make(chan centrifuge.ConnectingEvent, 4)
	client.OnConnecting(func(e centrifuge.ConnectingEvent) {
		if e.Code == 1 {
			transportLost <- e
		}
		if e.Code == 4001 && e.Reason == "permissions changed" {
			disconnected <- e
		}
	})
	subscribe := func(channel string) (*centrifuge.Subscription, chan centrifuge.SubscribedEvent, chan centrifuge.PublicationEvent) {
		t.Helper()
		sub, err := client.NewSubscription(channel, centrifuge.SubscriptionConfig{Recoverable: true, Positioned: true})
		if err != nil {
			t.Fatal(err)
		}
		ready := make(chan centrifuge.SubscribedEvent, 8)
		pubs := make(chan centrifuge.PublicationEvent, 32)
		sub.OnSubscribed(func(e centrifuge.SubscribedEvent) { ready <- e })
		sub.OnPublication(func(e centrifuge.PublicationEvent) { pubs <- e })
		if err := sub.Subscribe(); err != nil {
			t.Fatal(err)
		}
		return sub, ready, pubs
	}
	_, boardReady, boardEvents := subscribe(boardToken.Channel)
	if err := client.Connect(); err != nil {
		t.Fatal(err)
	}
	awaitRealtime(t, ctx, boardReady, "forum WSS subscription")
	created := call("alice", "POST", "/v1/forums/"+board.ID+"/topics", uuid.NewString(), forum.TopicDraft{Title: "real outbox", Body: "opening", Mode: "threaded"})
	if created.status != 201 {
		t.Fatalf("create topic status=%d", created.status)
	}
	var topic forum.CreatedTopic
	if err := json.Unmarshal(created.data, &topic); err != nil {
		t.Fatal(err)
	}
	firstEvent := awaitRealtime(t, ctx, boardEvents, "database topic event over WSS")
	topicEvent := decodeForumPublication(t, firstEvent, "forum.topic.created", board.ID)
	var deliveredTopic forum.Topic
	if err := json.Unmarshal(topicEvent.Data, &deliveredTopic); err != nil {
		t.Fatal(err)
	}
	if deliveredTopic != topic.Topic || topicEvent.Position != topic.Topic.Position {
		t.Fatal("topic publication differs from committed result")
	}
	topicToken := getAccess("alice", "/v1/topics/"+topic.Topic.ID+"/subscription")
	_, topicReady, topicEvents := subscribe(topicToken.Channel)
	awaitRealtime(t, ctx, topicReady, "topic WSS subscription")
	post := func(body string) forum.Entry {
		t.Helper()
		res := call("bob", "POST", "/v1/topics/"+topic.Topic.ID+"/messages", uuid.NewString(), forum.ReplyDraft{Body: body})
		if res.status != 201 {
			t.Fatalf("reply status=%d", res.status)
		}
		var entry forum.Entry
		if err := json.Unmarshal(res.data, &entry); err != nil {
			t.Fatal(err)
		}
		return entry
	}
	readMessage := func(pub centrifuge.PublicationEvent) forum.Entry {
		t.Helper()
		event := decodeForumPublication(t, pub, "forum.message.created", topic.Topic.ID)
		var entry forum.Entry
		if err := json.Unmarshal(event.Data, &entry); err != nil {
			t.Fatal(err)
		}
		if entry.StreamID != event.ScopeID || entry.Sequence != event.Position {
			t.Fatal("publication body and scope/position differ")
		}
		return entry
	}
	assertHistoryContains := func(missingIDs map[string]bool) {
		t.Helper()
		loaded := call("alice", "GET", "/v1/topics/"+topic.Topic.ID+"/messages?limit=100", "", nil)
		if loaded.status != 200 {
			t.Fatal("authorized HTTP fallback failed")
		}
		var state struct {
			Items []forum.Entry `json:"items"`
		}
		if err := json.Unmarshal(loaded.data, &state); err != nil {
			t.Fatal(err)
		}
		for _, entry := range state.Items {
			delete(missingIDs, entry.ID)
		}
		if len(missingIDs) != 0 {
			t.Fatal("HTTP fallback lost committed messages")
		}
	}
	entry := post("live message")
	if got := readMessage(awaitRealtime(t, ctx, topicEvents, "live WSS message")); got.ID != entry.ID {
		t.Fatal("WSS message differs from persisted response")
	}
	if _, err := client.Publish(ctx, topicToken.Channel, []byte(`{"body":"bypass persistence"}`)); err == nil {
		t.Fatal("client-side publish bypassed database authority")
	}
	if err := client.Disconnect(); err != nil {
		t.Fatal(err)
	}
	missed := post("message while disconnected")
	waitOutboxEmpty(t, ctx, owner)
	if err := client.Connect(); err != nil {
		t.Fatal(err)
	}
	recovered := awaitRealtime(t, ctx, topicReady, "topic resubscription")
	if !recovered.WasRecovering || !recovered.Recovered {
		t.Fatal("SDK did not recover the missed stream position")
	}
	if got := readMessage(awaitRealtime(t, ctx, topicEvents, "recovered WSS message")); got.ID != missed.ID {
		t.Fatal("recovery lost the committed message")
	}
	awaitRealtime(t, ctx, boardReady, "forum resubscription")
	if err := client.Disconnect(); err != nil {
		t.Fatal(err)
	}
	missingIDs := make(map[string]bool, 3)
	for i := 0; i < 3; i++ {
		missingIDs[post("outside the tiny recovery window").ID] = true
	}
	waitOutboxEmpty(t, ctx, owner)
	if err := client.Connect(); err != nil {
		t.Fatal(err)
	}
	gap := awaitRealtime(t, ctx, topicReady, "resubscription beyond cached history")
	if !gap.WasRecovering || gap.Recovered {
		t.Fatal("cache-window overflow was not reported")
	}
	if gap.StreamPosition == nil || gap.StreamPosition.Epoch == "" {
		t.Fatal("positioned subscription omitted its stream epoch")
	}
	awaitRealtime(t, ctx, boardReady, "forum resubscription after cache overflow")
	assertHistoryContains(missingIDs)

	// Consumer outages retain committed work. Real deletion failures force replay after delivery.
	waitOutboxEmpty(t, ctx, owner)
	if _, err := owner.Exec(ctx, "REVOKE SELECT ON odysseia.realtime_outbox FROM odysseia_realtime"); err != nil {
		t.Fatal(err)
	}
	queued := []forum.Entry{post("queued during consumer outage"), post("also queued")}
	waitConsumerPermissionFailures(t, ctx, logPath, 1)
	if _, err := owner.Exec(ctx, "GRANT SELECT ON odysseia.realtime_outbox TO odysseia_realtime"); err != nil {
		t.Fatal(err)
	}
	for _, want := range queued {
		if got := readMessage(awaitRealtime(t, ctx, topicEvents, "delivery after consumer resumes")); got.ID != want.ID {
			t.Fatal("consumer outage lost or reordered committed work")
		}
	}
	waitOutboxEmpty(t, ctx, owner)
	failures := consumerPermissionFailures(t, logPath)
	if _, err := owner.Exec(ctx, "REVOKE DELETE ON odysseia.realtime_outbox FROM odysseia_realtime"); err != nil {
		t.Fatal(err)
	}
	replayed := post("delivery before failed consumer acknowledgement")
	if got := readMessage(awaitRealtime(t, ctx, topicEvents, "publication before failed delete")); got.ID != replayed.ID {
		t.Fatal("wrong replay fixture event")
	}
	waitConsumerPermissionFailures(t, ctx, logPath, failures+2)
	var retained int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM odysseia.realtime_outbox WHERE payload#>>'{data,data,id}'=$1`, replayed.ID).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("unacknowledged event was lost: %v", err)
	}
	if _, err := owner.Exec(ctx, "GRANT DELETE ON odysseia.realtime_outbox TO odysseia_realtime"); err != nil {
		t.Fatal(err)
	}
	waitOutboxEmpty(t, ctx, owner)
	barrier := post("after replay acknowledgement")
	if got := readMessage(awaitRealtime(t, ctx, topicEvents, "post-replay barrier")); got.ID != barrier.ID {
		t.Fatal("idempotent consumer retry duplicated publication")
	}

	// Killing the real process drops both broker caches. API writes must still persist.
	waitOutboxEmpty(t, ctx, owner)
	if err := process.stop(); err != nil {
		t.Fatal(err)
	}
	awaitRealtime(t, ctx, transportLost, "SDK detects native service crash")
	offlineIDs := make(map[string]bool, 3)
	for range 3 {
		offlineIDs[post("committed while native service is down").ID] = true
	}
	assertForumCount(t, ctx, owner, `SELECT count(*) FROM odysseia.realtime_outbox WHERE payload->>'channel'=$1`, topicToken.Channel, 3)
	process.start(t, ctx)
	restarted := awaitRealtime(t, ctx, topicReady, "automatic resubscription after real process restart")
	if !restarted.WasRecovering || restarted.Recovered {
		t.Fatal("lost memory stream was incorrectly recovered")
	}
	if restarted.StreamPosition == nil || restarted.StreamPosition.Epoch == "" || restarted.StreamPosition.Epoch == gap.StreamPosition.Epoch {
		t.Fatal("native restart did not replace the stream epoch")
	}
	awaitRealtime(t, ctx, boardReady, "forum reconnect after restart")
	assertHistoryContains(offlineIDs)
	waitOutboxEmpty(t, ctx, owner)
	afterRestart := post("new live publication after restart fallback")
	for {
		entry := readMessage(awaitRealtime(t, ctx, topicEvents, "post-restart live boundary"))
		if entry.ID == afterRestart.ID {
			break
		}
	}

	// A delivered but unacknowledged command is replayed after the dedup cache is lost.
	// Delivery precedes acknowledgement. Drain the previous live boundary so
	// the following injected failure deliberately retains only the new command.
	waitOutboxEmpty(t, ctx, owner)
	failures = consumerPermissionFailures(t, logPath)
	if _, err := owner.Exec(ctx, "REVOKE DELETE ON odysseia.realtime_outbox FROM odysseia_realtime"); err != nil {
		t.Fatal(err)
	}
	beforeCrash := post("stable event replay after native crash")
	original := awaitRealtime(t, ctx, topicEvents, "original delivery before crash")
	originalEnvelope := decodeForumPublication(t, original, "forum.message.created", topic.Topic.ID)
	if readMessage(original).ID != beforeCrash.ID {
		t.Fatal("wrong original crash event")
	}
	waitConsumerPermissionFailures(t, ctx, logPath, failures+1)
	if _, err := owner.Exec(ctx, "REVOKE SELECT ON odysseia.realtime_outbox FROM odysseia_realtime"); err != nil {
		t.Fatal(err)
	}
	if err := process.stop(); err != nil {
		t.Fatal(err)
	}
	awaitRealtime(t, ctx, transportLost, "SDK detects second native crash")
	assertForumCount(t, ctx, owner, `SELECT count(*) FROM odysseia.realtime_outbox WHERE payload#>>'{data,id}'=$1`, originalEnvelope.ID, 1)
	process.start(t, ctx)
	secondRestart := awaitRealtime(t, ctx, topicReady, "resubscription before unacknowledged replay")
	if !secondRestart.WasRecovering || secondRestart.Recovered {
		t.Fatal("second restart retained a supposedly lost memory stream")
	}
	if secondRestart.StreamPosition == nil || secondRestart.StreamPosition.Epoch == "" || secondRestart.StreamPosition.Epoch == restarted.StreamPosition.Epoch {
		t.Fatal("second native restart did not replace the stream epoch")
	}
	awaitRealtime(t, ctx, boardReady, "forum reconnect before replay")
	if _, err := owner.Exec(ctx, "GRANT SELECT, DELETE ON odysseia.realtime_outbox TO odysseia_realtime"); err != nil {
		t.Fatal(err)
	}
	repeated := awaitRealtime(t, ctx, topicEvents, "same durable event replayed after dedup cache loss")
	repeatedEnvelope := decodeForumPublication(t, repeated, "forum.message.created", topic.Topic.ID)
	if !bytes.Equal(original.Data, repeated.Data) || repeatedEnvelope.ID != originalEnvelope.ID || readMessage(repeated).ID != beforeCrash.ID {
		t.Fatalf("restart replay mismatch: event %s -> %s, message %s -> %s, same JSON bytes=%v", originalEnvelope.ID, repeatedEnvelope.ID, beforeCrash.ID, readMessage(repeated).ID, bytes.Equal(original.Data, repeated.Data))
	}
	assertForumCount(t, ctx, owner, `SELECT count(*) FROM odysseia.messages WHERE id=$1`, beforeCrash.ID, 1)
	assertHistoryContains(map[string]bool{beforeCrash.ID: true})
	waitOutboxEmpty(t, ctx, owner)
	afterReplay := post("new live publication after restart replay")
	if readMessage(awaitRealtime(t, ctx, topicEvents, "post-replay live boundary")).ID != afterReplay.ID {
		t.Fatal("live stream failed after restart replay")
	}

	private, err := NewForums(owner).Provision(ctx, "not Alice's forum", []string{bob.ID})
	if err != nil {
		t.Fatal(err)
	}
	testSubscriptionDenied(t, ctx, wss, tlsConfig, connectionToken.Token, "forum:"+private.ID, "")
	testSubscriptionDenied(t, ctx, wss, tlsConfig, connectionToken.Token, topicToken.Channel, connectionToken.Token)
	legacy, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": alice.ID, "channel": topicToken.Channel, "exp": time.Now().Add(time.Minute).Unix()}).SignedString([]byte(newRealtimeSecret(t)))
	if err != nil {
		t.Fatal(err)
	}
	testSubscriptionDenied(t, ctx, wss, tlsConfig, connectionToken.Token, topicToken.Channel, legacy)

	bobToken := getAccess("bob", "/v1/realtime/connection-token")
	bobClient := centrifuge.NewJsonClient(wss, centrifuge.Config{Token: bobToken.Token, TLSConfig: tlsConfig})
	defer bobClient.Close()
	bobClient.OnConnecting(func(e centrifuge.ConnectingEvent) {
		if e.Code == 4001 {
			t.Error("revocation disconnected a different user's existing connection")
		}
	})
	bobSubscription, err := bobClient.NewSubscription(topicToken.Channel)
	if err != nil {
		t.Fatal(err)
	}
	bobReady := make(chan centrifuge.SubscribedEvent, 1)
	bobMessages := make(chan centrifuge.PublicationEvent, 4)
	bobSubscription.OnSubscribed(func(e centrifuge.SubscribedEvent) { bobReady <- e })
	bobSubscription.OnPublication(func(e centrifuge.PublicationEvent) { bobMessages <- e })
	if err := bobSubscription.Subscribe(); err != nil {
		t.Fatal(err)
	}
	if err := bobClient.Connect(); err != nil {
		t.Fatal(err)
	}
	awaitRealtime(t, ctx, bobReady, "remaining member subscription")

	// Two existing devices, including an authorized response paused before delivery.
	raw := connectRawRealtime(t, ctx, wss, tlsConfig, connectionToken.Token)
	defer raw.Close()
	if err := raw.WriteJSON(map[string]any{"id": 2, "subscribe": map[string]any{"channel": boardToken.Channel}}); err != nil {
		t.Fatal(err)
	}
	readRawRealtimeReply(t, raw, "subscribe")
	gateArmed.Store(true)
	if err := raw.WriteJSON(map[string]any{"id": 3, "subscribe": map[string]any{"channel": topicToken.Channel}}); err != nil {
		t.Fatal(err)
	}
	awaitRealtime(t, ctx, gateReady, "subscribe authorized before revocation")
	operation := forum.MemberRemoval{OperationID: uuid.NewString(), ForumID: board.ID, UserID: alice.ID, Operator: "realtime-fixture", Reason: "revoke two active devices"}
	started := time.Now()
	if _, err := NewForums(owner).RemoveMember(ctx, operation); err != nil {
		t.Fatal(err)
	}
	waitOutboxEmpty(t, ctx, owner)
	awaitRealtime(t, ctx, disconnected, "active SDK disconnected by durable revocation")
	releaseGate()
	readRawRealtimeDisconnect(t, raw, 4001, "permissions changed")
	t.Logf("local revocation observed on two devices in %s; not a production SLA", time.Since(started))
	checkTokenResponse(call("alice", "POST", "/v1/topics/"+topic.Topic.ID+"/subscription", "", nil), "/v1/topics/{topic_id}/subscription", 404)
	checkTokenResponse(call("alice", "POST", "/v1/forums/"+board.ID+"/subscription", "", nil), "/v1/forums/{forum_id}/subscription", 404)
	if res := call("alice", "GET", "/v1/topics/"+topic.Topic.ID+"/messages", "", nil); res.status != 404 {
		t.Fatal("removed member read persisted history")
	}
	testSubscriptionDenied(t, ctx, wss, tlsConfig, connectionToken.Token, topicToken.Channel, "")
	testSubscriptionDenied(t, ctx, wss, tlsConfig, connectionToken.Token, boardToken.Channel, "")

	// Another user's connection remains usable after the targeted disconnect.
	stillAllowed := post("remaining member receives after revocation")
	if got := readMessage(awaitRealtime(t, ctx, bobMessages, "remaining member live delivery")); got.ID != stillAllowed.ID {
		t.Fatal("revocation affected a different user")
	}
	waitOutboxEmpty(t, ctx, owner)

	// Ignore refresh in a raw connection to prove the server enforces connection expiry.
	shortSigner, err := realtime.NewSigner(connectionKey, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	short, err := shortSigner.Connection(identity.Actor{User: bob, ExpiresAt: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	expiring := connectRawRealtime(t, ctx, wss, tlsConfig, short.Token)
	defer expiring.Close()
	readRawRealtimeDisconnect(t, expiring, 3005, "expir")
}

func awaitRealtime[T any](t *testing.T, ctx context.Context, ch <-chan T, label string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-ctx.Done():
		t.Fatalf("%s: %v", label, ctx.Err())
	case <-time.After(8 * time.Second):
		t.Fatalf("timed out: %s", label)
	}
	var zero T
	return zero
}

func waitOutboxEmpty(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		var pending int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM odysseia.realtime_outbox`).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending == 0 {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("Centrifugo did not consume the transactional outbox")
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func testSubscriptionDenied(t *testing.T, ctx context.Context, address string, tlsConfig *tls.Config, connectionToken, channel, subscriptionToken string) uint32 {
	t.Helper()
	client := centrifuge.NewJsonClient(address, centrifuge.Config{Token: connectionToken, TLSConfig: tlsConfig})
	defer client.Close()
	connected := make(chan centrifuge.ConnectedEvent, 1)
	denied := make(chan uint32, 8)
	client.OnConnected(func(e centrifuge.ConnectedEvent) { connected <- e })
	client.OnDisconnected(func(e centrifuge.DisconnectedEvent) {
		if e.Code != 0 {
			denied <- e.Code
		}
	})
	sub, err := client.NewSubscription(channel, centrifuge.SubscriptionConfig{Token: subscriptionToken})
	if err != nil {
		t.Fatal(err)
	}
	sub.OnUnsubscribed(func(e centrifuge.UnsubscribedEvent) {
		if e.Code != 0 {
			denied <- e.Code
		}
	})
	sub.OnError(func(e centrifuge.SubscriptionErrorEvent) {
		var serverError *centrifuge.Error
		if errors.As(e.Error, &serverError) {
			denied <- serverError.Code
		}
	})
	sub.OnSubscribed(func(centrifuge.SubscribedEvent) { t.Error("unauthorized subscription was accepted") })
	if err := client.Connect(); err != nil {
		t.Fatal(err)
	}
	awaitRealtime(t, ctx, connected, "valid connection before unauthorized subscription")
	if err := sub.Subscribe(); err != nil {
		t.Fatal(err)
	}
	code := awaitRealtime(t, ctx, denied, "unauthorized subscription rejection")
	if code != 103 && code != 109 && code != 3500 {
		t.Fatalf("unexpected subscription rejection code %d", code)
	}
	return code
}
