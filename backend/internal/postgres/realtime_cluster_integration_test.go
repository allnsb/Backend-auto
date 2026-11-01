//go:build integration && realtime && cluster

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	centrifuge "github.com/centrifugal/centrifuge-go"
	"github.com/google/uuid"
	"odysseia.local/backend/internal/forum"
)

func TestRealtimeSharedBrokerAcrossTwoNodes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	t.Cleanup(cancel)
	fixture := newClusterFixture(t, ctx)
	owner, store := fixture.owner, fixture.store
	alice, bob, board, topic := fixture.alice, fixture.bob, fixture.board, fixture.topic
	connectionKey, proxyKey := fixture.connectionKey, fixture.proxyKey
	server, broker, consumerURL := fixture.server, fixture.broker, fixture.consumerURL
	aliceToken, bobToken := fixture.connectionToken(t, alice), fixture.connectionToken(t, bob)
	proxyURL := server.URL + "/_internal/realtime/subscribe"
	nodeA := startRealtimeProcess(t, ctx, consumerURL, connectionKey, proxyKey, proxyURL, broker)
	nodeB := startRealtimeProcess(t, ctx, consumerURL, connectionKey, proxyKey, proxyURL, broker)
	waitOutboxEmpty(t, ctx, owner)
	channel := "topic:" + topic.Topic.ID
	aliceA := newClusterPeer(t, nodeA, aliceToken, channel, nil)
	initialA := awaitRealtime(t, ctx, aliceA.ready, "Alice subscribed to node A")
	aliceB := newClusterPeer(t, nodeB, aliceToken, channel, nil)
	awaitRealtime(t, ctx, aliceB.ready, "Alice subscribed to node B")
	bobB := newClusterPeer(t, nodeB, bobToken, channel, nil)
	awaitRealtime(t, ctx, bobB.ready, "Bob subscribed to node B")
	post := func(body string) forum.Entry {
		t.Helper()
		entry, err := store.Reply(ctx, bob.ID, topic.Topic.ID, uuid.NewString(), forum.ReplyDraft{Body: body})
		if err != nil {
			t.Fatal(err)
		}
		return entry
	}
	var ordered []forum.Entry
	for range 6 {
		ordered = append(ordered, post("ordered cross-node publication"))
	}
	fromA := readClusterMessages(t, ctx, aliceA, ordered)
	fromB := readClusterMessages(t, ctx, aliceB, ordered)
	fromBob := readClusterMessages(t, ctx, bobB, ordered)
	if !slices.Equal(fromA.ids, fromB.ids) || !slices.Equal(fromA.ids, fromBob.ids) {
		t.Fatal("nodes did not deliver the same ordered event IDs")
	}

	// Both consumers must retry the same retained row; shared deduplication must
	// prevent those real dispatch attempts from duplicating live publications.
	waitOutboxEmpty(t, ctx, owner)
	failuresA, failuresB := consumerPermissionFailures(t, nodeA.logPath), consumerPermissionFailures(t, nodeB.logPath)
	if _, err := owner.Exec(ctx, "REVOKE DELETE ON odysseia.realtime_outbox FROM odysseia_realtime"); err != nil {
		t.Fatal(err)
	}
	replayed := []forum.Entry{post("publish succeeds, acknowledgement fails")}
	readClusterMessages(t, ctx, aliceA, replayed)
	readClusterMessages(t, ctx, aliceB, replayed)
	readClusterMessages(t, ctx, bobB, replayed)
	waitConsumerPermissionFailures(t, ctx, nodeA.logPath, failuresA+1)
	waitConsumerPermissionFailures(t, ctx, nodeB.logPath, failuresB+1)
	assertForumCount(t, ctx, owner, `SELECT count(*) FROM odysseia.realtime_outbox WHERE payload#>>'{data,data,id}'=$1`, replayed[0].ID, 1)
	if _, err := owner.Exec(ctx, "GRANT DELETE ON odysseia.realtime_outbox TO odysseia_realtime"); err != nil {
		t.Fatal(err)
	}
	waitOutboxEmpty(t, ctx, owner)
	barrier := []forum.Entry{post("cross-node retry barrier")}
	beforeStop := readClusterMessages(t, ctx, aliceA, barrier)
	readClusterMessages(t, ctx, aliceB, barrier)
	readClusterMessages(t, ctx, bobB, barrier)
	if initialA.StreamPosition == nil || initialA.StreamPosition.Epoch == "" {
		t.Fatal("shared stream has no initial epoch")
	}
	savedPosition := centrifuge.StreamPosition{Epoch: initialA.StreamPosition.Epoch, Offset: beforeStop.lastOffset}

	if err := nodeA.stop(); err != nil {
		t.Fatal(err)
	}
	awaitRealtime(t, ctx, aliceA.transportLost, "node A crash detected")
	missing := []forum.Entry{post("surviving node consumes first"), post("surviving node consumes second")}
	fromSurvivor := readClusterMessages(t, ctx, aliceB, missing)
	readClusterMessages(t, ctx, bobB, missing)
	// Supply the last applied position from A to a new connection on B. This
	// exercises explicit node handoff, without assuming a load balancer exists.
	moved := newClusterPeer(t, nodeB, aliceToken, channel, &savedPosition)
	assertClusterRecovery(t, awaitRealtime(t, ctx, moved.ready, "recovery on another node"), savedPosition.Epoch)
	fromMoved := readClusterMessages(t, ctx, moved, missing)
	if !slices.Equal(fromSurvivor.ids, fromMoved.ids) {
		t.Fatal("node handoff changed recovered event IDs")
	}
	nodeA.start(t, ctx)
	assertClusterRecovery(t, awaitRealtime(t, ctx, aliceA.ready, "same-address recovery after node restart"), savedPosition.Epoch)
	fromRestart := readClusterMessages(t, ctx, aliceA, missing)
	if !slices.Equal(fromSurvivor.ids, fromRestart.ids) {
		t.Fatal("node restart changed recovered event IDs")
	}

	operation := forum.MemberRemoval{OperationID: uuid.NewString(), ForumID: board.ID, UserID: alice.ID, Operator: "cluster-fixture", Reason: "revoke all devices across nodes"}
	started := time.Now()
	revocationCtx, stopRevocation := context.WithTimeout(ctx, 3*time.Second)
	defer stopRevocation()
	if _, err := NewForums(owner).RemoveMember(ctx, operation); err != nil {
		t.Fatal(err)
	}
	awaitRealtime(t, revocationCtx, aliceA.revoked, "node A active revocation")
	awaitRealtime(t, revocationCtx, aliceB.revoked, "node B active revocation")
	awaitRealtime(t, revocationCtx, moved.revoked, "moved device active revocation")
	t.Logf("cross-node revocation observed on three devices in %s; not a production SLA", time.Since(started))
	if code := testSubscriptionDenied(t, ctx, nodeA.url, nodeA.tlsConfig, aliceToken, channel, ""); code != 103 {
		t.Fatalf("node A did not check current permission: %d", code)
	}
	if code := testSubscriptionDenied(t, ctx, nodeB.url, nodeB.tlsConfig, aliceToken, channel, ""); code != 103 {
		t.Fatalf("node B did not check current permission: %d", code)
	}
	if _, err := store.ListMessages(ctx, alice.ID, topic.Topic.ID, 0, 100); !errors.Is(err, forum.ErrNotFound) {
		t.Fatalf("revoked member retained history access: %v", err)
	}
	readClusterMessages(t, ctx, bobB, []forum.Entry{post("remaining member after cross-node revocation")})
	waitOutboxEmpty(t, ctx, owner)
	if len(aliceA.messages)+len(aliceB.messages)+len(moved.messages) != 0 || len(bobB.revoked) != 0 || len(bobB.transportLost) != 0 {
		t.Fatal("targeted cross-node revocation leaked a publication or affected Bob")
	}
}

