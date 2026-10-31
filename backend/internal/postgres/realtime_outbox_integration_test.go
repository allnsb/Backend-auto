//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testRealtimeCommandGuards(t *testing.T, ctx context.Context, owner *pgxpool.Pool) {
	t.Helper()
	id, scope := uuid.NewString(), uuid.NewString()
	valid := map[string]any{"channel": "topic:" + scope, "idempotency_key": id, "data": map[string]any{"id": id, "version": 1, "type": "forum.message.created", "scope_id": scope, "position": "1", "data": map[string]any{"body": "synthetic body"}}}
	encoded, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	var pending int64
	if err := owner.QueryRow(ctx, "SELECT count(*) FROM odysseia.realtime_outbox").Scan(&pending); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, method, payload string
	}{
		{"unsupported command", "broadcast", string(encoded)},
		{"missing fields", "publish", `{}`},
		{"null payload", "publish", `null`},
		{"foreign namespace", "publish", replaceCommandField(t, encoded, "channel", "foreign:"+scope)},
		{"scope mismatch", "publish", replaceCommandField(t, encoded, "channel", "topic:"+uuid.NewString())},
		{"missing idempotency", "publish", replaceCommandField(t, encoded, "idempotency_key", nil)},
		{"changed event ID", "publish", replaceCommandField(t, encoded, "idempotency_key", uuid.NewString())},
		{"missing disconnect", "disconnect", `{"user":"` + scope + `"}`},
		{"reserved disconnect code", "disconnect", `{"user":"` + scope + `","disconnect":{"code":3000,"reason":"permissions changed"}}`},
	}
	for _, test := range cases {
		_, err := owner.Exec(ctx, `INSERT INTO odysseia.realtime_outbox(method,payload) VALUES ($1,$2)`, test.method, test.payload)
		var constraint *pgconn.PgError
		if !errors.As(err, &constraint) || constraint.Code != "23514" {
			t.Fatalf("%s was not rejected by an outbox constraint", test.name)
		}
	}
	// Routing is generated, so even an owner cannot insert a caller-selected
	// data partition. Invalid envelopes above still hit their own constraints.
	_, err = owner.Exec(ctx, `INSERT INTO odysseia.realtime_outbox(method,payload,partition) VALUES ('publish',$1,42)`, encoded)
	var generated *pgconn.PgError
	if !errors.As(err, &generated) || generated.Code != "428C9" {
		t.Fatal("partition routing was caller controlled")
	}
	var remaining int64
	if err := owner.QueryRow(ctx, "SELECT count(*) FROM odysseia.realtime_outbox").Scan(&remaining); err != nil || remaining != pending {
		t.Fatal("invalid commands changed the pending queue")
	}
	testOutboxMigrationBarrier(t, ctx, owner)
	testOutboxPollingMigration(t, ctx, owner)
	testP3MigrationRoundTrip(t, ctx, owner)
}

