package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"odysseia.local/backend/internal/blob"
	"odysseia.local/backend/internal/conversation"
	"odysseia.local/backend/internal/media"
	"odysseia.local/backend/internal/message"
)

// AttachmentSettings wires the process store and the budgets that may be lower
// than the database ceilings. Store is required. A nil store makes uploads fail
// closed instead of writing only a database row.
type AttachmentSettings struct {
	Store            blob.Store
	Limits           media.Limits
	UploadsPerMinute int
	QuotaBytes       int64
	PresignTTL       time.Duration
	MaxPerMessage    int
}

func (s *Forums) UseAttachments(settings AttachmentSettings) error {
	if settings.Store == nil {
		return errors.New("attachment store is required")
	}
	if err := settings.Limits.Valid(); err != nil {
		return err
	}
	if settings.UploadsPerMinute < 1 || settings.UploadsPerMinute > 60 {
		return errors.New("attachment upload rate must be from 1 to 60 per minute")
	}
	if settings.QuotaBytes < 1 || settings.QuotaBytes > 1<<30 {
		return errors.New("attachment quota must be from 1 byte to 1GiB")
	}
	if settings.PresignTTL < time.Second || settings.PresignTTL > 5*time.Minute {
		return errors.New("attachment presign ttl must be from 1s to 5m")
	}
	if settings.MaxPerMessage < 1 || settings.MaxPerMessage > conversation.MaxAttachmentsPerMessage {
		return errors.New("attachments per message must be from 1 to 4")
	}
	s.blobs = settings.Store
	s.mediaLimits = settings.Limits
	s.uploadsPerMinute = settings.UploadsPerMinute
	s.quotaBytes = settings.QuotaBytes
	s.presignTTL = settings.PresignTTL
	s.maxAttachments = settings.MaxPerMessage
	return nil
}

// UploadConversationAttachment re-checks the bytes, writes the object, then
// inserts a ready row for the uploader. The database row is the record that a
// later message may bind. A failed insert deletes the object; a failed delete
// can leave an unreferenced object until an operator removes it.
func (s *Forums) UploadConversationAttachment(ctx context.Context, actor, conversationID string, prepared media.Prepared) (message.Attachment, error) {
	if s.blobs == nil {
		return message.Attachment{}, errors.New("attachment storage is not configured")
	}
	if err := validConversationID(actor, conversationID); err != nil {
		return message.Attachment{}, err
	}
	if err := prepared.Check(s.mediaLimits); err != nil {
		return message.Attachment{}, err
	}
	checked := prepared
	if err := s.visibleConversation(ctx, actor, conversationID); err != nil {
		return message.Attachment{}, err
	}
	if err := s.rejectAttachmentBudget(ctx, actor, int64(len(checked.Bytes))); err != nil {
		return message.Attachment{}, err
	}
	attachmentID := uuid.NewString()
	key := "conversations/" + conversationID + "/" + attachmentID
	disposition, err := media.ContentDisposition(checked.ContentType)
	if err != nil {
		return message.Attachment{}, err
	}
	if err := s.blobs.Put(ctx, blob.Object{
		Key: key, ContentType: checked.ContentType, ContentDisposition: disposition, Body: checked.Bytes,
	}); err != nil {
		return message.Attachment{}, fmt.Errorf("store attachment: %w", err)
	}
	stored := message.Attachment{
		ID: attachmentID, ConversationID: conversationID, Kind: checked.Kind, ContentType: checked.ContentType,
		ByteSize: int64(len(checked.Bytes)), Width: checked.Width, Height: checked.Height, DurationMS: checked.DurationMS,
	}
	err = writeTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockAttachmentBudget(ctx, tx, actor); err != nil {
			return err
		}
		if err := s.attachmentBudget(ctx, tx, actor, stored.ByteSize); err != nil {
			return err
		}
		if err := s.visibleConversationTx(ctx, tx, actor, conversationID); err != nil {
			return err
		}
		return insertAttachment(ctx, tx, actor, key, checked.Sum[:], &stored)
	})
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if deleteErr := s.blobs.Delete(cleanup, key); deleteErr != nil {
			slog.ErrorContext(cleanup, "attachment object cleanup failed", "attachment_id", attachmentID, "error", deleteErr)
		}
		return message.Attachment{}, err
	}
	return stored, nil
}

// OpenConversationAttachment returns a short-lived GET URL for an active member.
// Another member can fetch the object only after it is bound to a message.
// The uploader can fetch their own ready object before sending. Non-members,
// removed members, and unknown ids are the same not-found result.
func (s *Forums) OpenConversationAttachment(ctx context.Context, actor, conversationID, attachmentID string) (string, error) {
	if s.blobs == nil {
		return "", errors.New("attachment storage is not configured")
	}
	if err := validConversationID(actor, conversationID, attachmentID); err != nil {
		return "", err
	}
	var key, contentType string
	err := s.pool.QueryRow(ctx, `SELECT a.object_key, a.content_type
		FROM odysseia.attachments a
		JOIN odysseia.conversations c ON c.id = a.conversation_id
		JOIN odysseia.conversation_members m ON m.conversation_id = c.id AND m.user_id = $3
		WHERE a.id = $1 AND a.conversation_id = $2 AND a.status = 'ready'
		  AND m.removed_at IS NULL AND c.dissolved_at IS NULL
		  AND (a.uploader_id = $3 OR EXISTS (
		      SELECT 1 FROM odysseia.message_attachments link WHERE link.attachment_id = a.id
		  ))`, attachmentID, conversationID, actor).Scan(&key, &contentType)
	if err != nil {
		return "", conversationError(err)
	}
	disposition, err := media.ContentDisposition(contentType)
	if err != nil {
		return "", err
	}
	signed, err := s.blobs.PresignGet(ctx, blob.Get{
		Key: key, ContentType: contentType, ContentDisposition: disposition, TTL: s.presignTTL,
	})
	if err != nil {
		return "", fmt.Errorf("presign attachment: %w", err)
	}
	return signed, nil
}

