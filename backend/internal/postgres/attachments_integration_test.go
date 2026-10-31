//go:build integration

package postgres

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"odysseia.local/backend/internal/blob"
	"odysseia.local/backend/internal/conversation"
	"odysseia.local/backend/internal/identity"
	"odysseia.local/backend/internal/media"
)

func testAttachments(t *testing.T, ctx context.Context, migrationURL string, runtimePool *pgxpool.Pool) {
	t.Helper()
	owner, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := owner.Exec(ctx, `GRANT SELECT, INSERT ON odysseia.attachments, odysseia.message_attachments TO odysseia_runtime`); err != nil {
		t.Fatal(err)
	}
	users := NewUsers(owner)
	alice, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://p4b.example", Subject: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://p4b.example", Subject: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	cara, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://p4b.example", Subject: "cara"})
	if err != nil {
		t.Fatal(err)
	}
	dave, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://p4b.example", Subject: "dave"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewForums(owner).Provision(ctx, "attachment forum", []string{alice.ID, bob.ID, dave.ID}); err != nil {
		t.Fatal(err)
	}
	store, err := NewCachedForums(runtimePool, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	memory := blob.NewMemory()
	picture := mustIntegrationJPEG(t)
	limits := media.DefaultLimits()
	prepared, err := media.Inspect(picture, 0, limits)
	if err != nil {
		t.Fatal(err)
	}
	limits.MaxBytes = int64(len(prepared.Bytes))
	if err := store.UseAttachments(AttachmentSettings{
		Store: memory, Limits: limits, UploadsPerMinute: 10, QuotaBytes: int64(len(prepared.Bytes)),
		PresignTTL: time.Minute, MaxPerMessage: 4,
	}); err != nil {
		t.Fatal(err)
	}
	dm, _, err := store.OpenDM(ctx, alice.ID, uuid.NewString(), conversation.DMDraft{UserID: bob.ID})
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := store.OpenDM(ctx, alice.ID, uuid.NewString(), conversation.DMDraft{UserID: dave.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UploadConversationAttachment(ctx, cara.ID, dm.ID, prepared); !errors.Is(err, conversation.ErrNotFound) || memory.Puts != 0 {
		t.Fatalf("non-member upload err=%v puts=%d", err, memory.Puts)
	}
	if _, err := store.UploadConversationAttachment(ctx, alice.ID, dm.ID, media.Prepared{Bytes: []byte("<svg xmlns='http://www.w3.org/2000/svg'/>")}); !errors.Is(err, media.ErrType) {
		t.Fatalf("svg upload err=%v", err)
	}
	huge := bytes.Repeat(picture, 2)
	if _, err := store.UploadConversationAttachment(ctx, alice.ID, dm.ID, media.Prepared{Bytes: huge}); !errors.Is(err, media.ErrTooLarge) {
		t.Fatalf("oversize upload err=%v", err)
	}
	uploaded, err := store.UploadConversationAttachment(ctx, alice.ID, dm.ID, prepared)
	if err != nil || uploaded.Kind != "image" || uploaded.ByteSize != int64(len(prepared.Bytes)) {
		t.Fatalf("upload %+v err=%v", uploaded, err)
	}
	if _, err := store.UploadConversationAttachment(ctx, alice.ID, dm.ID, prepared); !errors.Is(err, conversation.ErrQuota) || memory.Len() != 1 {
		t.Fatalf("quota err=%v stored=%d", err, memory.Len())
	}
	if _, err := store.OpenConversationAttachment(ctx, cara.ID, dm.ID, uploaded.ID); !errors.Is(err, conversation.ErrNotFound) {
		t.Fatalf("non-member download err=%v", err)
	}
	if _, err := store.OpenConversationAttachment(ctx, bob.ID, dm.ID, uploaded.ID); !errors.Is(err, conversation.ErrNotFound) {
		t.Fatalf("unbound peer download err=%v", err)
	}
	aliceURL, err := store.OpenConversationAttachment(ctx, alice.ID, dm.ID, uploaded.ID)
	if err != nil || !strings.Contains(aliceURL, uploaded.ID) {
		t.Fatalf("uploader download %s err=%v", aliceURL, err)
	}
	if _, err := store.SendConversationMessage(ctx, bob.ID, dm.ID, uuid.NewString(), conversation.MessageDraft{AttachmentIDs: []string{uploaded.ID}}); !errors.Is(err, conversation.ErrAttachment) {
		t.Fatalf("peer bind err=%v", err)
	}
	if _, err := store.SendConversationMessage(ctx, alice.ID, other.ID, uuid.NewString(), conversation.MessageDraft{AttachmentIDs: []string{uploaded.ID}}); !errors.Is(err, conversation.ErrAttachment) {
		t.Fatalf("cross-conversation bind err=%v", err)
	}
	if _, err := store.SendConversationMessage(ctx, alice.ID, dm.ID, uuid.NewString(), conversation.MessageDraft{}); !errors.Is(err, conversation.ErrInvalid) {
		t.Fatalf("empty text err=%v", err)
	}
	key := uuid.NewString()
	draft := conversation.MessageDraft{AttachmentIDs: []string{uploaded.ID}}
	sent, err := store.SendConversationMessage(ctx, alice.ID, dm.ID, key, draft)
	if err != nil || sent.Body != "" || len(sent.Attachments) != 1 || sent.Attachments[0].ID != uploaded.ID {
		t.Fatalf("sent=%+v err=%v", sent, err)
	}
	replay, err := store.SendConversationMessage(ctx, alice.ID, dm.ID, key, draft)
	if err != nil || replay.ID != sent.ID || len(replay.Attachments) != 1 {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if _, err := store.SendConversationMessage(ctx, alice.ID, dm.ID, uuid.NewString(), draft); !errors.Is(err, conversation.ErrAttachment) {
		t.Fatalf("second bind err=%v", err)
	}
	bobURL, err := store.OpenConversationAttachment(ctx, bob.ID, dm.ID, uploaded.ID)
	if err != nil || !strings.Contains(bobURL, uploaded.ID) {
		t.Fatalf("member download %s err=%v", bobURL, err)
	}
	if _, err := store.OpenConversationAttachment(ctx, cara.ID, dm.ID, uploaded.ID); !errors.Is(err, conversation.ErrNotFound) {
		t.Fatalf("non-member download after bind err=%v", err)
	}
	page, err := store.ListConversationMessages(ctx, bob.ID, dm.ID, 0, 10)
	if err != nil || len(page) != 1 || len(page[0].Attachments) != 1 || page[0].Attachments[0].ID != uploaded.ID {
		t.Fatalf("history=%+v err=%v", page, err)
	}
	var payload string
	if err := owner.QueryRow(ctx, `SELECT payload#>>'{data,data}' FROM odysseia.realtime_outbox
		WHERE payload#>>'{data,type}'='conversation.message.created' AND payload#>>'{data,scope_id}'=$1
		ORDER BY id DESC LIMIT 1`, dm.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, uploaded.ID) || strings.Contains(payload, "X-Amz-") || strings.Contains(payload, "blob.invalid") {
		t.Fatalf("event leaked a download url or dropped the attachment: %s", payload)
	}
	assertAttachmentConstraint(t, ctx, owner, alice.ID)
}

func assertAttachmentConstraint(t *testing.T, ctx context.Context, owner *pgxpool.Pool, author string) {
	t.Helper()
	stream := uuid.NewString()
	messageID := uuid.NewString()
	if _, err := owner.Exec(ctx, `INSERT INTO odysseia.message_streams(id, kind, last_position) VALUES ($1, 'topic', 1)`, stream); err != nil {
		t.Fatal(err)
	}
	_, err := owner.Exec(ctx, `INSERT INTO odysseia.messages(id, stream_id, position, author_id, body) VALUES ($1, $2, 1, $3, '')`, messageID, stream, author)
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != "23514" {
		t.Fatalf("empty forum body err=%v", err)
	}
	if _, err := owner.Exec(ctx, `INSERT INTO odysseia.messages(id, stream_id, position, author_id, body) VALUES ($1, $2, 1, $3, 'kept')`, messageID, stream, author); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `DELETE FROM odysseia.messages WHERE id=$1`, messageID); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `DELETE FROM odysseia.message_streams WHERE id=$1`, stream); err != nil {
		t.Fatal(err)
	}
	conversationStream := uuid.NewString()
	if _, err := owner.Exec(ctx, `INSERT INTO odysseia.message_streams(id, kind, last_position) VALUES ($1, 'conversation', 1)`, conversationStream); err != nil {
		t.Fatal(err)
	}
	_, err = owner.Exec(ctx, `INSERT INTO odysseia.messages(id, stream_id, position, author_id, body) VALUES ($1, $2, 1, $3, '')`, uuid.NewString(), conversationStream, author)
	if !errors.As(err, &pgError) || pgError.Code != "23514" {
		t.Fatalf("empty conversation body without attachment err=%v", err)
	}
	if _, err := owner.Exec(ctx, `DELETE FROM odysseia.message_streams WHERE id=$1`, conversationStream); err != nil {
		t.Fatal(err)
	}
}

func mustIntegrationJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