func testP3MigrationRoundTrip(t *testing.T, ctx context.Context, owner *pgxpool.Pool) {
	t.Helper()
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	scope := uuid.NewString()
	if err := enqueueForumEvent(ctx, tx, "topic:"+scope, "forum.message.created", scope, 1, map[string]string{"body": "P3 migration preserves the pending envelope"}); err != nil {
		t.Fatal(err)
	}
	var id int64
	var partition int
	var payload []byte
	if err := tx.QueryRow(ctx, `SELECT id,partition,payload FROM odysseia.realtime_outbox WHERE payload#>>'{data,scope_id}'=$1`, scope).Scan(&id, &partition, &payload); err != nil {
		t.Fatal(err)
	}
	source, err := migrations.ReadFile("migrations/00008_discussion_modes_and_channels.sql")
	if err != nil {
		t.Fatal(err)
	}
	later, err := migrations.ReadFile("migrations/00010_conversations.sql")
	if err != nil {
		t.Fatal(err)
	}
	attachments, err := migrations.ReadFile("migrations/00011_attachments.sql")
	if err != nil {
		t.Fatal(err)
	}
	// 00011 references conversations, and 00010 owns the conversation foreign key
	// on the stream kind, so both leave before 00008 can drop that key.
	p3 := strings.Split(string(source), "-- +goose Down")
	p4 := strings.Split(string(later), "-- +goose Down")
	p4b := strings.Split(string(attachments), "-- +goose Down")
	for _, direction := range []string{p4b[1], p4[1], p3[1], p3[0], p4[0], p4b[0]} {
		if _, err := tx.Exec(ctx, direction); err != nil {
			t.Fatal(err)
		}
	}
	var preserved bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM odysseia.realtime_outbox WHERE id=$1 AND partition=$2 AND payload=$3::jsonb)`, id, partition, payload).Scan(&preserved); err != nil || !preserved {
		t.Fatalf("P3 migration changed a pending envelope: %v", err)
	}
}

// Removing optional wake-ups must not rewrite routing or discard a backlog.
// Exercise both directions inside a rolled-back transaction, including the
// actual trigger/function removal; delivery itself is checked by native tests.
func testOutboxPollingMigration(t *testing.T, ctx context.Context, owner *pgxpool.Pool) {
	t.Helper()
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	scope := uuid.NewString()
	if err := enqueueForumEvent(ctx, tx, "topic:"+scope, "forum.message.created", scope, 1, map[string]string{"body": "backlog survives polling migration"}); err != nil {
		t.Fatal(err)
	}
	var id int64
	var partition int
	var payload []byte
	err = tx.QueryRow(ctx, `SELECT id,partition,payload FROM odysseia.realtime_outbox WHERE payload#>>'{data,scope_id}'=$1`, scope).Scan(&id, &partition, &payload)
	if err != nil {
		t.Fatal(err)
	}
	source, err := migrations.ReadFile("migrations/00007_remove_outbox_notifications.sql")
	if err != nil {
		t.Fatal(err)
	}
	directions := strings.Split(string(source), "-- +goose Down")
	for _, migration := range []string{directions[1], directions[0]} {
		if _, err := tx.Exec(ctx, migration); err != nil {
			t.Fatal(err)
		}
	}
	var preserved, removed bool
	err = tx.QueryRow(ctx, `SELECT
		EXISTS(SELECT FROM odysseia.realtime_outbox WHERE id=$1 AND partition=$2 AND payload#>>'{data,scope_id}'=$3 AND payload=$4::jsonb),
		NOT EXISTS(SELECT FROM pg_trigger WHERE tgrelid='odysseia.realtime_outbox'::regclass AND tgname='notify_realtime_partition')
		AND to_regprocedure('odysseia.notify_realtime_partition()') IS NULL`, id, partition, scope, payload).Scan(&preserved, &removed)
	if err != nil || !preserved || !removed {
		t.Fatalf("polling migration changed pending data or retained the trigger: %v", err)
	}
}

func testOutboxMigrationBarrier(t *testing.T, ctx context.Context, owner *pgxpool.Pool) {
	t.Helper()
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	scope := uuid.NewString()
	if err := enqueueForumEvent(ctx, tx, "topic:"+scope, "forum.message.created", scope, 1, map[string]string{"body": "pending migration guard"}); err != nil {
		t.Fatal(err)
	}
	source, err := migrations.ReadFile("migrations/00006_partition_realtime_outbox.sql")
	if err != nil {
		t.Fatal(err)
	}
	up := strings.Split(string(source), "-- +goose Down")[0]
	_, err = tx.Exec(ctx, up)
	var blocked *pgconn.PgError
	if !errors.As(err, &blocked) || blocked.Code != "P0001" || !strings.Contains(blocked.Message, "drain the realtime outbox") {
		t.Fatal("partition migration did not refuse a pending backlog before changing routing")
	}
}

func replaceCommandField(t *testing.T, data []byte, field string, value any) string {
	t.Helper()
	var command map[string]any
	if err := json.Unmarshal(data, &command); err != nil {
		t.Fatal(err)
	}
	command[field] = value
	result, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	return string(result)
}
