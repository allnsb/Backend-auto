//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"odysseia.local/backend/internal/blob"
	"odysseia.local/backend/internal/conversation"
	"odysseia.local/backend/internal/emoji"
	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/identity"
)

// Run within the normal migration fixture, using the restricted runtime role
// for business commands and the owner only for arranging concurrent states.
func testTakeoverRegressions(t *testing.T, ctx context.Context, migrationURL string, runtime *pgxpool.Pool) {
	owner, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	users := NewUsers(owner)
	alice, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://takeover-regression.invalid", Subject: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://takeover-regression.invalid", Subject: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	board, err := NewForums(owner).Provision(ctx, "takeover regression", []string{alice.ID, bob.ID})
	if err != nil {
		t.Fatal(err)
	}
	store := NewForums(runtime)
	group, err := store.CreateGroup(ctx, alice.ID, uuid.NewString(), conversation.GroupDraft{Title: "concurrent removal", MemberIDs: []string{bob.ID}})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("waiting sender observes committed removal", func(t *testing.T) {
		config := runtime.Config().Copy()
		name := "revoked-writer-" + uuid.NewString()
		config.ConnConfig.RuntimeParams["application_name"] = name
		writers, err := pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		defer writers.Close()
		tx, err := owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := lockConversation(ctx, tx, alice.ID, group.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE odysseia.conversation_members SET removed_at=statement_timestamp()
			WHERE conversation_id=$1 AND user_id=$2`, group.ID, bob.ID); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, err := NewForums(writers).SendConversationMessage(ctx, bob.ID, group.ID, uuid.NewString(), conversation.MessageDraft{Body: "must not commit"})
			result <- err
		}()
		waitTakeoverLock(t, ctx, owner, name)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-result; !errors.Is(err, conversation.ErrNotFound) {
			t.Fatalf("revoked queued send error=%v", err)
		}
		assertConversationCount(t, ctx, owner, 0, `SELECT count(*) FROM odysseia.messages WHERE stream_id=$1`, group.ID)
	})

	if _, err := owner.Exec(ctx, `INSERT INTO odysseia.user_groups(user_id,group_id) VALUES($1,$2)`, alice.ID, identity.OwnerGroupID); err != nil {
		t.Fatal(err)
	}
	t.Run("directory winner manages forum emoji", func(t *testing.T) {
		objects, err := blob.NewMemoryPublic("https://emoji.invalid")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.UseEmoji(EmojiSettings{Store: objects, Limits: emoji.DefaultLimits()}); err != nil {
			t.Fatal(err)
		}
		image := mustEmojiImage(t, mustIntegrationPNG(t))
		created, err := store.CreateForumEmoji(ctx, alice.ID, board.ID, uuid.NewString(), "directory_owner", image)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteForumEmoji(ctx, alice.ID, board.ID, created.ID); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("group changes preserve the last effective manager", func(t *testing.T) {
		roles := NewCollaboration(runtime)
		removed, err := roles.RemoveGroup(ctx, alice.ID, identity.OwnerGroupID, alice.ID)
		if removed || !errors.Is(err, forum.ErrLastManager) {
			t.Fatalf("remove last manager=%v error=%v", removed, err)
		}
		observer := uuid.NewString()
		if _, err := owner.Exec(ctx, `INSERT INTO odysseia.identity_groups
			(id,name,priority,can_read,can_post,can_delete,can_manage_members,can_manage_groups)
			VALUES($1,'takeover_observer',1000,true,false,false,false,false)`, observer); err != nil {
			t.Fatal(err)
		}
		if err := roles.AssignGroup(ctx, alice.ID, observer, alice.ID); !errors.Is(err, forum.ErrLastManager) {
			t.Fatalf("higher read-only group must not hide the last manager: %v", err)
		}
		effective, err := roles.EffectivePermissions(ctx, alice.ID, board.ID)
		if err != nil || effective.GroupID != identity.OwnerGroupID {
			t.Fatalf("rejected changes must roll back: effective=%+v error=%v", effective, err)
		}
		if err := roles.AssignGroup(ctx, alice.ID, identity.ModeratorGroupID, bob.ID); err != nil {
			t.Fatal(err)
		}
		removed, err = roles.RemoveGroup(ctx, alice.ID, identity.OwnerGroupID, alice.ID)
		if !removed || err != nil {
			t.Fatalf("another manager allows removal: removed=%v error=%v", removed, err)
		}
	})
	t.Run("concurrent directory removals keep one manager", func(t *testing.T) {
		carol, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://takeover-regression.invalid", Subject: "carol"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(ctx, `INSERT INTO odysseia.user_groups(user_id,group_id)
			SELECT unnest($1::uuid[]),$2::uuid ON CONFLICT DO NOTHING`, []string{alice.ID, bob.ID, carol.ID}, identity.OwnerGroupID); err != nil {
			t.Fatal(err)
		}
		roles := NewCollaboration(runtime)
		if _, err := roles.RemoveGroup(ctx, carol.ID, identity.ModeratorGroupID, bob.ID); err != nil {
			t.Fatal(err)
		}
		results := make(chan error, 2)
		for _, userID := range []string{alice.ID, bob.ID} {
			go func() {
				_, err := roles.RemoveGroup(ctx, carol.ID, identity.OwnerGroupID, userID)
				results <- err
			}()
		}
		accepted, rejected := 0, 0
		for range 2 {
			switch err := <-results; {
			case err == nil:
				accepted++
			case errors.Is(err, forum.ErrLastManager):
				rejected++
			default:
				t.Fatalf("concurrent removal: %v", err)
			}
		}
		if accepted != 1 || rejected != 1 {
			t.Fatalf("accepted=%d rejected=%d; exactly one manager must remain", accepted, rejected)
		}
	})
}

func waitTakeoverLock(t *testing.T, ctx context.Context, owner *pgxpool.Pool, name string) {
	t.Helper()
	wait, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		// pg_stat_activity hides another role's wait_event without monitoring
		// privileges. pg_locks exposes the wait without widening runtime grants.
		if err := owner.QueryRow(wait, `SELECT EXISTS(SELECT 1 FROM pg_locks l
			JOIN pg_stat_activity a ON a.pid=l.pid
			WHERE a.application_name=$1 AND NOT l.granted)`, name).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-ticker.C:
		case <-wait.Done():
			t.Fatal("command did not reach the held database lock")
		}
	}
}