const attachmentUsageSQL = `SELECT COALESCE(sum(byte_size), 0),
	count(*) FILTER (WHERE created_at > transaction_timestamp() - interval '1 minute')
	FROM odysseia.attachments WHERE uploader_id = $1`

func (s *Forums) rejectAttachmentBudget(ctx context.Context, actor string, extra int64) error {
	return s.attachmentBudget(ctx, s.pool, actor, extra)
}

func lockAttachmentBudget(ctx context.Context, tx pgx.Tx, actor string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('odysseia-attachment-upload'), hashtext($1))`, actor)
	return err
}

func (s *Forums) attachmentBudget(ctx context.Context, q rowQueryer, actor string, extra int64) error {
	var total int64
	var recent int
	if err := q.QueryRow(ctx, attachmentUsageSQL, actor).Scan(&total, &recent); err != nil {
		return err
	}
	if recent >= s.uploadsPerMinute {
		return conversation.ErrRate
	}
	if total+extra > s.quotaBytes {
		return conversation.ErrQuota
	}
	return nil
}

func (s *Forums) visibleConversationTx(ctx context.Context, tx pgx.Tx, actor, id string) error {
	return visibleConversationQuery(ctx, tx, actor, id)
}

func insertAttachment(ctx context.Context, tx pgx.Tx, actor, key string, sum []byte, item *message.Attachment) error {
	var width, height, duration any
	if item.Kind == "image" {
		width = item.Width
		height = item.Height
	} else {
		duration = item.DurationMS
	}
	return tx.QueryRow(ctx, `INSERT INTO odysseia.attachments (
			id, conversation_id, uploader_id, kind, content_type, byte_size, width, height, duration_ms, object_key, sha256, status
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'ready')
		RETURNING created_at`,
		item.ID, item.ConversationID, actor, item.Kind, item.ContentType, item.ByteSize,
		width, height, duration, key, sum,
	).Scan(&item.CreatedAt)
}

// lockAttachments returns ready, unbound attachments owned by actor in this
// conversation, in the caller's order. Rows are locked by id so two sends
// cannot bind the same object. Any miss is one attachment error.
func lockAttachments(ctx context.Context, tx pgx.Tx, actor, conversationID string, ids []string) ([]message.Attachment, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT a.id::text, a.conversation_id::text, a.uploader_id::text, a.kind, a.content_type,
			a.byte_size, COALESCE(a.width, 0), COALESCE(a.height, 0), COALESCE(a.duration_ms, 0), a.status, a.created_at,
			EXISTS (SELECT 1 FROM odysseia.message_attachments link WHERE link.attachment_id = a.id)
		FROM odysseia.attachments a
		WHERE a.id = ANY($1::uuid[])
		ORDER BY a.id
		FOR UPDATE OF a`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := make(map[string]lockedAttachment, len(ids))
	for rows.Next() {
		var item lockedAttachment
		if err := rows.Scan(&item.view.ID, &item.view.ConversationID, &item.uploader, &item.view.Kind, &item.view.ContentType,
			&item.view.ByteSize, &item.view.Width, &item.view.Height, &item.view.DurationMS, &item.status, &item.view.CreatedAt, &item.used); err != nil {
			return nil, err
		}
		found[item.view.ID] = item
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ordered := make([]message.Attachment, 0, len(ids))
	for _, id := range ids {
		item, ok := found[id]
		if !ok || item.used || item.status != "ready" || item.uploader != actor || item.view.ConversationID != conversationID {
			return nil, conversation.ErrAttachment
		}
		ordered = append(ordered, item.view)
	}
	return ordered, nil
}

type lockedAttachment struct {
	view     message.Attachment
	uploader string
	status   string
	used     bool
}

func fillAttachments(ctx context.Context, q rowQueryer, items []message.Message) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]string, len(items))
	for index := range items {
		ids[index] = items[index].ID
	}
	rows, err := q.Query(ctx, `SELECT link.message_id::text, a.id::text, a.conversation_id::text, a.kind, a.content_type,
			a.byte_size, COALESCE(a.width, 0), COALESCE(a.height, 0), COALESCE(a.duration_ms, 0), a.created_at
		FROM odysseia.message_attachments link
		JOIN odysseia.attachments a ON a.id = link.attachment_id
		WHERE link.message_id = ANY($1::uuid[])
		ORDER BY link.message_id, link.position`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	grouped := map[string][]message.Attachment{}
	for rows.Next() {
		var messageID string
		var item message.Attachment
		if err := rows.Scan(&messageID, &item.ID, &item.ConversationID, &item.Kind, &item.ContentType,
			&item.ByteSize, &item.Width, &item.Height, &item.DurationMS, &item.CreatedAt); err != nil {
			return err
		}
		grouped[messageID] = append(grouped[messageID], item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for index := range items {
		items[index].Attachments = grouped[items[index].ID]
	}
	return nil
}
