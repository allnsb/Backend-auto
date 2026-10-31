//go:build integration && realtime && cluster

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"odysseia.local/backend/internal/forum"
)

// Hold acknowledgement of one real data partition. A different stream and
// control-plane revocation must progress while the held command remains queued.
func TestRealtimePartitionIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	f := newClusterFixture(t, ctx)
	firstScope := uuid.MustParse(f.topic.Topic.ID)
	var independent forum.CreatedTopic
	for range 32 {
		candidate, err := f.store.CreateTopic(ctx, f.bob.ID, f.board.ID, uuid.NewString(), forum.TopicDraft{Title: "other data partition", Body: "opening", Mode: "threaded"})
		if err != nil {
			t.Fatal(err)
		}
		id := uuid.MustParse(candidate.Topic.ID)
		if id[15]%4 != firstScope[15]%4 {
			independent = candidate
			break
		}
	}
	if independent.Topic.ID == "" {
		t.Fatal("fixture did not produce a distinct data partition")
	}
	first, err := f.store.Reply(ctx, f.bob.ID, f.topic.Topic.ID, uuid.NewString(), forum.ReplyDraft{Body: "hold this acknowledgement"})
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	var commandID int64
	err = blocker.QueryRow(ctx, `SELECT id FROM odysseia.realtime_outbox WHERE payload#>>'{data,data,id}'=$1 FOR UPDATE`, first.ID).Scan(&commandID)
	if err != nil {
		t.Fatal(err)
	}
	node := startRealtimeProcess(t, ctx, f.consumerURL, f.connectionKey, f.proxyKey, f.server.URL+"/_internal/realtime/subscribe", f.broker)
	probe, err := Open(ctx, f.consumerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(probe.Close)
	observeCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting int
		err := probe.QueryRow(observeCtx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND usename=current_user AND wait_event_type='Lock' AND lower(query) LIKE '%delete%'`).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case <-ticker.C:
		case <-observeCtx.Done():
			t.Fatal("consumer did not reach the held delete")
		}
	}
	bobA := newClusterPeer(t, node, f.connectionToken(t, f.bob), "topic:"+f.topic.Topic.ID, nil)
	awaitRealtime(t, ctx, bobA.ready, "blocked partition subscribed")
	bobB := newClusterPeer(t, node, f.connectionToken(t, f.bob), "topic:"+independent.Topic.ID, nil)
	awaitRealtime(t, ctx, bobB.ready, "independent partition subscribed")
	alice := newClusterPeer(t, node, f.connectionToken(t, f.alice), "topic:"+f.topic.Topic.ID, nil)
	awaitRealtime(t, ctx, alice.ready, "revocable subscriber connected")
	blocked, err := f.store.Reply(ctx, f.bob.ID, f.topic.Topic.ID, uuid.NewString(), forum.ReplyDraft{Body: "wait behind the held command"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.store.Reply(ctx, f.bob.ID, independent.Topic.ID, uuid.NewString(), forum.ReplyDraft{Body: "unrelated partition progresses"})
	if err != nil {
		t.Fatal(err)
	}
	progressCtx, finish := context.WithTimeout(ctx, 2*time.Second)
	defer finish()
	readClusterMessages(t, progressCtx, bobB, []forum.Entry{other})
	_, err = NewForums(f.owner).RemoveMember(ctx, forum.MemberRemoval{OperationID: uuid.NewString(), ForumID: f.board.ID, UserID: f.alice.ID, Operator: "partition fixture", Reason: "control traffic must not wait for a data partition"})
	if err != nil {
		t.Fatal(err)
	}
	awaitRealtime(t, progressCtx, alice.revoked, "control partition revokes before data acknowledgement")
	select {
	case <-bobA.messages:
		t.Fatal("held data partition published its next command early")
	default:
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	readClusterMessages(t, ctx, bobA, []forum.Entry{blocked})
	waitOutboxEmpty(t, ctx, f.owner)
}
