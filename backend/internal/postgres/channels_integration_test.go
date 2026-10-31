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
	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/identity"
	"odysseia.local/backend/internal/message"
)

func testDiscussionModesAndChannels(t *testing.T, ctx context.Context, ownerPool, runtimePool *pgxpool.Pool) {
	t.Helper()
	operator := NewForums(ownerPool)
	store := newCachedForumFixture(t, runtimePool)
	alice, err := NewUsers(ownerPool).Provision(ctx, identity.ExternalID{Issuer: "https://p3-fixture.example", Subject: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	outsider, err := NewUsers(ownerPool).Provision(ctx, identity.ExternalID{Issuer: "https://p3-fixture.example", Subject: "outsider"})
	if err != nil {
		t.Fatal(err)
	}
	board, err := operator.Provision(ctx, "all three discussion forms", []string{alice.ID})
	if err != nil {
		t.Fatal(err)
	}
	chat, err := store.CreateTopic(ctx, alice.ID, board.ID, uuid.NewString(), forum.TopicDraft{Title: "chat layout", Body: "one canonical opening", Mode: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetTopic(ctx, alice.ID, chat.Topic.ID)
	if err != nil || loaded.Mode != "chat" {
		t.Fatalf("chat mode was not persisted: %v", err)
	}
	child, err := store.Reply(ctx, alice.ID, chat.Topic.ID, uuid.NewString(), forum.ReplyDraft{Body: "layout does not change quote semantics", RootID: chat.OpeningMessage.ID, QuoteID: chat.OpeningMessage.ID})
	if err != nil || child.RootID != child.QuoteID {
		t.Fatalf("chat layout lost independent root/quote relationships: %v", err)
	}
	_, err = store.CreateTopic(ctx, alice.ID, board.ID, uuid.NewString(), forum.TopicDraft{Title: "invalid mode", Body: "invalid", Mode: "unknown"})
	if !errors.Is(err, forum.ErrInvalid) {
		t.Fatalf("unknown mode accepted: %v", err)
	}

	key := uuid.NewString()
	type created struct {
		channel forum.Channel
		err     error
	}
	results := make(chan created, 8)
	// Two independent adapter instances exercise PostgreSQL idempotency and
	// ordering, rather than depending exclusively on the local stream gate.
	other := NewForums(runtimePool)
	for index := range 8 {
		backend := store
		if index%2 != 0 {
			backend = other
		}
		go func() {
			value, err := backend.CreateChannel(ctx, alice.ID, board.ID, key, forum.ChannelDraft{Title: "fixed chat"})
			results <- created{value, err}
		}()
	}
	var channel forum.Channel
	for range 8 {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		if channel.ID != "" && channel != got.channel {
			t.Fatal("concurrent channel retry created different resources")
		}
		channel = got.channel
	}
	if channel.Position <= chat.Topic.Position {
		t.Fatal("topic and channel creation do not share forum event order")
	}
	if _, err := store.CreateChannel(ctx, alice.ID, board.ID, key, forum.ChannelDraft{Title: "changed"}); !errors.Is(err, forum.ErrConflict) {
		t.Fatalf("changed channel retry: %v", err)
	}
	assertForumCount(t, ctx, ownerPool, `SELECT count(*) FROM odysseia.realtime_outbox WHERE payload#>>'{data,data,id}'=$1`, channel.ID, 1)
	page, err := store.ListChannelMessages(ctx, alice.ID, channel.ID, 0, 2)
	if err != nil || len(page) != 0 {
		t.Fatalf("channel must start without a synthetic opening: %v", err)
	}

	requestID := uuid.NewString()
	messages := make(chan struct {
		value message.Message
		err   error
	}, 8)
	for index := range 8 {
		backend := store
		if index%2 != 0 {
			backend = other
		}
		go func() {
			value, err := backend.SendChannelMessage(ctx, alice.ID, channel.ID, requestID, forum.ChannelMessageDraft{Body: "first canonical channel message"})
			messages <- struct {
				value message.Message
				err   error
			}{value, err}
		}()
	}
	var first message.Message
	for range 8 {
		got := <-messages
		if got.err != nil {
			t.Fatal(got.err)
		}
		if first.ID != "" && !sameStoredMessage(first, got.value) {
			t.Fatal("channel retry duplicated message identity")
		}
		first = got.value
	}
	if first.Sequence != 1 || first.AuthorID != alice.ID {
		t.Fatal("channel message identity/order is incorrect")
	}
	quoted, err := store.SendChannelMessage(ctx, alice.ID, channel.ID, uuid.NewString(), forum.ChannelMessageDraft{Body: "quote in same channel", QuoteID: first.ID})
	if err != nil || quoted.QuoteID != first.ID || quoted.Sequence != 2 {
		t.Fatalf("channel quote failed: %v", err)
	}
	if _, err := store.SendChannelMessage(ctx, alice.ID, channel.ID, requestID, forum.ChannelMessageDraft{Body: "changed retry", QuoteID: uuid.NewString()}); !errors.Is(err, forum.ErrConflict) {
		t.Fatalf("conflict must precede invalid changed quote: %v", err)
	}
	if _, err := store.SendChannelMessage(ctx, alice.ID, channel.ID, uuid.NewString(), forum.ChannelMessageDraft{Body: "cross-stream reference", QuoteID: child.ID}); !errors.Is(err, forum.ErrReference) {
		t.Fatalf("topic reference leaked into channel: %v", err)
	}
	if _, err := store.Reply(ctx, alice.ID, chat.Topic.ID, uuid.NewString(), forum.ReplyDraft{Body: "cross-stream reference", QuoteID: first.ID}); !errors.Is(err, forum.ErrReference) {
		t.Fatalf("channel reference leaked into topic: %v", err)
	}
	page, err = store.ListChannelMessages(ctx, alice.ID, channel.ID, 0, 1)
	if err != nil || len(page) != 2 || !sameStoredMessage(page[0], first) || !sameStoredMessage(page[1], quoted) {
		t.Fatalf("shared history page changed canonical content: %v", err)
	}
	assertForumCount(t, ctx, ownerPool, `SELECT count(*) FROM odysseia.forum_entries WHERE stream_id=$1`, channel.ID, 0)
	assertForumCount(t, ctx, ownerPool, `SELECT count(*) FROM odysseia.messages WHERE stream_id=$1`, channel.ID, 2)
	assertForumCount(t, ctx, ownerPool, `SELECT count(*) FROM odysseia.realtime_outbox WHERE payload->>'channel'=$1`, "channel:"+channel.ID, 2)
	if _, err := store.GetTopic(ctx, alice.ID, channel.ID); !errors.Is(err, forum.ErrNotFound) {
		t.Fatalf("channel became a forum topic: %v", err)
	}
	if _, err := store.GetChannel(ctx, alice.ID, chat.Topic.ID); !errors.Is(err, forum.ErrNotFound) {
		t.Fatalf("topic became a product channel: %v", err)
	}
	if _, err := store.GetChannel(ctx, outsider.ID, channel.ID); !errors.Is(err, forum.ErrNotFound) {
		t.Fatalf("channel metadata leaked: %v", err)
	}
	if _, err := store.SendChannelMessage(ctx, outsider.ID, channel.ID, uuid.NewString(), forum.ChannelMessageDraft{Body: "forbidden"}); !errors.Is(err, forum.ErrNotFound) {
		t.Fatalf("outsider channel write: %v", err)
	}
	_, err = ownerPool.Exec(ctx, `INSERT INTO odysseia.channels(id,forum_id,position,author_id,title,request_id,request_hash) VALUES ($1,$2,99999,$3,'invalid shared stream',$4,$5)`, chat.Topic.ID, board.ID, alice.ID, uuid.NewString(), make([]byte, 32))
	var constraint *pgconn.PgError
	if !errors.As(err, &constraint) || constraint.Code != "23503" {
		t.Fatalf("database allowed a topic stream to become a channel: %v", err)
	}

	t.Run("atomic_channel_failures", func(t *testing.T) { testChannelRollback(t, ctx, store, ownerPool, alice.ID, board.ID, channel.ID) })
	t.Run("https_contract", func(t *testing.T) {
		testChannelHTTP(t, ctx, runtimePool, store, alice, outsider, board, channel, chat.Topic.ID)
	})
	_, err = operator.RemoveMember(ctx, forum.MemberRemoval{OperationID: uuid.NewString(), ForumID: board.ID, UserID: alice.ID, Operator: "P3 fixture", Reason: "channel cache and retries must not retain access"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListChannelMessages(ctx, alice.ID, channel.ID, 0, 1); !errors.Is(err, forum.ErrNotFound) {
		t.Fatalf("cached channel history survived revocation: %v", err)
	}
	if _, err := store.SendChannelMessage(ctx, alice.ID, channel.ID, requestID, forum.ChannelMessageDraft{Body: first.Body}); !errors.Is(err, forum.ErrNotFound) {
		t.Fatalf("idempotent channel retry survived revocation: %v", err)
	}
	testP3DowngradeGuard(t, ctx, ownerPool)
}

func testChannelRollback(t *testing.T, ctx context.Context, store *Forums, owner *pgxpool.Pool, actor, forumID, channelID string) {
	t.Helper()
	var before string
	query := `SELECT json_build_array((SELECT count(*) FROM odysseia.messages WHERE stream_id=$1),(SELECT last_position FROM odysseia.message_streams WHERE id=$1),(SELECT count(*) FROM odysseia.channels),(SELECT count(*) FROM odysseia.message_streams),(SELECT last_topic_position FROM odysseia.forums WHERE id=$2),(SELECT count(*) FROM odysseia.realtime_outbox))::text`
	if err := owner.QueryRow(ctx, query, channelID, forumID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `ALTER TABLE odysseia.realtime_outbox ADD CONSTRAINT synthetic_channel_failure CHECK(false) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	drop := `ALTER TABLE odysseia.realtime_outbox DROP CONSTRAINT IF EXISTS synthetic_channel_failure`
	t.Cleanup(func() {
		if _, err := owner.Exec(context.Background(), drop); err != nil {
			t.Error(err)
		}
	})
	key := uuid.NewString()
	_, sendErr := store.SendChannelMessage(ctx, actor, channelID, key, forum.ChannelMessageDraft{Body: "retry after rollback"})
	_, createErr := store.CreateChannel(ctx, actor, forumID, key, forum.ChannelDraft{Title: "retry after rollback"})
	for _, err := range []error{sendErr, createErr} {
		var constraint *pgconn.PgError
		if !errors.As(err, &constraint) || constraint.Code != "23514" {
			t.Fatalf("lost outbox failure: %v", err)
		}
	}
	var after string
	if err := owner.QueryRow(ctx, query, channelID, forumID).Scan(&after); err != nil || before != after {
		t.Fatalf("channel failure retained body/counter/event: %v", err)
	}
	if _, err := owner.Exec(ctx, drop); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SendChannelMessage(ctx, actor, channelID, key, forum.ChannelMessageDraft{Body: "retry after rollback"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateChannel(ctx, actor, forumID, key, forum.ChannelDraft{Title: "retry after rollback"}); err != nil {
		t.Fatal(err)
	}
}

func testP3DowngradeGuard(t *testing.T, ctx context.Context, owner *pgxpool.Pool) {
	t.Helper()
	source, err := migrations.ReadFile("migrations/00008_discussion_modes_and_channels.sql")
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
		t.Fatalf("downgrade silently discarded P3 data: %v", err)
	}
}

// sameStoredMessage compares the persisted text fields. Attachments are a slice,
// so the message value itself is no longer comparable with ==.
func sameStoredMessage(left, right message.Message) bool {
	return left.ID == right.ID && left.StreamID == right.StreamID && left.Sequence == right.Sequence &&
		left.AuthorID == right.AuthorID && left.Body == right.Body && left.QuoteID == right.QuoteID &&
		left.CreatedAt.Equal(right.CreatedAt) && len(left.Attachments) == len(right.Attachments)
}

func sameStoredEntry(left, right forum.Entry) bool {
	return sameStoredMessage(left.Message, right.Message) && left.RootID == right.RootID
}
