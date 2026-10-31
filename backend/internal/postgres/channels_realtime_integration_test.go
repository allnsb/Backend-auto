//go:build integration && realtime && cluster

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	centrifuge "github.com/centrifugal/centrifuge-go"
	"github.com/google/uuid"
	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/message"
)

// Reuse the actual two-node engine and SDK fixture for all product shapes.
// Native PUB/SUB channels alone do not establish product membership or history.
func TestDiscussionModesAndFixedChannelRealtime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	f := newClusterFixture(t, ctx)
	chat, err := f.store.CreateTopic(ctx, f.bob.ID, f.board.ID, uuid.NewString(), forum.TopicDraft{Title: "chat topic", Body: "same canonical message store", Mode: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := f.store.CreateChannel(ctx, f.bob.ID, f.board.ID, uuid.NewString(), forum.ChannelDraft{Title: "persistent channel"})
	if err != nil {
		t.Fatal(err)
	}
	nodeA := startRealtimeProcess(t, ctx, f.consumerURL, f.connectionKey, f.proxyKey, f.server.URL+"/_internal/realtime/subscribe", f.broker)
	nodeB := startRealtimeProcess(t, ctx, f.consumerURL, f.connectionKey, f.proxyKey, f.server.URL+"/_internal/realtime/subscribe", f.broker)
	aliceToken := f.connectionToken(t, f.alice)
	threaded := newClusterPeer(t, nodeA, aliceToken, "topic:"+f.topic.Topic.ID, nil)
	instant := newClusterPeer(t, nodeB, aliceToken, "topic:"+chat.Topic.ID, nil)
	channelA := newClusterPeer(t, nodeA, aliceToken, "channel:"+channel.ID, nil)
	channelB := newClusterPeer(t, nodeB, aliceToken, "channel:"+channel.ID, nil)
	bob := newClusterPeer(t, nodeB, f.connectionToken(t, f.bob), "channel:"+channel.ID, nil)
	awaitRealtime(t, ctx, threaded.ready, "threaded subscription")
	awaitRealtime(t, ctx, instant.ready, "chat-layout subscription")
	initial := awaitRealtime(t, ctx, channelA.ready, "channel on node A")
	awaitRealtime(t, ctx, channelB.ready, "channel on node B")
	awaitRealtime(t, ctx, bob.ready, "remaining channel member")
	for _, target := range []struct {
		id   string
		peer *clusterPeer
	}{{f.topic.Topic.ID, threaded}, {chat.Topic.ID, instant}} {
		entry, err := f.store.Reply(ctx, f.bob.ID, target.id, uuid.NewString(), forum.ReplyDraft{Body: "both topic layouts publish"})
		if err != nil {
			t.Fatal(err)
		}
		readClusterMessages(t, ctx, target.peer, []forum.Entry{entry})
	}
	first, err := f.store.SendChannelMessage(ctx, f.bob.ID, channel.ID, uuid.NewString(), forum.ChannelMessageDraft{Body: "channel first"})
	if err != nil {
		t.Fatal(err)
	}
	firstA := readChannelPublication(t, ctx, channelA, first)
	firstB := readChannelPublication(t, ctx, channelB, first)
	readChannelPublication(t, ctx, bob, first)
	if firstA.ID != firstB.ID {
		t.Fatal("channel event identity changed across nodes")
	}
	second, err := f.store.SendChannelMessage(ctx, f.bob.ID, channel.ID, uuid.NewString(), forum.ChannelMessageDraft{Body: "channel quote", QuoteID: first.ID})
	if err != nil {
		t.Fatal(err)
	}
	secondA := readChannelPublication(t, ctx, channelA, second)
	secondB := readChannelPublication(t, ctx, channelB, second)
	readChannelPublication(t, ctx, bob, second)
	if initial.StreamPosition == nil {
		t.Fatal("channel stream has no recovery position")
	}
	recovered := newClusterPeer(t, nodeB, aliceToken, "channel:"+channel.ID, &centrifuge.StreamPosition{Offset: firstA.Offset, Epoch: initial.StreamPosition.Epoch})
	assertClusterRecovery(t, awaitRealtime(t, ctx, recovered.ready, "channel recovery on another node"), initial.StreamPosition.Epoch)
	secondC := readChannelPublication(t, ctx, recovered, second)
	if secondA.ID != secondB.ID || secondA.ID != secondC.ID {
		t.Fatal("channel recovery changed event ID")
	}
	_, err = NewForums(f.owner).RemoveMember(ctx, forum.MemberRemoval{OperationID: uuid.NewString(), ForumID: f.board.ID, UserID: f.alice.ID, Operator: "P3 realtime fixture", Reason: "one inherited authorization boundary across all discussion forms"})
	if err != nil {
		t.Fatal(err)
	}
	for _, peer := range []*clusterPeer{threaded, instant, channelA, channelB, recovered} {
		awaitRealtime(t, ctx, peer.revoked, "inherited member revoked across nodes")
	}
	if _, err := f.store.ListChannelMessages(ctx, f.alice.ID, channel.ID, 0, 10); !errors.Is(err, forum.ErrNotFound) {
		t.Fatalf("revoked channel history still accessible: %v", err)
	}
	third, err := f.store.SendChannelMessage(ctx, f.bob.ID, channel.ID, uuid.NewString(), forum.ChannelMessageDraft{Body: "remaining member still receives"})
	if err != nil {
		t.Fatal(err)
	}
	readChannelPublication(t, ctx, bob, third)
	waitOutboxEmpty(t, ctx, f.owner)
	for _, peer := range []*clusterPeer{threaded, instant, channelA, channelB, recovered} {
		if len(peer.messages) != 0 {
			t.Fatal("publication leaked after inherited revocation")
		}
	}
}

type channelPublication struct {
	ID     string
	Offset uint64
}

func readChannelPublication(t *testing.T, ctx context.Context, peer *clusterPeer, want message.Message) channelPublication {
	t.Helper()
	publication := awaitRealtime(t, ctx, peer.messages, "canonical channel message")
	event := decodeForumPublication(t, publication, "channel.message.created", want.StreamID)
	var got message.Message
	if err := json.Unmarshal(event.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got != want || event.Position != want.Sequence || publication.Offset == 0 {
		t.Fatal("channel WSS content differs from its committed message")
	}
	return channelPublication{ID: event.ID, Offset: publication.Offset}
}
