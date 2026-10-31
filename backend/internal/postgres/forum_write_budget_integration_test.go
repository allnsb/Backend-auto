//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"odysseia.local/backend/internal/forum"
)

// Hold a real stream lock. Other streams must still read and write before
// releasing it, even with more hot callers than the global writer budget.
func testWriteBudgetReadReserve(t *testing.T, ctx context.Context, store *Forums, owner, runtime *pgxpool.Pool, actor, topicID string) {
	t.Helper()
	topic, err := store.GetTopic(ctx, actor, topicID)
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := store.CreateTopic(ctx, actor, topic.ForumID, uuid.NewString(), forum.TopicDraft{Title: "independent stream", Body: "opening", Mode: "threaded"})
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	var position int64
	if err := blocker.QueryRow(ctx, `SELECT last_position FROM odysseia.message_streams WHERE id=$1 FOR UPDATE`, topicID).Scan(&position); err != nil {
		t.Fatal(err)
	}
	writeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	writers := int(store.writes.limit)
	perCounter := min(writers, int(forumWriteScopeLimit))
	results := make(chan error, 2*writers)
	for range 2 * writers {
		go func() {
			_, err := store.Reply(writeCtx, actor, topicID, uuid.NewString(), forum.ReplyDraft{Body: "canceled before the stream lock is released"})
			results <- err
		}()
	}
	probe, err := Open(ctx, fixtureURL(t, "ODYSSEIA_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	observeCtx, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var waiting int
	for waiting < perCounter {
		select {
		case <-ticker.C:
			err := probe.QueryRow(observeCtx, `SELECT count(*) FROM pg_stat_activity
				WHERE datname=current_database() AND usename=current_user AND pid<>pg_backend_pid() AND wait_event_type='Lock'`).Scan(&waiting)
			if err != nil {
				t.Fatal(err)
			}
		case <-observeCtx.Done():
			t.Fatal("writers did not reach the held database lock")
		}
	}
	if waiting != perCounter {
		t.Fatalf("counter budget exceeded: %d database lock waiters, limit %d", waiting, perCounter)
	}
	waitWriteReferences(t, store.writes, writeScope{streamID: topicID}, min(2*writers, forumWriteScopeQueueLimit))
	readCtx, finishRead := context.WithTimeout(ctx, time.Second)
	defer finishRead()
	if _, err := store.GetTopic(readCtx, actor, topicID); err != nil {
		t.Fatalf("blocked writers starved an authorized read: %v", err)
	}
	if _, err := store.Reply(readCtx, actor, ordinary.Topic.ID, uuid.NewString(), forum.ReplyDraft{Body: "unrelated stream must progress"}); err != nil {
		t.Fatalf("blocked hot stream starved an unrelated write: %v", err)
	}
	cancel()
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 * writers {
		if err := <-results; err == nil {
			t.Fatal("canceled blocked writer committed")
		}
	}
	var after int64
	if err := owner.QueryRow(ctx, `SELECT last_position FROM odysseia.message_streams WHERE id=$1`, topicID).Scan(&after); err != nil || after != position {
		t.Fatalf("blocked canceled work changed the stream: %d -> %d: %v", position, after, err)
	}
}
