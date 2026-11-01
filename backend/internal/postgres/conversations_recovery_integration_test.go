//go:build integration && realtime && cluster

package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"testing"
	"time"

	centrifuge "github.com/centrifugal/centrifuge-go"
	"odysseia.local/backend/internal/conversation"
	"odysseia.local/backend/internal/message"
)

func TestConversationInboxRecoveryAndSnapshot(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		missing int
		recover bool
	}{{"cached and retried", 2, true}, {"history window exceeded", 70, false}} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			t.Cleanup(cancel)
			f := newConversationRealtimeFixture(t, ctx)
			nodeA := startRealtimeProcess(t, ctx, f.base.consumerURL, f.base.connectionKey, f.base.proxyKey, f.server.URL+"/_internal/realtime/subscribe", f.base.broker)
			nodeB := startRealtimeProcess(t, ctx, f.base.consumerURL, f.base.connectionKey, f.base.proxyKey, f.server.URL+"/_internal/realtime/subscribe", f.base.broker)
			token := f.connectionToken(t, ctx, "alice")
			channel := conversation.InboxChannel(f.users["alice"].ID)
			aliceA := newClusterPeer(t, nodeA, token, channel, nil)
			aliceB := newClusterPeer(t, nodeB, token, channel, nil)
			bob := newClusterPeer(t, nodeB, f.connectionToken(t, ctx, "bob"), conversation.InboxChannel(f.users["bob"].ID), nil)
			initial := awaitRealtime(t, ctx, aliceA.ready, "Alice initial personal stream")
			awaitRealtime(t, ctx, aliceB.ready, "Alice second device")
			awaitRealtime(t, ctx, bob.ready, "Bob personal stream")
			var dm conversation.Conversation
			f.call(t, ctx, "alice", "POST", "/v1/conversations/dm", conversation.DMDraft{UserID: f.users["bob"].ID}, &dm, 201)
			for _, peer := range []*clusterPeer{aliceA, aliceB, bob} {
				readInboxEvent(t, ctx, peer, eventConversationCreated, dm.ID)
			}
			if initial.StreamPosition == nil || initial.StreamPosition.Epoch == "" {
				t.Fatal("inbox has no recovery epoch")
			}
			post := func(body string) message.Message {
				t.Helper()
				var msg message.Message
				f.call(t, ctx, "bob", "POST", "/v1/conversations/"+dm.ID+"/messages", conversation.MessageDraft{Body: body}, &msg, 201)
				return msg
			}
			waitOutboxEmpty(t, ctx, f.base.owner)
			if scenario.recover {
				// Real native consumers publish, then fail acknowledgement. The
				// retained rows must retry without duplicating either user's inbox.
				failuresA := consumerPermissionFailures(t, nodeA.logPath)
				failuresB := consumerPermissionFailures(t, nodeB.logPath)
				if _, err := f.base.owner.Exec(ctx, "REVOKE DELETE ON odysseia.realtime_outbox FROM odysseia_realtime"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					restore, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					if _, err := f.base.owner.Exec(restore, "GRANT DELETE ON odysseia.realtime_outbox TO odysseia_realtime"); err != nil {
						t.Error("restore owned consumer grant", err)
					}
				})
				retried := post("publication succeeds before acknowledgement")
				for _, peer := range []*clusterPeer{aliceA, aliceB, bob} {
					readInboxMessage(t, ctx, peer, retried)
				}
				waitConsumerPermissionFailures(t, ctx, nodeA.logPath, failuresA+1)
				waitConsumerPermissionFailures(t, ctx, nodeB.logPath, failuresB+1)
				if _, err := f.base.owner.Exec(ctx, "GRANT DELETE ON odysseia.realtime_outbox TO odysseia_realtime"); err != nil {
					t.Fatal(err)
				}
				waitOutboxEmpty(t, ctx, f.base.owner)
			}
			baseline := post("barrier before disconnect")
			_, offset := readInboxMessage(t, ctx, aliceA, baseline)
			readInboxMessage(t, ctx, aliceB, baseline)
			readInboxMessage(t, ctx, bob, baseline)
			saved := centrifuge.StreamPosition{Epoch: initial.StreamPosition.Epoch, Offset: offset}
			if err := nodeA.stop(); err != nil {
				t.Fatal(err)
			}
			awaitRealtime(t, ctx, aliceA.transportLost, "personal stream node stopped")
			missing := make([]message.Message, 0, scenario.missing)
			liveIDs := make([]string, 0, scenario.missing)
			for i := range scenario.missing {
				msg := post(fmt.Sprintf("offline message %d", i))
				live, _ := readInboxMessage(t, ctx, aliceB, msg)
				readInboxMessage(t, ctx, bob, msg)
				missing = append(missing, msg)
				liveIDs = append(liveIDs, live.ID)
			}
			waitOutboxEmpty(t, ctx, f.base.owner)
			var moved *clusterPeer
			if scenario.recover {
				moved = newClusterPeer(t, nodeB, token, channel, &saved)
			} else {
				// GetState is a snapshot provider, not a constant saved cursor:
				// on error 112 the SDK calls it again. Exercise the ordinary SDK
				// reconnect path here so cache loss reports Recovered=false.
				nodeA.start(t, ctx)
				moved = aliceA
			}
			resumed := awaitRealtime(t, ctx, moved.ready, "personal stream node handoff")
			if !resumed.WasRecovering || resumed.Recovered != scenario.recover {
				t.Fatalf("recovering=%v recovered=%v expected=%v", resumed.WasRecovering, resumed.Recovered, scenario.recover)
			}
			if scenario.recover {
				assertClusterRecovery(t, resumed, saved.Epoch)
				for index, want := range missing {
					event, _ := readInboxMessage(t, ctx, moved, want)
					if event.ID != liveIDs[index] {
						t.Fatal("recovery changed the recipient's stable event ID")
					}
				}
				nodeA.start(t, ctx)
				assertClusterRecovery(t, awaitRealtime(t, ctx, aliceA.ready, "personal stream node restarted"), saved.Epoch)
				for index, want := range missing {
					event, _ := readInboxMessage(t, ctx, aliceA, want)
					if event.ID != liveIDs[index] {
						t.Fatal("restart changed the recipient's event ID")
					}
				}
				return
			}

			// Subscribe first, then load authorized HTTPS state while buffering
			// new WSS events. Merge by message ID, never by inbox offset or a
			// single sequence shared between different conversations.
			barrier := post("overlap between HTTPS and WSS")
			readInboxMessage(t, ctx, moved, barrier)
			readInboxMessage(t, ctx, aliceB, barrier)
			readInboxMessage(t, ctx, bob, barrier)
			merged := map[string]message.Message{barrier.ID: barrier}
			var detail conversation.Conversation
			f.call(t, ctx, "alice", "GET", "/v1/conversations/"+dm.ID, nil, &detail, 200)
			if detail.LastPosition != barrier.Sequence || detail.Unread != barrier.Sequence {
				t.Fatal("authoritative conversation state lost unread position")
			}
			path := "/v1/conversations/" + dm.ID + "/messages?limit=17"
			pages, snapshotCount := 0, 0
			var last int64
			for {
				var page struct {
					Items []message.Message `json:"items"`
					Next  string            `json:"next"`
				}
				f.call(t, ctx, "alice", "GET", path, nil, &page, 200)
				pages++
				for _, msg := range page.Items {
					if msg.Sequence <= last {
						t.Fatal("HTTPS pagination repeated or reordered a message")
					}
					last = msg.Sequence
					merged[msg.ID] = msg
					snapshotCount++
				}
				if page.Next == "" {
					break
				}
				path = "/v1/conversations/" + dm.ID + "/messages?limit=17&after=" + url.QueryEscape(page.Next)
			}
			expected := append([]message.Message{baseline}, missing...)
			expected = append(expected, barrier)
			if pages != 5 || snapshotCount != len(expected) || len(merged) != len(expected) {
				t.Fatalf("pages=%d snapshot=%d merged=%d expected=%d", pages, snapshotCount, len(merged), len(expected))
			}
			for _, want := range expected {
				if !sameStoredMessage(merged[want.ID], want) {
					t.Fatal("HTTPS/WSS merge lost committed private content")
				}
			}
			t.Logf("inbox cache gap: %d messages recovered in %d HTTPS pages with live overlap deduplicated", len(merged), pages)
		})
	}
}

