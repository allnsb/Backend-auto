//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/identity"
)

func testForums(t *testing.T, ctx context.Context, migrationURL string, runtimePool *pgxpool.Pool) {
	t.Helper()
	ownerPool, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerPool.Close()
	testRealtimeCommandGuards(t, ctx, ownerPool)
	_, err = ownerPool.Exec(ctx, `
		GRANT SELECT ON odysseia.forums, odysseia.forum_members, odysseia.message_streams,
			odysseia.topics, odysseia.messages, odysseia.forum_entries, odysseia.channels TO odysseia_runtime;
		GRANT INSERT ON odysseia.message_streams, odysseia.topics, odysseia.messages, odysseia.forum_entries, odysseia.channels TO odysseia_runtime;
		GRANT INSERT(method,payload) ON odysseia.realtime_outbox TO odysseia_runtime;
		GRANT USAGE ON SEQUENCE odysseia.realtime_outbox_id_seq TO odysseia_runtime;
		GRANT UPDATE(last_topic_position) ON odysseia.forums TO odysseia_runtime;
		GRANT UPDATE(last_position) ON odysseia.message_streams TO odysseia_runtime;`)
	if err != nil {
		t.Fatal(err)
	}
	users := NewUsers(ownerPool)
	alice, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://forum-fixture.example", Subject: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://forum-fixture.example", Subject: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	owner := NewForums(ownerPool)
	store := newCachedForumFixture(t, runtimePool)
	t.Run("authorized_content_cache", func(t *testing.T) { testMessagePageCache(t, ctx, store, ownerPool, alice.ID) })
	board, err := owner.Provision(ctx, "独立普通论坛", []string{alice.ID, bob.ID})
	if err != nil {
		t.Fatal(err)
	}
	private, err := owner.Provision(ctx, "另一成员论坛", []string{bob.ID})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Provision(ctx, "must not create forums", []string{alice.ID})
	var privilegeError *pgconn.PgError
	if !errors.As(err, &privilegeError) || privilegeError.Code != "42501" {
		t.Fatalf("runtime must not create an operator-only forum: %v", err)
	}
	_, err = runtimePool.Exec(ctx, `DELETE FROM odysseia.forum_members WHERE forum_id=$1 AND user_id=$2`, private.ID, bob.ID)
	if !errors.As(err, &privilegeError) || privilegeError.Code != "42501" {
		t.Fatalf("runtime must not change membership: %v", err)
	}
	if _, err := store.GetForum(ctx, alice.ID, private.ID); !errors.Is(err, forum.ErrNotFound) {
		t.Fatalf("private forum leaked: %v", err)
	}
	if _, err := store.CreateTopic(ctx, alice.ID, private.ID, uuid.NewString(), forum.TopicDraft{Title: "forbidden", Body: "forbidden", Mode: "threaded"}); !errors.Is(err, forum.ErrNotFound) {
		t.Fatalf("cross-forum write: %v", err)
	}

	draft := forum.TopicDraft{Title: "没有作品也可以发帖", Body: "single canonical opening body", Mode: "threaded"}
	requestID := uuid.NewString()
	type topicResult struct {
		value forum.CreatedTopic
		err   error
	}
	results := make(chan topicResult, 12)
	for range 12 {
		go func() {
			value, err := store.CreateTopic(ctx, alice.ID, board.ID, requestID, draft)
			results <- topicResult{value, err}
		}()
	}
	var first forum.CreatedTopic
	for range 12 {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		if first.Topic.ID == "" {
			first = got.value
		}
		if got.value.Topic.ID != first.Topic.ID || got.value.OpeningMessage.ID != first.OpeningMessage.ID {
			t.Fatal("duplicate topic request created different data")
		}
	}
	if first.OpeningMessage.AuthorID != alice.ID || first.OpeningMessage.Sequence != 1 {
		t.Fatal("opening message identity or order is incorrect")
	}
	changed := draft
	changed.Body = "different input"
	if _, err := store.CreateTopic(ctx, alice.ID, board.ID, requestID, changed); !errors.Is(err, forum.ErrConflict) {
		t.Fatalf("changed topic retry: %v", err)
	}
	assertForumCount(t, ctx, ownerPool, `SELECT count(*) FROM odysseia.topics WHERE forum_id=$1`, board.ID, 1)
	assertForumCount(t, ctx, ownerPool, `SELECT count(*) FROM odysseia.realtime_outbox WHERE payload->>'channel'=$1`, "forum:"+board.ID, 1)

	rootRequestID := uuid.NewString()
	root, err := store.Reply(ctx, alice.ID, first.Topic.ID, rootRequestID, forum.ReplyDraft{Body: "root floor"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Reply(ctx, bob.ID, first.Topic.ID, uuid.NewString(), forum.ReplyDraft{Body: "child", RootID: root.ID, QuoteID: root.ID})
	if err != nil {
		t.Fatal(err)
	}
	next, err := store.Reply(ctx, alice.ID, first.Topic.ID, uuid.NewString(), forum.ReplyDraft{Body: "quote child without deeper nesting", RootID: root.ID, QuoteID: child.ID})
	if err != nil {
		t.Fatal(err)
	}
	if next.RootID != root.ID || next.QuoteID != child.ID {
		t.Fatal("root ownership was confused with quoted message")
	}
	if _, err := store.Reply(ctx, alice.ID, first.Topic.ID, uuid.NewString(), forum.ReplyDraft{Body: "third level", RootID: child.ID}); !errors.Is(err, forum.ErrReference) {
		t.Fatalf("third nesting level: %v", err)
	}
	_, err = ownerPool.Exec(ctx, `UPDATE odysseia.forum_entries SET root_id=$1 WHERE message_id=$2`, child.ID, next.ID)
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != "23503" {
		t.Fatalf("database failed to enforce one nested level: %v", err)
	}
	other, err := store.CreateTopic(ctx, bob.ID, private.ID, uuid.NewString(), draft)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Reply(ctx, alice.ID, first.Topic.ID, uuid.NewString(), forum.ReplyDraft{Body: "cross-topic quote", QuoteID: other.OpeningMessage.ID}); !errors.Is(err, forum.ErrReference) {
		t.Fatalf("cross-topic quote accepted: %v", err)
	}
	_, err = ownerPool.Exec(ctx, `UPDATE odysseia.messages SET quote_id=$1 WHERE id=$2`, other.OpeningMessage.ID, child.ID)
	if !errors.As(err, &pgError) || pgError.Code != "23503" {
		t.Fatalf("database failed to enforce same-stream quote: %v", err)
	}

	t.Run("reply retries and ordering", func(t *testing.T) { testReplyConcurrency(t, ctx, store, ownerPool, alice.ID, first.Topic.ID) })
	t.Run("quoted reply after lock holder commits", func(t *testing.T) {
		testReplyReferenceSnapshot(t, ctx, ownerPool, runtimePool, alice.ID, first.Topic.ID)
	})
	t.Run("transaction event rollback", func(t *testing.T) { testForumRollback(t, ctx, store, ownerPool, alice.ID, board.ID, first.Topic.ID) })
	t.Run("blocked writes preserve reads", func(t *testing.T) {
		testWriteBudgetReadReserve(t, ctx, store, ownerPool, runtimePool, alice.ID, first.Topic.ID)
	})
	t.Run("configured write budget", func(t *testing.T) {
		for _, limit := range []int64{-1, int64(runtimePool.Config().MaxConns) + 1} {
			if configured, err := NewCachedForums(runtimePool, limit); err == nil || configured != nil {
				t.Fatalf("invalid limit %d allocated a serving store: %v", limit, err)
			}
		}
		configured, err := NewCachedForums(runtimePool, 2)
		if err != nil {
			t.Fatal(err)
		}
		defer configured.Close()
		if configured.writes.limit != 2 {
			t.Fatalf("explicit write limit was ignored: %d", configured.writes.limit)
		}
		testWriteBudgetReadReserve(t, ctx, configured, ownerPool, runtimePool, alice.ID, first.Topic.ID)
	})
	t.Run("queued authorization and privacy", func(t *testing.T) {
		testQueuedWriteAuthorization(t, ctx, store, ownerPool, alice.ID, bob.ID)
	})
	t.Run("HTTPS forum contract", func(t *testing.T) { testForumHTTP(t, ctx, runtimePool, alice, bob, board, private, first.Topic.ID) })
	_, err = ownerPool.Exec(ctx, `DELETE FROM odysseia.forum_members WHERE forum_id=$1 AND user_id=$2`, board.ID, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetTopic(ctx, alice.ID, first.Topic.ID); !errors.Is(err, forum.ErrNotFound) {
		t.Fatalf("revoked user read history: %v", err)
	}
	if _, err := store.CreateTopic(ctx, alice.ID, board.ID, requestID, draft); !errors.Is(err, forum.ErrNotFound) {
		t.Fatalf("idempotency retry bypassed revoked membership: %v", err)
	}
	if _, err := store.Reply(ctx, alice.ID, first.Topic.ID, rootRequestID, forum.ReplyDraft{Body: "root floor"}); !errors.Is(err, forum.ErrNotFound) {
		t.Fatalf("reply retry bypassed revoked membership: %v", err)
	}
	t.Run("audited member removal", func(t *testing.T) { testMemberRemoval(t, ctx, ownerPool, runtimePool, bob.ID) })
}

func testReplyConcurrency(t *testing.T, ctx context.Context, store *Forums, ownerPool *pgxpool.Pool, actor, topicID string) {
	t.Helper()
	key := uuid.NewString()
	type result struct {
		entry forum.Entry
		err   error
	}
	results := make(chan result, 12)
	for range 12 {
		go func() {
			entry, err := store.Reply(ctx, actor, topicID, key, forum.ReplyDraft{Body: "one logical reply"})
			results <- result{entry, err}
		}()
	}
	var expected forum.Entry
	for range 12 {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		if expected.ID == "" {
			expected = got.entry
		}
		if got.entry.ID != expected.ID || got.entry.Sequence != expected.Sequence {
			t.Fatal("idempotent reply duplicated")
		}
	}
	if _, err := store.Reply(ctx, actor, topicID, key, forum.ReplyDraft{Body: "conflicting body"}); !errors.Is(err, forum.ErrConflict) {
		t.Fatalf("conflicting reply retry: %v", err)
	}
	for i := range 8 {
		go func() {
			entry, err := store.Reply(ctx, actor, topicID, uuid.NewString(), forum.ReplyDraft{Body: fmt.Sprintf("parallel-%d", i)})
			results <- result{entry, err}
		}()
	}
	positions := make(map[int64]struct{}, 8)
	for range 8 {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		positions[got.entry.Sequence] = struct{}{}
	}
	if len(positions) != 8 {
		t.Fatal("parallel writes reused sequence positions")
	}
	entries, err := store.ListMessages(ctx, actor, topicID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i, entry := range entries {
		if entry.Sequence != int64(i+1) {
			t.Fatal("committed message order has duplicate or missing positions")
		}
	}
	assertForumCount(t, ctx, ownerPool, `SELECT count(*) FROM odysseia.realtime_outbox WHERE payload->>'channel'=$1`, "topic:"+topicID, int64(len(entries)-1))
	var valid bool
	err = ownerPool.QueryRow(ctx, `SELECT bool_and(payload->>'idempotency_key'=payload#>>'{data,id}' AND payload#>>'{data,version}'='1') FROM odysseia.realtime_outbox WHERE method='publish'`).Scan(&valid)
	if err != nil || !valid {
		t.Fatalf("outbox event identity/version missing: %v", err)
	}
	encoded, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	err = ownerPool.QueryRow(ctx, `SELECT o.payload#>'{data,data}'=$2::jsonb
		AND (o.payload#>>'{data,data,created_at}')::timestamptz=m.created_at
		AND o.payload#>>'{data,data,sequence}'=m.position::text
		FROM odysseia.realtime_outbox o JOIN odysseia.messages m ON m.id::text=o.payload#>>'{data,data,id}'
		WHERE m.id=$1`, expected.ID, encoded).Scan(&valid)
	if err != nil || !valid {
		t.Fatalf("reply response, persisted sequence/time and event differ: %v", err)
	}
}

func testForumRollback(t *testing.T, ctx context.Context, store *Forums, ownerPool *pgxpool.Pool, actor, forumID, topicID string) {
	t.Helper()
	snapshot := func(t *testing.T) string {
		t.Helper()
		var state []byte
		err := ownerPool.QueryRow(ctx, `SELECT json_build_array((SELECT count(*) FROM odysseia.topics),
			(SELECT count(*) FROM odysseia.messages),(SELECT count(*) FROM odysseia.message_streams),
			(SELECT count(*) FROM odysseia.forum_entries),
			(SELECT count(*) FROM odysseia.realtime_outbox),(SELECT last_topic_position FROM odysseia.forums WHERE id=$1),
			(SELECT last_position FROM odysseia.message_streams WHERE id=$2))`, forumID, topicID).Scan(&state)
		if err != nil {
			t.Fatal(err)
		}
		return string(state)
	}
	// These are fixed fixture tables: fail each batch position independently.
	for _, table := range []string{"forum_entries", "realtime_outbox"} {
		t.Run(table, func(t *testing.T) {
			before := snapshot(t)
			target := "odysseia." + table
			if _, err := ownerPool.Exec(ctx, "ALTER TABLE "+target+" ADD CONSTRAINT synthetic_reject_write CHECK (false) NOT VALID"); err != nil {
				t.Fatal(err)
			}
			drop := "ALTER TABLE " + target + " DROP CONSTRAINT IF EXISTS synthetic_reject_write"
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, err := ownerPool.Exec(cleanup, drop); err != nil {
					t.Error(err)
				}
			})
			replyKey, topicKey := uuid.NewString(), uuid.NewString()
			replyDraft := forum.ReplyDraft{Body: "retry after a failed transaction"}
			topicDraft := forum.TopicDraft{Title: "retry after rollback", Body: "opening body", Mode: "threaded"}
			_, replyErr := store.Reply(ctx, actor, topicID, replyKey, replyDraft)
			_, topicErr := store.CreateTopic(ctx, actor, forumID, topicKey, topicDraft)
			var pgError *pgconn.PgError
			if !errors.As(replyErr, &pgError) || pgError.Code != "23514" {
				t.Fatalf("reply lost the constraint failure: %v", replyErr)
			}
			if !errors.As(topicErr, &pgError) || pgError.Code != "23514" {
				t.Fatalf("topic lost the constraint failure: %v", topicErr)
			}
			if after := snapshot(t); after != before {
				t.Fatalf("data or counters survived failed write: before=%s after=%s", before, after)
			}
			if _, err := ownerPool.Exec(ctx, drop); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Reply(ctx, actor, topicID, replyKey, replyDraft); err != nil {
				t.Fatalf("same reply key failed after repair: %v", err)
			}
			if _, err := store.CreateTopic(ctx, actor, forumID, topicKey, topicDraft); err != nil {
				t.Fatalf("same topic key failed after repair: %v", err)
			}
		})
	}
}

func assertForumCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query, value string, want int64) {
	t.Helper()
	var count int64
	if err := pool.QueryRow(ctx, query, value).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("count=%d, want %d", count, want)
	}
}
