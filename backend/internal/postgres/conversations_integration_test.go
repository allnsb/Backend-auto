//go:build integration

package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"odysseia.local/backend/internal/conversation"
	"odysseia.local/backend/internal/identity"
)

func testConversations(t *testing.T, ctx context.Context, migrationURL string, runtimePool *pgxpool.Pool) {
	t.Helper()
	ownerPool, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerPool.Close()
	_, err = ownerPool.Exec(ctx, `GRANT USAGE ON SCHEMA odysseia TO odysseia_runtime;
		GRANT SELECT ON odysseia.users, odysseia.forum_members, odysseia.message_streams, odysseia.messages TO odysseia_runtime;
		GRANT SELECT, INSERT ON odysseia.conversations, odysseia.conversation_members TO odysseia_runtime;
		GRANT INSERT ON odysseia.message_streams, odysseia.messages TO odysseia_runtime;
		GRANT UPDATE (owner_id, dissolved_at) ON odysseia.conversations TO odysseia_runtime;
		GRANT UPDATE (role, joined_position, last_read_position, joined_at, removed_at) ON odysseia.conversation_members TO odysseia_runtime;
		GRANT UPDATE (last_position) ON odysseia.message_streams TO odysseia_runtime;
		GRANT INSERT (method, payload) ON odysseia.realtime_outbox TO odysseia_runtime;
		GRANT USAGE ON SEQUENCE odysseia.realtime_outbox_id_seq TO odysseia_runtime;`)
	if err != nil {
		t.Fatal(err)
	}
	users := NewUsers(ownerPool)
	alice, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://p4.example", Subject: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://p4.example", Subject: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	cara, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://p4.example", Subject: "cara"})
	if err != nil {
		t.Fatal(err)
	}
	dave, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://p4.example", Subject: "dave"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewForums(ownerPool).Provision(ctx, "shared forum", []string{alice.ID, bob.ID, dave.ID}); err != nil {
		t.Fatal(err)
	}
	store, err := NewCachedForums(runtimePool, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, _, err := store.OpenDM(ctx, alice.ID, uuid.NewString(), conversation.DMDraft{UserID: cara.ID}); !errors.Is(err, conversation.ErrNotFound) {
		t.Fatalf("dm without a shared forum: %v", err)
	}
	assertConversationCount(t, ctx, ownerPool, 0, `SELECT count(*) FROM odysseia.conversations`)

	draft := conversation.DMDraft{UserID: bob.ID}
	type openResult struct {
		id    string
		owned bool
		err   error
	}
	opened := make(chan openResult, 8)
	for range 8 {
		go func() {
			value, owned, err := store.OpenDM(ctx, alice.ID, uuid.NewString(), draft)
			opened <- openResult{value.ID, owned, err}
		}()
	}
	var dmID string
	var owners int
	for range 8 {
		got := <-opened
		if got.err != nil {
			t.Fatal(got.err)
		}
		if dmID == "" {
			dmID = got.id
		}
		if got.id != dmID {
			t.Fatal("concurrent direct conversations diverged")
		}
		if got.owned {
			owners++
		}
	}
	if owners != 1 {
		t.Fatalf("idempotency owners=%d", owners)
	}
	assertConversationCount(t, ctx, ownerPool, 1, `SELECT count(*) FROM odysseia.conversations WHERE kind='dm'`)
	assertConversationCount(t, ctx, ownerPool, 2, `SELECT count(*) FROM odysseia.realtime_outbox WHERE payload#>>'{data,type}'='conversation.created' AND payload#>>'{data,scope_id}'=$1`, dmID)

	again, owned, err := store.OpenDM(ctx, bob.ID, uuid.NewString(), conversation.DMDraft{UserID: alice.ID})
	if err != nil || owned || again.ID != dmID {
		t.Fatalf("reopen owned=%v id=%s err=%v", owned, again.ID, err)
	}
	var requestID string
	if err := ownerPool.QueryRow(ctx, `SELECT request_id::text FROM odysseia.conversations WHERE id=$1`, dmID).Scan(&requestID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.OpenDM(ctx, alice.ID, requestID, conversation.DMDraft{UserID: cara.ID}); !errors.Is(err, conversation.ErrConflict) {
		t.Fatalf("changed dm retry: %v", err)
	}
	if _, err := store.GetConversation(ctx, cara.ID, dmID); !errors.Is(err, conversation.ErrNotFound) {
		t.Fatalf("non-member read: %v", err)
	}

	messageKey := uuid.NewString()
	sent, err := store.SendConversationMessage(ctx, alice.ID, dmID, messageKey, conversation.MessageDraft{Body: "hello"})
	if err != nil || sent.Sequence != 1 || sent.AuthorID != alice.ID {
		t.Fatalf("send %+v err=%v", sent, err)
	}
	replay, err := store.SendConversationMessage(ctx, alice.ID, dmID, messageKey, conversation.MessageDraft{Body: "hello"})
	if err != nil || replay.ID != sent.ID {
		t.Fatalf("replay %+v err=%v", replay, err)
	}
	if _, err := store.SendConversationMessage(ctx, alice.ID, dmID, messageKey, conversation.MessageDraft{Body: "different"}); !errors.Is(err, conversation.ErrConflict) {
		t.Fatalf("changed message retry: %v", err)
	}
	assertConversationCount(t, ctx, ownerPool, 2, `SELECT count(*) FROM odysseia.realtime_outbox WHERE payload#>>'{data,type}'='conversation.message.created' AND payload#>>'{data,scope_id}'=$1`, dmID)
	assertConversationCount(t, ctx, ownerPool, 2, `SELECT count(*) FROM odysseia.realtime_outbox o JOIN odysseia.conversation_members m ON o.payload->>'channel' = 'inbox:#' || m.user_id::text WHERE o.payload#>>'{data,type}'='conversation.message.created' AND m.conversation_id=$1::uuid`, dmID)
	view, err := store.GetConversation(ctx, alice.ID, dmID)
	if err != nil || view.LastReadPosition != 1 || view.Unread != 0 {
		t.Fatalf("author cursor %+v err=%v", view, err)
	}
	bobView, err := store.GetConversation(ctx, bob.ID, dmID)
	if err != nil || bobView.Unread != 1 || bobView.LastReadPosition != 0 {
		t.Fatalf("peer unread %+v err=%v", bobView, err)
	}
	if _, err := store.SendConversationMessage(ctx, cara.ID, dmID, uuid.NewString(), conversation.MessageDraft{Body: "no"}); !errors.Is(err, conversation.ErrNotFound) {
		t.Fatalf("non-member write: %v", err)
	}
	quoted, err := store.SendConversationMessage(ctx, bob.ID, dmID, uuid.NewString(), conversation.MessageDraft{Body: "reply", QuoteID: sent.ID})
	if err != nil || quoted.QuoteID != sent.ID || quoted.Sequence != 2 {
		t.Fatalf("quote %+v err=%v", quoted, err)
	}
	group, err := store.CreateGroup(ctx, alice.ID, uuid.NewString(), conversation.GroupDraft{Title: "其他", MemberIDs: []string{dave.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SendConversationMessage(ctx, alice.ID, dmID, uuid.NewString(), conversation.MessageDraft{Body: "cross", QuoteID: "00000000-0000-4000-8000-000000000099"}); !errors.Is(err, conversation.ErrReference) {
		t.Fatalf("missing quote: %v", err)
	}
	if _, err := store.SendConversationMessage(ctx, alice.ID, group.ID, uuid.NewString(), conversation.MessageDraft{Body: "cross", QuoteID: sent.ID}); !errors.Is(err, conversation.ErrReference) {
		t.Fatalf("cross-conversation quote: %v", err)
	}

	stale, err := store.MarkConversationRead(ctx, alice.ID, dmID, 0)
	if err != nil || stale.Position != 1 {
		t.Fatalf("read moved backward %+v err=%v", stale, err)
	}
	readEvents := countQuery(t, ctx, ownerPool, `SELECT count(*) FROM odysseia.realtime_outbox WHERE payload#>>'{data,type}'='conversation.read' AND payload#>>'{data,scope_id}'=$1`, dmID)
	advanced, err := store.MarkConversationRead(ctx, alice.ID, dmID, 2)
	if err != nil || advanced.Position != 2 || advanced.Unread != 0 {
		t.Fatalf("read %+v err=%v", advanced, err)
	}
	if again, err := store.MarkConversationRead(ctx, alice.ID, dmID, 1); err != nil || again.Position != 2 {
		t.Fatalf("read regression via API %+v err=%v", again, err)
	}
	if countQuery(t, ctx, ownerPool, `SELECT count(*) FROM odysseia.realtime_outbox WHERE payload#>>'{data,type}'='conversation.read' AND payload#>>'{data,scope_id}'=$1`, dmID) != readEvents+1 {
		t.Fatal("read event was not private or was repeated")
	}
	assertConversationCount(t, ctx, ownerPool, 1, `SELECT count(*) FROM odysseia.realtime_outbox WHERE payload#>>'{data,type}'='conversation.read' AND payload->>'channel'=$1`, "inbox:#"+alice.ID)
	assertConversationCount(t, ctx, ownerPool, 0, `SELECT count(*) FROM odysseia.realtime_outbox WHERE payload#>>'{data,type}'='conversation.read' AND payload->>'channel'=$1`, "inbox:#"+bob.ID)
	if _, err := ownerPool.Exec(ctx, `UPDATE odysseia.conversation_members SET last_read_position = 0 WHERE conversation_id = $1 AND user_id = $2`, dmID, alice.ID); err == nil {
		t.Fatal("database accepted a read cursor regression")
	} else {
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != "23514" {
			t.Fatalf("read regression error: %v", err)
		}
	}
	summary, err := store.ListUnread(ctx, alice.ID)
	if err != nil || summary.Total != 0 {
		t.Fatalf("alice unread %+v err=%v", summary, err)
	}
	bobUnread, err := store.ListUnread(ctx, bob.ID)
	if err != nil || bobUnread.Total != 0 {
		t.Fatalf("author unread %+v err=%v", bobUnread, err)
	}

	if _, err := store.CreateGroup(ctx, alice.ID, uuid.NewString(), conversation.GroupDraft{Title: "拒绝", MemberIDs: []string{cara.ID}}); !errors.Is(err, conversation.ErrNotFound) {
		t.Fatalf("group invite without a shared forum: %v", err)
	}
	room, err := store.CreateGroup(ctx, alice.ID, uuid.NewString(), conversation.GroupDraft{Title: "房间", MemberIDs: []string{bob.ID}})
	if err != nil || room.OwnerID != alice.ID || len(room.Members) != 2 {
		t.Fatalf("group %+v err=%v", room, err)
	}
	if err := store.RemoveConversationMember(ctx, alice.ID, room.ID, alice.ID); !errors.Is(err, conversation.ErrLastManager) {
		t.Fatalf("owner leave: %v", err)
	}
	if err := store.RemoveConversationMember(ctx, bob.ID, room.ID, alice.ID); !errors.Is(err, conversation.ErrForbidden) {
		t.Fatalf("member removes owner: %v", err)
	}
	if err := store.RemoveConversationMember(ctx, alice.ID, room.ID, bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetConversation(ctx, bob.ID, room.ID); !errors.Is(err, conversation.ErrNotFound) {
		t.Fatalf("removed member read: %v", err)
	}
	if _, err := store.SendConversationMessage(ctx, bob.ID, room.ID, uuid.NewString(), conversation.MessageDraft{Body: "gone"}); !errors.Is(err, conversation.ErrNotFound) {
		t.Fatalf("removed member write: %v", err)
	}
	assertConversationCount(t, ctx, ownerPool, 2, `SELECT count(*) FROM odysseia.realtime_outbox WHERE payload#>>'{data,type}'='conversation.member.removed' AND payload#>>'{data,scope_id}'=$1`, room.ID)

	transferRoom, err := store.CreateGroup(ctx, alice.ID, uuid.NewString(), conversation.GroupDraft{Title: "移交", MemberIDs: []string{bob.ID, dave.ID}})
	if err != nil {
		t.Fatal(err)
	}
	transferred, err := store.TransferConversationOwner(ctx, alice.ID, transferRoom.ID, bob.ID)
	if err != nil || transferred.OwnerID != bob.ID {
		t.Fatalf("transfer %+v err=%v", transferred, err)
	}
	if err := store.RemoveConversationMember(ctx, alice.ID, transferRoom.ID, alice.ID); err != nil {
		t.Fatalf("former owner leave: %v", err)
	}
	if err := store.RemoveConversationMember(ctx, bob.ID, transferRoom.ID, dave.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveConversationMember(ctx, bob.ID, transferRoom.ID, bob.ID); !errors.Is(err, conversation.ErrLastManager) {
		t.Fatalf("sole owner leave: %v", err)
	}
	if err := store.DissolveConversation(ctx, bob.ID, transferRoom.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetConversation(ctx, bob.ID, transferRoom.ID); !errors.Is(err, conversation.ErrNotFound) {
		t.Fatalf("dissolved group: %v", err)
	}

	limited, err := NewCachedForums(runtimePool, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer limited.Close()
	if err := limited.UseConversationMemberLimit(2); err != nil {
		t.Fatal(err)
	}
	small, err := limited.CreateGroup(ctx, alice.ID, uuid.NewString(), conversation.GroupDraft{Title: "两人", MemberIDs: []string{bob.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limited.AddConversationMember(ctx, alice.ID, small.ID, dave.ID); !errors.Is(err, conversation.ErrInvalid) {
		t.Fatalf("configured member limit: %v", err)
	}
	testConversationMemberCeiling(t, ctx, ownerPool, small.ID)
	testConversationDowngradeGuard(t, ctx, ownerPool)
}

func testConversationMemberCeiling(t *testing.T, ctx context.Context, owner *pgxpool.Pool, conversationID string) {
	t.Helper()
	if _, err := owner.Exec(ctx, `WITH fresh AS (
		INSERT INTO odysseia.users (issuer, subject)
		SELECT 'https://p4-ceiling.example', 'm' || i::text FROM generate_series(1, 48) AS i
		RETURNING id
	)
	INSERT INTO odysseia.conversation_members (conversation_id, user_id, role)
	SELECT $1, id, 'member' FROM fresh`, conversationID); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := owner.QueryRow(ctx, `INSERT INTO odysseia.users (issuer, subject) VALUES ('https://p4-ceiling.example', 'overflow') RETURNING id::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	_, err := owner.Exec(ctx, `INSERT INTO odysseia.conversation_members (conversation_id, user_id, role) VALUES ($1, $2, 'member')`, conversationID, id)
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != "23514" {
		t.Fatalf("member ceiling: %v", err)
	}
}

func testConversationDowngradeGuard(t *testing.T, ctx context.Context, owner *pgxpool.Pool) {
	t.Helper()
	source, err := migrations.ReadFile("migrations/00010_conversations.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	_, err = tx.Exec(ctx, strings.Split(string(source), "-- +goose Down")[1])
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != "P0001" {
		t.Fatalf("downgrade silently discarded conversation data: %v", err)
	}
}

func assertConversationCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, want int64, query string, args ...any) {
	t.Helper()
	var count int64
	if err := pool.QueryRow(ctx, query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("%s count=%d want=%d", query, count, want)
	}
}

func countQuery(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query, value string) int64 {
	t.Helper()
	var count int64
	if err := pool.QueryRow(ctx, query, value).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
