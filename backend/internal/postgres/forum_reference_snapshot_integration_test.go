//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"odysseia.local/backend/internal/forum"
)

// Observe an actual database lock wait, then publish the root it protects.
// The follower must read that commit before validating the root and quote.
func testReplyReferenceSnapshot(t *testing.T, ctx context.Context, owner, runtimePool *pgxpool.Pool, actor, topicID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	writer, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(context.Background())
	rootDraft := forum.ReplyDraft{Body: "root becomes visible after the lock wait"}
	digest, err := requestDigest(rootDraft)
	if err != nil {
		t.Fatal(err)
	}
	rootKey := uuid.NewString()
	state, err := lockReply(ctx, writer, actor, topicID, rootKey, digest, rootDraft)
	if err != nil {
		t.Fatal(err)
	}
	root, err := (&Forums{}).appendReply(ctx, writer, topicID, actor, rootDraft, rootKey, digest[:], state)
	if err != nil {
		t.Fatal(err)
	}
	cfg := runtimePool.Config().Copy()
	cfg.MaxConns = 1
	followerPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer followerPool.Close()
	writeCtx, stop := context.WithCancel(ctx)
	defer stop() // Unblock the owned writer before closing its pool on failure.
	var pid uint32
	if err := followerPool.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	draft := forum.ReplyDraft{Body: "quote the newly committed root", RootID: root.ID, QuoteID: root.ID}
	key := uuid.NewString()
	store := NewForums(followerPool)
	type replyResult struct {
		entry forum.Entry
		err   error
	}
	finished := make(chan replyResult, 1)
	go func() {
		entry, err := store.Reply(writeCtx, actor, topicID, key, draft)
		finished <- replyResult{entry, err}
	}()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := runtimePool.QueryRow(ctx, `SELECT EXISTS(SELECT FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("quoted follower did not reach the real stream lock")
		}
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var got replyResult
	select {
	case got = <-finished:
	case <-ctx.Done():
		t.Fatal("quoted follower did not finish after commit")
	}
	if got.err != nil || got.entry.RootID != root.ID || got.entry.QuoteID != root.ID || got.entry.Sequence != root.Sequence+1 {
		t.Fatalf("quoted follower missed the committed root: %v", got.err)
	}
	retry, err := store.Reply(ctx, actor, topicID, key, draft)
	if err != nil || retry.ID != got.entry.ID {
		t.Fatalf("quoted retry did not return the original: %v", err)
	}
	changed := draft
	changed.QuoteID = uuid.NewString()
	if _, err := store.Reply(ctx, actor, topicID, key, changed); !errors.Is(err, forum.ErrConflict) {
		t.Fatalf("idempotency conflict lost precedence over reference validation: %v", err)
	}
	if _, err := store.Reply(ctx, actor, topicID, uuid.NewString(), draft); err != nil {
		t.Fatalf("connection was not reusable after the failed batch: %v", err)
	}
}
