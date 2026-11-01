//go:build integration && realtime && cluster

package postgres

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"odysseia.local/backend/internal/conversation"
	"odysseia.local/backend/internal/emoji"
	"odysseia.local/backend/internal/message"
)

func TestConversationInboxLifecycleAcrossNodes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	f := newConversationRealtimeFixture(t, ctx)
	nodeA := startRealtimeProcess(t, ctx, f.base.consumerURL, f.base.connectionKey, f.base.proxyKey, f.server.URL+"/_internal/realtime/subscribe", f.base.broker)
	nodeB := startRealtimeProcess(t, ctx, f.base.consumerURL, f.base.connectionKey, f.base.proxyKey, f.server.URL+"/_internal/realtime/subscribe", f.base.broker)
	aliceToken := f.connectionToken(t, ctx, "alice")
	aliceInbox := conversation.InboxChannel(f.users["alice"].ID)
	aliceA := newClusterPeer(t, nodeA, aliceToken, aliceInbox, nil)
	aliceB := newClusterPeer(t, nodeB, aliceToken, aliceInbox, nil)
	bob := newClusterPeer(t, nodeB, f.connectionToken(t, ctx, "bob"), conversation.InboxChannel(f.users["bob"].ID), nil)
	cara := newClusterPeer(t, nodeA, f.connectionToken(t, ctx, "cara"), conversation.InboxChannel(f.users["cara"].ID), nil)
	outsiderToken := f.connectionToken(t, ctx, "outsider")
	outsider := newClusterPeer(t, nodeB, outsiderToken, conversation.InboxChannel(f.users["outsider"].ID), nil)
	for _, peer := range []*clusterPeer{aliceA, aliceB, bob, cara, outsider} {
		awaitRealtime(t, ctx, peer.ready, "personal inbox subscription")
	}
	if code := testSubscriptionDenied(t, ctx, nodeB.url, nodeB.tlsConfig, outsiderToken, aliceInbox, ""); code != 103 {
		t.Fatalf("another user's inbox subscription was not denied: %d", code)
	}
	// History and publishing have independent permissions; a signed connection
	// must not bypass the database by reading another inbox or forging events.
	connection := connectRawRealtime(t, ctx, nodeB.url, nodeB.tlsConfig, outsiderToken)
	for index, command := range []map[string]any{
		{"history": map[string]any{"channel": aliceInbox, "limit": -1}},
		{"publish": map[string]any{"channel": conversation.InboxChannel(f.users["outsider"].ID), "data": map[string]string{"type": "forged"}}},
	} {
		command["id"] = index + 2
		if err := connection.WriteJSON(command); err != nil {
			t.Fatal(err)
		}
		var reply struct {
			ID    int `json:"id"`
			Error struct {
				Code uint32 `json:"code"`
			} `json:"error"`
		}
		if err := connection.ReadJSON(&reply); err != nil {
			t.Fatal(err)
		}
		if reply.ID != index+2 || reply.Error.Code != 103 {
			t.Fatalf("private history or client publishing was not denied: code=%d", reply.Error.Code)
		}
	}

	var dm conversation.Conversation
	f.call(t, ctx, "alice", "POST", "/v1/conversations/dm", conversation.DMDraft{UserID: f.users["bob"].ID}, &dm, 201)
	for _, peer := range []*clusterPeer{aliceA, aliceB, bob} {
		readInboxEvent(t, ctx, peer, eventConversationCreated, dm.ID)
	}
	send := func(actor, id string, draft conversation.MessageDraft, peers ...*clusterPeer) message.Message {
		t.Helper()
		var msg message.Message
		f.call(t, ctx, actor, "POST", "/v1/conversations/"+id+"/messages", draft, &msg, 201)
		for _, peer := range peers {
			readInboxMessage(t, ctx, peer, msg)
		}
		return msg
	}
	first := send("bob", dm.ID, conversation.MessageDraft{Body: "private across two nodes"}, aliceA, aliceB, bob)
	var unread conversation.UnreadSummary
	f.call(t, ctx, "alice", "GET", "/v1/me/unread", nil, &unread, 200)
	if unread.Total != 1 {
		t.Fatalf("recipient unread=%d", unread.Total)
	}
	var read conversation.ReadState
	f.call(t, ctx, "alice", "PUT", "/v1/conversations/"+dm.ID+"/read", map[string]string{"position": "1"}, &read, 200)
	for _, peer := range []*clusterPeer{aliceA, aliceB} {
		readInboxEvent(t, ctx, peer, eventConversationRead, dm.ID)
	}
	send("bob", dm.ID, conversation.MessageDraft{Body: "quote stays in this DM", QuoteID: first.ID}, aliceA, aliceB, bob)
	f.call(t, ctx, "outsider", "GET", "/v1/conversations/"+dm.ID, nil, nil, 404)

	var group conversation.Conversation
	f.call(t, ctx, "alice", "POST", "/v1/conversations/groups", conversation.GroupDraft{Title: "group lifecycle", MemberIDs: []string{f.users["bob"].ID}}, &group, 201)
	for _, peer := range []*clusterPeer{aliceA, aliceB, bob} {
		readInboxEvent(t, ctx, peer, eventConversationCreated, group.ID)
	}
	beforeJoin := send("alice", group.ID, conversation.MessageDraft{Body: "history before joining"}, aliceA, aliceB, bob)
	f.call(t, ctx, "alice", "PUT", "/v1/conversations/"+group.ID+"/members/"+f.users["cara"].ID, nil, nil, 200)
	for _, peer := range []*clusterPeer{aliceA, aliceB, bob, cara} {
		readInboxEvent(t, ctx, peer, eventConversationMemberAdded, group.ID)
	}
	var history struct {
		Items []message.Message `json:"items"`
	}
	f.call(t, ctx, "cara", "GET", "/v1/conversations/"+group.ID+"/messages", nil, &history, 200)
	if len(history.Items) != 1 || history.Items[0].ID != beforeJoin.ID {
		t.Fatal("new group member did not receive the agreed full history")
	}
	send("cara", group.ID, conversation.MessageDraft{Body: "new member can send"}, aliceA, aliceB, bob, cara)

	var image, voice message.Attachment
	uploadPath := "/v1/conversations/" + group.ID + "/attachments"
	f.upload(t, ctx, "alice", uploadPath, mustIntegrationJPEG(t), nil, &image)
	voiceBytes, err := os.ReadFile("../media/testdata/location-opus.webm")
	if err != nil {
		t.Fatal(err)
	}
	f.upload(t, ctx, "alice", uploadPath, voiceBytes, map[string]string{"duration_ms": "1000"}, &voice)
	attached := send("alice", group.ID, conversation.MessageDraft{AttachmentIDs: []string{image.ID, voice.ID}}, aliceA, aliceB, bob, cara)
	if attached.Body != "" || len(attached.Attachments) != 2 || f.blobs.Len() != 2 {
		t.Fatal("empty-text image/voice message did not retain its attachments")
	}
	checkP4AttachmentRedirect(t, ctx, f, "bob", group.ID, image.ID)
	f.call(t, ctx, "outsider", "GET", "/v1/conversations/"+group.ID+"/attachments/"+image.ID, nil, nil, 404)

	var custom emoji.Emoji
	f.upload(t, ctx, "alice", "/v1/emojis", mustIntegrationPNG(t), map[string]string{"name": "p4_wave"}, &custom)
	withEmoji := send("alice", group.ID, conversation.MessageDraft{Body: "<:p4_wave:" + custom.ID + ">"}, aliceA, aliceB, bob, cara)
	if len(withEmoji.Emojis) != 1 || withEmoji.Emojis[0].ID != custom.ID {
		t.Fatal("custom emoji metadata was absent from the WSS message")
	}
	if _, err := f.base.owner.Exec(ctx, `UPDATE odysseia.attachments SET created_at=statement_timestamp()-interval '8 days'
		WHERE id=ANY($1::uuid[])`, []string{image.ID, voice.ID}); err != nil {
		t.Fatal(err)
	}
	swept, err := SweepAttachments(ctx, f.base.owner, f.blobs, 168*time.Hour)
	if err != nil || swept.ExpiredRows != 2 || f.blobs.Len() != 0 {
		t.Fatalf("attached object cleanup: %+v error=%v", swept, err)
	}
	f.call(t, ctx, "bob", "GET", "/v1/conversations/"+group.ID+"/attachments/"+image.ID, nil, nil, 404)
	f.call(t, ctx, "cara", "GET", "/v1/conversations/"+group.ID+"/messages", nil, &history, 200)
	if len(history.Items) != 4 || !history.Items[2].Attachments[0].Expired || history.Items[2].Body != "" {
		t.Fatal("cleanup changed the message or lost expired attachment history")
	}

	f.call(t, ctx, "alice", "DELETE", "/v1/conversations/"+group.ID+"/members/"+f.users["bob"].ID, nil, nil, 200)
	for _, peer := range []*clusterPeer{aliceA, aliceB, bob, cara} {
		readInboxEvent(t, ctx, peer, eventConversationMemberRemoved, group.ID)
	}
	f.call(t, ctx, "bob", "GET", "/v1/conversations/"+group.ID+"/messages", nil, nil, 404)
	f.call(t, ctx, "bob", "POST", "/v1/conversations/"+group.ID+"/messages", conversation.MessageDraft{Body: "revoked"}, nil, 404)
	afterRemoval := send("alice", group.ID, conversation.MessageDraft{Body: "remaining members only"}, aliceA, aliceB, cara)
	f.call(t, ctx, "alice", "PUT", "/v1/conversations/"+group.ID+"/owner", map[string]string{"user_id": f.users["cara"].ID}, nil, 200)
	for _, peer := range []*clusterPeer{aliceA, aliceB, cara} {
		readInboxEvent(t, ctx, peer, eventConversationOwnerChanged, group.ID)
	}
	f.call(t, ctx, "alice", "DELETE", "/v1/conversations/"+group.ID+"/members/"+f.users["alice"].ID, nil, nil, 200)
	for _, peer := range []*clusterPeer{aliceA, aliceB, cara} {
		readInboxEvent(t, ctx, peer, eventConversationMemberRemoved, group.ID)
	}
	f.call(t, ctx, "cara", "DELETE", "/v1/conversations/"+group.ID, nil, nil, 200)
	readInboxEvent(t, ctx, cara, eventConversationDissolved, group.ID)
	f.call(t, ctx, "cara", "GET", "/v1/conversations/"+group.ID, nil, nil, 404)
	send("bob", dm.ID, conversation.MessageDraft{Body: "other conversation remains available"}, aliceA, aliceB, bob)
	waitOutboxEmpty(t, ctx, f.base.owner)
	for _, event := range inboxHistory(t, ctx, bob) {
		if event.Type == eventConversationRead {
			t.Fatal("another member's private read state reached Bob's inbox")
		}
		var msg message.Message
		if event.Type == eventConversationMessage {
			if err := json.Unmarshal(event.Data, &msg); err != nil {
				t.Fatal(err)
			}
		}
		if msg.ID == afterRemoval.ID {
			t.Fatal("post-removal message leaked into the removed member's history")
		}
	}
	if len(inboxHistory(t, ctx, outsider)) != 0 {
		t.Fatal("outsider received private events")
	}
	t.Log("P4 HTTPS/JWT/WSS: all seven inbox event types, two devices, image/voice/emoji, cleanup and member isolation verified")
}

func checkP4AttachmentRedirect(t *testing.T, ctx context.Context, f *conversationRealtimeFixture, actor, conversationID, attachmentID string) {
	t.Helper()
	client := *f.server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	request, err := http.NewRequestWithContext(ctx, "GET", f.server.URL+"/v1/conversations/"+conversationID+"/attachments/"+attachmentID, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+f.tokens[actor])
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound || response.Header.Get("Location") == "" || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("authorized attachment redirect status=%d", response.StatusCode)
	}
}
