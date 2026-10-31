//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"odysseia.local/backend/internal/forum"
)

func testMemberRemoval(t *testing.T, ctx context.Context, ownerPool, runtimePool *pgxpool.Pool, memberID string) {
	t.Helper()
	owner := NewForums(ownerPool)
	board, err := owner.Provision(ctx, "membership command fixture", []string{memberID})
	if err != nil {
		t.Fatal(err)
	}
	command := forum.MemberRemoval{OperationID: uuid.NewString(), ForumID: board.ID, UserID: memberID, Operator: "synthetic-operator", Reason: "verify explicit revocation"}
	_, err = NewForums(runtimePool).RemoveMember(ctx, command)
	var denied *pgconn.PgError
	if !errors.As(err, &denied) || denied.Code != "42501" {
		t.Fatalf("API runtime must not remove membership: %v", err)
	}
	if _, err := ownerPool.Exec(ctx, `ALTER TABLE odysseia.realtime_outbox ADD CONSTRAINT synthetic_reject_removal CHECK (false) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	defer ownerPool.Exec(context.Background(), `ALTER TABLE odysseia.realtime_outbox DROP CONSTRAINT IF EXISTS synthetic_reject_removal`)
	if _, err := owner.RemoveMember(ctx, command); err == nil {
		t.Fatal("removal survived outbox failure")
	}
	if _, err := owner.GetForum(ctx, memberID, board.ID); err != nil {
		t.Fatal("failed event did not roll back membership removal")
	}
	assertForumCount(t, ctx, ownerPool, `SELECT count(*) FROM odysseia.forum_member_removals WHERE operation_id=$1`, command.OperationID, 0)
	if _, err := ownerPool.Exec(ctx, `ALTER TABLE odysseia.realtime_outbox DROP CONSTRAINT synthetic_reject_removal`); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result forum.RemovedMember
		err    error
	}
	results := make(chan outcome, 12)
	for range 12 {
		go func() { value, err := owner.RemoveMember(ctx, command); results <- outcome{value, err} }()
	}
	for range 12 {
		got := <-results
		if got.err != nil || !got.result.Removed || got.result.MemberRemoval != command {
			t.Fatalf("concurrent removal: %v", got.err)
		}
	}
	assertForumCount(t, ctx, ownerPool, `SELECT count(*) FROM odysseia.forum_member_removals WHERE operation_id=$1`, command.OperationID, 1)
	assertForumCount(t, ctx, ownerPool, `SELECT count(*) FROM odysseia.realtime_outbox WHERE method='disconnect' AND payload->>'user'=$1`, memberID, 1)
	if _, err := owner.GetForum(ctx, memberID, board.ID); !errors.Is(err, forum.ErrNotFound) {
		t.Fatal("removed member can still read")
	}
	changed := command
	changed.Reason = "different command with same operation ID"
	if _, err := owner.RemoveMember(ctx, changed); !errors.Is(err, forum.ErrConflict) {
		t.Fatal("operation identity accepted different command")
	}
	command.OperationID = uuid.NewString()
	noop, err := owner.RemoveMember(ctx, command)
	if err != nil || noop.Removed {
		t.Fatalf("absent-member removal result: %v", err)
	}
	assertForumCount(t, ctx, ownerPool, `SELECT count(*) FROM odysseia.realtime_outbox WHERE method='disconnect' AND payload->>'user'=$1`, memberID, 2)
}