func TestConversationInboxRecoversRemovalWithoutFutureContent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	f := newConversationRealtimeFixture(t, ctx)
	nodeA := startRealtimeProcess(t, ctx, f.base.consumerURL, f.base.connectionKey, f.base.proxyKey, f.server.URL+"/_internal/realtime/subscribe", f.base.broker)
	nodeB := startRealtimeProcess(t, ctx, f.base.consumerURL, f.base.connectionKey, f.base.proxyKey, f.server.URL+"/_internal/realtime/subscribe", f.base.broker)
	token := f.connectionToken(t, ctx, "alice")
	channel := conversation.InboxChannel(f.users["alice"].ID)
	alice := newClusterPeer(t, nodeA, token, channel, nil)
	bob := newClusterPeer(t, nodeB, f.connectionToken(t, ctx, "bob"), conversation.InboxChannel(f.users["bob"].ID), nil)
	initial := awaitRealtime(t, ctx, alice.ready, "member before disconnect")
	awaitRealtime(t, ctx, bob.ready, "group owner")
	var group conversation.Conversation
	f.call(t, ctx, "bob", "POST", "/v1/conversations/groups", conversation.GroupDraft{Title: "offline removal", MemberIDs: []string{f.users["alice"].ID}}, &group, 201)
	_, offset := readInboxEvent(t, ctx, alice, eventConversationCreated, group.ID)
	readInboxEvent(t, ctx, bob, eventConversationCreated, group.ID)
	if initial.StreamPosition == nil {
		t.Fatal("missing personal stream position")
	}
	saved := centrifuge.StreamPosition{Epoch: initial.StreamPosition.Epoch, Offset: offset}
	if err := nodeA.stop(); err != nil {
		t.Fatal(err)
	}
	awaitRealtime(t, ctx, alice.transportLost, "member offline")
	f.call(t, ctx, "bob", "DELETE", "/v1/conversations/"+group.ID+"/members/"+f.users["alice"].ID, nil, nil, 200)
	readInboxEvent(t, ctx, bob, eventConversationMemberRemoved, group.ID)
	var private message.Message
	f.call(t, ctx, "bob", "POST", "/v1/conversations/"+group.ID+"/messages", conversation.MessageDraft{Body: "after removal"}, &private, 201)
	readInboxMessage(t, ctx, bob, private)
	waitOutboxEmpty(t, ctx, f.base.owner)
	moved := newClusterPeer(t, nodeB, token, channel, &saved)
	assertClusterRecovery(t, awaitRealtime(t, ctx, moved.ready, "offline removal recovered"), saved.Epoch)
	readInboxEvent(t, ctx, moved, eventConversationMemberRemoved, group.ID)
	f.call(t, ctx, "alice", "GET", "/v1/conversations/"+group.ID+"/messages", nil, nil, 404)
	f.call(t, ctx, "alice", "POST", "/v1/conversations/"+group.ID+"/messages", conversation.MessageDraft{Body: "revoked"}, nil, 404)
	for _, event := range inboxHistory(t, ctx, moved) {
		var msg message.Message
		if err := json.Unmarshal(event.Data, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.ID == private.ID {
			t.Fatal("post-removal message entered the recovered private inbox")
		}
	}
}