type clusterPeer struct {
	ready                  chan centrifuge.SubscribedEvent
	messages               chan centrifuge.PublicationEvent
	transportLost, revoked chan centrifuge.ConnectingEvent
	subscription           *centrifuge.Subscription
}

func newClusterPeer(t *testing.T, node *realtimeProcess, token, channel string, since *centrifuge.StreamPosition) *clusterPeer {
	t.Helper()
	p := &clusterPeer{ready: make(chan centrifuge.SubscribedEvent, 8), messages: make(chan centrifuge.PublicationEvent, 32), transportLost: make(chan centrifuge.ConnectingEvent, 8), revoked: make(chan centrifuge.ConnectingEvent, 8)}
	client := centrifuge.NewJsonClient(node.url, centrifuge.Config{Token: token, TLSConfig: node.tlsConfig})
	t.Cleanup(func() { client.Close() })
	client.OnConnecting(func(e centrifuge.ConnectingEvent) {
		if e.Code == 1 {
			p.transportLost <- e
		}
		if e.Code == 4001 && e.Reason == "permissions changed" {
			p.revoked <- e
		}
	})
	config := centrifuge.SubscriptionConfig{Recoverable: true, Positioned: true}
	if since != nil {
		// Seed a known-valid cursor for cross-node recovery. This fixture's
		// constant callback is not a production GetState implementation: an
		// expired cursor needs a fresh application snapshot, not endless reuse.
		position := *since
		config.GetState = func(centrifuge.SubscriptionGetStateEvent) (centrifuge.StreamPosition, error) { return position, nil }
	}
	sub, err := client.NewSubscription(channel, config)
	if err != nil {
		t.Fatal(err)
	}
	p.subscription = sub
	sub.OnSubscribed(func(e centrifuge.SubscribedEvent) { p.ready <- e })
	sub.OnPublication(func(e centrifuge.PublicationEvent) { p.messages <- e })
	if err := sub.Subscribe(); err != nil {
		t.Fatal(err)
	}
	if err := client.Connect(); err != nil {
		t.Fatal(err)
	}
	return p
}

type clusterDelivery struct {
	ids        []string
	lastOffset uint64
}

func readClusterMessages(t *testing.T, ctx context.Context, peer *clusterPeer, expected []forum.Entry) clusterDelivery {
	t.Helper()
	var delivered clusterDelivery
	for _, want := range expected {
		publication := awaitRealtime(t, ctx, peer.messages, "ordered cluster message")
		event := decodeForumPublication(t, publication, "forum.message.created", want.StreamID)
		var got forum.Entry
		if err := json.Unmarshal(event.Data, &got); err != nil {
			t.Fatal(err)
		}
		if !sameStoredEntry(got, want) || event.Position != want.Sequence || publication.Offset == 0 || publication.Offset <= delivered.lastOffset {
			t.Fatal("cluster delivery lost, duplicated or reordered committed content")
		}
		delivered.ids = append(delivered.ids, event.ID)
		delivered.lastOffset = publication.Offset
	}
	return delivered
}

func assertClusterRecovery(t *testing.T, event centrifuge.SubscribedEvent, epoch string) {
	t.Helper()
	if !event.WasRecovering || !event.Recovered || event.StreamPosition == nil || event.StreamPosition.Epoch != epoch {
		t.Fatal("shared broker did not recover the original stream epoch")
	}
}
