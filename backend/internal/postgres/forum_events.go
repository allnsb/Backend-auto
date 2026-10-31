package postgres

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const insertForumEventSQL = `INSERT INTO odysseia.realtime_outbox(method,payload) VALUES ('publish',$1)`

// enqueueForumEvent persists the provider command with the business transaction.
// The pending command keeps its event ID through broker restarts; broker deduplication is temporary.
// PostgreSQL generates stable per-scope routing; writers never select partitions.
func enqueueForumEvent(ctx context.Context, tx pgx.Tx, channel, name, scopeID string, position int64, data any) error {
	payload, err := forumEventPayload(channel, name, scopeID, position, data)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, insertForumEventSQL, payload)
	return err
}

// forumEventPayload encodes the existing provider envelope; callers persist it
// in the same transaction as the business record, individually or in a batch.
func forumEventPayload(channel, name, scopeID string, position int64, data any) ([]byte, error) {
	id := uuid.NewString()
	event := struct {
		ID       string `json:"id"`
		Version  int    `json:"version"`
		Type     string `json:"type"`
		ScopeID  string `json:"scope_id"`
		Position int64  `json:"position,string"`
		Data     any    `json:"data"`
	}{ID: id, Version: 1, Type: name, ScopeID: scopeID, Position: position, Data: data}
	return json.Marshal(struct {
		Channel        string `json:"channel"`
		Data           any    `json:"data"`
		IdempotencyKey string `json:"idempotency_key"`
	}{Channel: channel, Data: event, IdempotencyKey: id})
}

// enqueueForumDisconnect targets every connection of this user. Reconnection must
// recheck current membership; committing this command does not confirm client departure.
func enqueueForumDisconnect(ctx context.Context, tx pgx.Tx, userID string) error {
	payload, err := json.Marshal(map[string]any{
		"user":       userID,
		"disconnect": map[string]any{"code": 4001, "reason": "permissions changed"},
	})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO odysseia.realtime_outbox(method,payload) VALUES ('disconnect',$1)`, payload)
	return err
}
