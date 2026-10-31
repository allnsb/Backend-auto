//go:build integration

package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"odysseia.local/backend/internal/forum"
)

func testQueuedWriteAuthorization(t *testing.T, ctx context.Context, store *Forums, owner *pgxpool.Pool, actor, outsider string) {
	t.Helper()
	for _, command := range []string{"reply", "opening", "channel_create", "channel_send"} {
		t.Run(command, func(t *testing.T) {
			operator := NewForums(owner)
			board, err := operator.Provision(ctx, "queued authorization fixture", []string{actor})
			if err != nil {
				t.Fatal(err)
			}
			topic, err := store.CreateTopic(ctx, actor, board.ID, uuid.NewString(), forum.TopicDraft{Title: "private topic", Body: "opening", Mode: "threaded"})
			if err != nil {
				t.Fatal(err)
			}
			key := writeScope{streamID: topic.Topic.ID}
			execute := func(ctx context.Context, user string) error {
				_, err := store.Reply(ctx, user, strings.ToUpper(topic.Topic.ID), uuid.NewString(), forum.ReplyDraft{Body: "must recheck after waiting"})
				return err
			}
			switch command {
			case "opening":
				key = writeScope{forumID: board.ID}
				execute = func(ctx context.Context, user string) error {
					_, err := store.CreateTopic(ctx, user, strings.ToUpper(board.ID), uuid.NewString(), forum.TopicDraft{Title: "queued opening", Body: "must recheck after waiting", Mode: "threaded"})
					return err
				}
			case "channel_create":
				key = writeScope{forumID: board.ID}
				execute = func(ctx context.Context, user string) error {
					_, err := store.CreateChannel(ctx, user, board.ID, uuid.NewString(), forum.ChannelDraft{Title: "queued channel"})
					return err
				}
			case "channel_send":
				channel, err := store.CreateChannel(ctx, actor, board.ID, uuid.NewString(), forum.ChannelDraft{Title: "queued send"})
				if err != nil {
					t.Fatal(err)
				}
				key = writeScope{streamID: channel.ID}
				execute = func(ctx context.Context, user string) error {
					_, err := store.SendChannelMessage(ctx, user, channel.ID, uuid.NewString(), forum.ChannelMessageDraft{Body: "must recheck after waiting"})
					return err
				}
			}
			var holders []func()
			releaseHolders := func() {
				for i, release := range holders {
					if release != nil {
						release()
						holders[i] = nil
					}
				}
			}
			t.Cleanup(releaseHolders)
			// Occupy the configured local limit, then queue one real command.
			for range store.writes.perScope {
				release, err := store.writes.acquire(ctx, key)
				if err != nil {
					t.Fatal(err)
				}
				holders = append(holders, release)
			}
			// An outsider must receive the normal denial without entering the hot
			// queue. The uppercase input also exercises canonical counter keys.
			deniedCtx, stop := context.WithTimeout(ctx, 100*time.Millisecond)
			defer stop()
			if err := execute(deniedCtx, outsider); !errors.Is(err, forum.ErrNotFound) {
				t.Fatalf("private counter activity affected outsider denial: %v", err)
			}
			queuedCtx, cancel := context.WithCancel(ctx)
			t.Cleanup(cancel)
			result := make(chan error, 1)
			go func() { result <- execute(queuedCtx, actor) }()
			waitWriteReferences(t, store.writes, key, int(store.writes.perScope)+1)
			_, err = operator.RemoveMember(ctx, forum.MemberRemoval{OperationID: uuid.NewString(), ForumID: board.ID, UserID: actor, Operator: "integration fixture", Reason: "revoke after precheck, before transaction"})
			if err != nil {
				t.Fatal(err)
			}
			releaseHolders()
			select {
			case err := <-result:
				if !errors.Is(err, forum.ErrNotFound) {
					t.Fatalf("queued command reused stale authorization: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("queued command did not finish after release")
			}
			assertForumCount(t, ctx, owner, `SELECT count(*) FROM odysseia.topics WHERE forum_id=$1`, board.ID, 1)
			assertForumCount(t, ctx, owner, `SELECT count(*) FROM odysseia.messages WHERE stream_id=$1`, topic.Topic.ID, 1)
		})
	}
}
