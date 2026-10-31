//go:build integration

package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"odysseia.local/backend/internal/blob"
	"odysseia.local/backend/internal/config"
	"odysseia.local/backend/internal/conversation"
	"odysseia.local/backend/internal/identity"
	"odysseia.local/backend/internal/media"
)

func testAttachmentSweep(t *testing.T, ctx context.Context, migrationURL string, runtimePool *pgxpool.Pool) {
	t.Helper()
	owner, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := owner.Exec(ctx, `GRANT SELECT, INSERT, UPDATE, DELETE ON odysseia.attachments TO odysseia_runtime;
		GRANT SELECT, INSERT ON odysseia.message_attachments TO odysseia_runtime`); err != nil {
		t.Fatal(err)
	}
	users := NewUsers(owner)
	alice, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://sweep.example", Subject: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://sweep.example", Subject: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewForums(owner).Provision(ctx, "sweep forum", []string{alice.ID, bob.ID}); err != nil {
		t.Fatal(err)
	}
	store, err := NewCachedForums(runtimePool, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	memory := blob.NewMemory()
	prepared, err := media.Inspect(mustIntegrationJPEG(t), 0, media.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	limits := media.DefaultLimits()
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

	unbound, err := store.UploadConversationAttachment(ctx, alice.ID, dm.ID, prepared)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UploadConversationAttachment(ctx, alice.ID, dm.ID, prepared); !errors.Is(err, conversation.ErrQuota) {
		t.Fatalf("quota before sweep err=%v", err)
	}
	unboundKey := attachmentObjectKey(t, ctx, owner, unbound.ID)
	ageAttachment(t, ctx, owner, unbound.ID)
	if result := mustSweep(t, ctx, runtimePool, memory); result.DeletedRows != 1 || result.ExpiredRows != 0 || attachmentExists(t, ctx, owner, unbound.ID) || objectPresent(memory, unboundKey) {
		t.Fatalf("unbound sweep %+v exists=%v object=%v", result, attachmentExists(t, ctx, owner, unbound.ID), objectPresent(memory, unboundKey))
	}

	bound, err := store.UploadConversationAttachment(ctx, alice.ID, dm.ID, prepared)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UploadConversationAttachment(ctx, alice.ID, dm.ID, prepared); !errors.Is(err, conversation.ErrQuota) {
		t.Fatalf("quota while bound object is stored err=%v", err)
	}
	sent, err := store.SendConversationMessage(ctx, alice.ID, dm.ID, uuid.NewString(), conversation.MessageDraft{AttachmentIDs: []string{bound.ID}})
	if err != nil || sent.Body != "" || len(sent.Attachments) != 1 || sent.Attachments[0].Expired {
		t.Fatalf("sent=%+v err=%v", sent, err)
	}
	if _, err := runtimePool.Exec(ctx, `DELETE FROM odysseia.message_attachments WHERE attachment_id=$1`, bound.ID); !isPrivilege(err) {
		t.Fatalf("runtime deleted a message link: %v", err)
	}
	boundKey := attachmentObjectKey(t, ctx, owner, bound.ID)
	ageAttachment(t, ctx, owner, bound.ID)
	if result := mustSweep(t, ctx, runtimePool, memory); result.DeletedRows != 0 || result.ExpiredRows != 1 || !attachmentExists(t, ctx, owner, bound.ID) || objectPresent(memory, boundKey) {
		t.Fatalf("bound sweep %+v", result)
	}
	var status string
	var expiredAt bool
	var links, bodyLength int
	if err := owner.QueryRow(ctx, `SELECT a.status, a.expired_at IS NOT NULL,
			(SELECT count(*) FROM odysseia.message_attachments link WHERE link.attachment_id=a.id),
			(SELECT octet_length(body) FROM odysseia.messages WHERE id=$2)
		FROM odysseia.attachments a WHERE a.id=$1`, bound.ID, sent.ID).Scan(&status, &expiredAt, &links, &bodyLength); err != nil {
		t.Fatal(err)
	}
	if status != "expired" || !expiredAt || links != 1 || bodyLength != 0 {
		t.Fatalf("status=%s expired_at=%v links=%d body=%d", status, expiredAt, links, bodyLength)
	}
	if _, err := store.OpenConversationAttachment(ctx, bob.ID, dm.ID, bound.ID); !errors.Is(err, conversation.ErrNotFound) {
		t.Fatalf("expired download err=%v", err)
	}
	page, err := store.ListConversationMessages(ctx, bob.ID, dm.ID, 0, 10)
	if err != nil || len(page) != 1 || page[0].Body != "" || len(page[0].Attachments) != 1 || page[0].Attachments[0].ID != bound.ID || !page[0].Attachments[0].Expired {
		t.Fatalf("history=%+v err=%v", page, err)
	}
	young, err := store.UploadConversationAttachment(ctx, alice.ID, dm.ID, prepared)
	if err != nil {
		t.Fatalf("upload after expiry err=%v", err)
	}
	if _, err := store.UploadConversationAttachment(ctx, alice.ID, dm.ID, prepared); !errors.Is(err, conversation.ErrQuota) {
		t.Fatalf("young object must still consume quota, err=%v", err)
	}
	deletes := memory.Deletes
	if result := mustSweep(t, ctx, runtimePool, memory); result.RemovedObjects != 0 || result.DeletedRows != 0 || result.ExpiredRows != 0 || memory.Deletes != deletes {
		t.Fatalf("fresh sweep %+v deletes=%d want %d", result, memory.Deletes, deletes)
	}
	youngKey := attachmentObjectKey(t, ctx, owner, young.ID)
	if _, err := store.OpenConversationAttachment(ctx, alice.ID, dm.ID, young.ID); err != nil || !objectPresent(memory, youngKey) {
		t.Fatalf("young download err=%v present=%v", err, objectPresent(memory, youngKey))
	}
	assertExpiryMigrationRefuses(t, ctx, owner)

	first, firstKey := insertAgedAttachment(t, ctx, owner, memory, dm.ID, alice.ID)
	second, secondKey := insertAgedAttachment(t, ctx, owner, memory, dm.ID, alice.ID)
	third, thirdKey := insertAgedAttachment(t, ctx, owner, memory, dm.ID, alice.ID)
	result, err := sweepAttachments(ctx, runtimePool, memory, config.DefaultAttachmentRetention, 2)
	if err != nil || result.DeletedRows != 3 || result.ExpiredRows != 0 {
		t.Fatalf("batched sweep %+v err=%v", result, err)
	}
	for _, item := range []struct{ id, key string }{{first, firstKey}, {second, secondKey}, {third, thirdKey}} {
		if attachmentExists(t, ctx, owner, item.id) || objectPresent(memory, item.key) {
			t.Fatalf("batched row %s remained", item.id)
		}
	}

	gone, goneKey := insertAgedAttachment(t, ctx, owner, memory, dm.ID, alice.ID)
	kept, keptKey := insertAgedAttachment(t, ctx, owner, memory, dm.ID, alice.ID)
	refusing := &refuseKeys{Memory: memory, refuse: map[string]bool{keptKey: true}}
	result, err = sweepAttachments(ctx, runtimePool, refusing, config.DefaultAttachmentRetention, 10)
	if err == nil || !strings.Contains(err.Error(), "could not delete 1") || result.DeletedRows != 1 || attachmentExists(t, ctx, owner, gone) || !attachmentExists(t, ctx, owner, kept) || !objectPresent(memory, keptKey) || objectPresent(memory, goneKey) {
		t.Fatalf("partial %+v err=%v gone=%v kept=%v", result, err, attachmentExists(t, ctx, owner, gone), attachmentExists(t, ctx, owner, kept))
	}
	if result = mustSweep(t, ctx, runtimePool, memory); result.DeletedRows != 1 || attachmentExists(t, ctx, owner, kept) || objectPresent(memory, keptKey) {
		t.Fatalf("retry %+v kept=%v", result, attachmentExists(t, ctx, owner, kept))
	}

	orphan, orphanKey := insertAgedAttachment(t, ctx, owner, memory, dm.ID, alice.ID)
	if err := memory.Delete(ctx, orphanKey); err != nil {
		t.Fatal(err)
	}
	if result = mustSweep(t, ctx, runtimePool, memory); result.DeletedRows != 1 || attachmentExists(t, ctx, owner, orphan) {
		t.Fatalf("retry after object was already gone %+v exists=%v", result, attachmentExists(t, ctx, owner, orphan))
	}
	if _, err := store.OpenConversationAttachment(ctx, alice.ID, dm.ID, young.ID); err != nil || !objectPresent(memory, youngKey) {
		t.Fatalf("young attachment changed err=%v", err)
	}
}

func mustSweep(t *testing.T, ctx context.Context, pool *pgxpool.Pool, store blob.Store) AttachmentSweepResult {
	t.Helper()
	result, err := SweepAttachments(ctx, pool, store, config.DefaultAttachmentRetention)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func ageAttachment(t *testing.T, ctx context.Context, owner *pgxpool.Pool, id string) {
	t.Helper()
	tag, err := owner.Exec(ctx, `UPDATE odysseia.attachments
		SET created_at = transaction_timestamp() - interval '8 days' WHERE id=$1`, id)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("age attachment affected=%d err=%v", tag.RowsAffected(), err)
	}
}

func attachmentObjectKey(t *testing.T, ctx context.Context, owner *pgxpool.Pool, id string) string {
	t.Helper()
	var key string
	if err := owner.QueryRow(ctx, `SELECT object_key FROM odysseia.attachments WHERE id=$1`, id).Scan(&key); err != nil {
		t.Fatal(err)
	}
	return key
}

func attachmentExists(t *testing.T, ctx context.Context, owner *pgxpool.Pool, id string) bool {
	t.Helper()
	var exists bool
	if err := owner.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM odysseia.attachments WHERE id=$1)`, id).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

func objectPresent(store *blob.Memory, key string) bool {
	_, err := store.PresignGet(context.Background(), blob.Get{Key: key, TTL: time.Second})
	return err == nil
}

func insertAgedAttachment(t *testing.T, ctx context.Context, owner *pgxpool.Pool, store *blob.Memory, conversationID, uploader string) (string, string) {
	t.Helper()
	id := uuid.NewString()
	key := "conversations/" + conversationID + "/" + id
	_, err := owner.Exec(ctx, `INSERT INTO odysseia.attachments (
			id, conversation_id, uploader_id, kind, content_type, byte_size, width, height,
			object_key, sha256, status, created_at
		) VALUES (
			$1, $2, $3, 'image', 'image/jpeg', 4, 2, 2,
			$4, decode(repeat('ab', 32), 'hex'), 'ready',
			transaction_timestamp() - interval '8 days'
		)`, id, conversationID, uploader, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, blob.Object{Key: key, ContentType: "image/jpeg", Body: []byte("jpeg")}); err != nil {
		t.Fatal(err)
	}
	return id, key
}

func isPrivilege(err error) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "42501"
}

func assertExpiryMigrationRefuses(t *testing.T, ctx context.Context, owner *pgxpool.Pool) {
	t.Helper()
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	source, err := migrations.ReadFile("migrations/00012_attachment_expiry.sql")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(source), "-- +goose Down")
	if len(parts) != 2 {
		t.Fatal("expiry migration is missing its down section")
	}
	if _, err := tx.Exec(ctx, parts[1]); err == nil || !strings.Contains(err.Error(), "expired attachment rows exist") {
		t.Fatalf("down migration err=%v", err)
	}
}

type refuseKeys struct {
	*blob.Memory
	refuse map[string]bool
}

func (r *refuseKeys) Delete(ctx context.Context, key string) error {
	if r.refuse[key] {
		return errors.New("object delete refused")
	}
	return r.Memory.Delete(ctx, key)
}
