//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/identity"
)

func testMessagePageCache(t *testing.T, ctx context.Context, store *Forums, ownerPool *pgxpool.Pool, alice string) {
	t.Helper()
	owner := NewForums(ownerPool)
	reader, err := NewUsers(ownerPool).Provision(ctx, identity.ExternalID{Issuer: "https://cache-fixture.example", Subject: "revocable-reader"})
	if err != nil {
		t.Fatal(err)
	}
	bob := reader.ID
	board, err := owner.Provision(ctx, "page cache fixture", []string{alice, bob})
	if err != nil {
		t.Fatal(err)
	}
	created, err := owner.CreateTopic(ctx, alice, board.ID, uuid.NewString(), forum.TopicDraft{Title: "cached history", Body: "first", Mode: "threaded"})
	if err != nil {
		t.Fatal(err)
	}
	read := func(actor string) []forum.Entry {
		t.Helper()
		entries, err := store.ListMessages(ctx, actor, created.Topic.ID, 0, 1)
		if err != nil {
			t.Fatal(err)
		}
		store.pages.data.Wait()
		return entries
	}
	first := read(alice)
	first[0].Body = "caller mutation must not enter shared cache"
	before := store.pages.hits.Load()
	if got := read(bob); len(got) != 1 || got[0].Body != "first" || store.pages.hits.Load() <= before {
		t.Fatal("authorized reader did not reuse an isolated content page")
	}
	if _, err := owner.Reply(ctx, alice, created.Topic.ID, uuid.NewString(), forum.ReplyDraft{Body: "second"}); err != nil {
		t.Fatal(err)
	}
	if got := read(alice); len(got) != 2 || got[1].Body != "second" {
		t.Fatal("another store's append did not advance the partial page")
	}
	loads := store.pages.loads.Load()
	if _, err := owner.Reply(ctx, alice, created.Topic.ID, uuid.NewString(), forum.ReplyDraft{Body: "third"}); err != nil {
		t.Fatal(err)
	}
	if got := read(alice); len(got) != 2 || got[1].Body != "second" || store.pages.loads.Load() != loads {
		t.Fatal("closed page prefix was unnecessarily invalidated by append")
	}
	if _, err := owner.RemoveMember(ctx, forum.MemberRemoval{OperationID: uuid.NewString(), ForumID: board.ID, UserID: bob, Operator: "cache fixture", Reason: "revoke a warm-cache reader"}); err != nil {
		t.Fatal(err)
	}
	hits := store.pages.hits.Load()
	if _, err := store.ListMessages(ctx, bob, created.Topic.ID, 0, 1); !errors.Is(err, forum.ErrNotFound) || store.pages.hits.Load() != hits {
		t.Fatal("a cached page bypassed current membership")
	}
}
