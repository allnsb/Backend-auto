package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"odysseia.local/backend/internal/blob"
)

const (
	// Rows stay locked until this batch's sequential object deletes finish.
	// A network stall can extend the transaction; the batch is a row-count cap,
	// not a guarantee of short lock duration.
	// R2 accepts a single DeleteObject that is already gone; DeleteObjects is
	// not used because aws-sdk-go-v2 attaches a required CRC32 checksum that
	// R2 rejects.
	attachmentSweepBatch    = 100
	attachmentDeleteTimeout = 30 * time.Second
)

// AttachmentSweepResult counts one invocation. A later run of the same rows
// is a no-op once unbound rows are gone and bound rows are expired.
type AttachmentSweepResult struct {
	RemovedObjects int
	DeletedRows    int
	ExpiredRows    int
}

type agedAttachment struct {
	id  string
	key string
}

// SweepAttachments removes conversation objects older than retention, measured
// from attachments.created_at with the database clock. Unbound rows are
// deleted. Bound rows are marked expired and message_attachments is left in
// place, so an empty message body still satisfies its constraint. Message text
// is not modified. Object deletion goes through store; a missing object is
// success. Rows whose object delete fails stay ready for the next run.
func SweepAttachments(ctx context.Context, pool *pgxpool.Pool, store blob.Store, retention time.Duration) (AttachmentSweepResult, error) {
	return sweepAttachments(ctx, pool, store, retention, attachmentSweepBatch)
}

func sweepAttachments(ctx context.Context, pool *pgxpool.Pool, store blob.Store, retention time.Duration, batch int) (AttachmentSweepResult, error) {
	if retention <= 0 || batch < 1 {
		return AttachmentSweepResult{}, errors.New("attachment sweep retention and batch must be positive")
	}
	if pool == nil || store == nil {
		return AttachmentSweepResult{}, errors.New("attachment sweep requires a database pool and an object store")
	}
	var total AttachmentSweepResult
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		result, more, err := sweepAttachmentBatch(ctx, pool, store, retention, batch)
		total.RemovedObjects += result.RemovedObjects
		total.DeletedRows += result.DeletedRows
		total.ExpiredRows += result.ExpiredRows
		if err != nil {
			return total, err
		}
		if !more {
			return total, nil
		}
	}
}

func sweepAttachmentBatch(ctx context.Context, pool *pgxpool.Pool, store blob.Store, retention time.Duration, batch int) (AttachmentSweepResult, bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return AttachmentSweepResult{}, false, err
	}
	defer tx.Rollback(ctx)
	items, err := lockAgedAttachments(ctx, tx, retention, batch)
	if err != nil {
		return AttachmentSweepResult{}, false, err
	}
	if len(items) == 0 {
		return AttachmentSweepResult{}, false, nil
	}
	removed, failed, sample, deleteErr := deleteAgedObjects(ctx, store, items)
	var result AttachmentSweepResult
	if len(removed) > 0 {
		deleted, expired, err := retireAttachments(ctx, tx, removed)
		if err != nil {
			return AttachmentSweepResult{}, false, err
		}
		result = AttachmentSweepResult{RemovedObjects: len(removed), DeletedRows: deleted, ExpiredRows: expired}
	}
	if err := tx.Commit(ctx); err != nil {
		return AttachmentSweepResult{}, false, err
	}
	if deleteErr != nil {
		return result, false, deleteErr
	}
	if failed > 0 {
		return result, false, fmt.Errorf("attachment sweep could not delete %d objects: %w", failed, sample)
	}
	return result, len(items) == batch, nil
}

func lockAgedAttachments(ctx context.Context, tx pgx.Tx, retention time.Duration, batch int) ([]agedAttachment, error) {
	rows, err := tx.Query(ctx, `SELECT a.id::text, a.object_key
		FROM odysseia.attachments a
		WHERE a.status = 'ready'
		  AND a.created_at <= transaction_timestamp() - ($1::bigint * interval '1 microsecond')
		ORDER BY a.created_at, a.id
		LIMIT $2
		FOR UPDATE OF a SKIP LOCKED`, retention.Microseconds(), batch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]agedAttachment, 0, batch)
	for rows.Next() {
		var item agedAttachment
		if err := rows.Scan(&item.id, &item.key); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func deleteAgedObjects(ctx context.Context, store blob.Store, items []agedAttachment) (removed []string, failed int, sample error, err error) {
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return removed, failed, sample, err
		}
		deleteCtx, cancel := context.WithTimeout(ctx, attachmentDeleteTimeout)
		deleteErr := store.Delete(deleteCtx, item.key)
		cancel()
		if deleteErr != nil {
			if ctx.Err() != nil {
				return removed, failed, sample, ctx.Err()
			}
			failed++
			if sample == nil {
				sample = deleteErr
			}
			continue
		}
		removed = append(removed, item.id)
	}
	return removed, failed, sample, nil
}

// retireAttachments rechecks the link inside the same statement. A row that
// was bound while its object was deleted is expired instead of removed, and
// the message link is not deleted.
func retireAttachments(ctx context.Context, tx pgx.Tx, ids []string) (int, int, error) {
	var deleted, expired int
	err := tx.QueryRow(ctx, `WITH chosen AS (
			SELECT a.id,
				EXISTS (
					SELECT 1 FROM odysseia.message_attachments link
					WHERE link.attachment_id = a.id
				) AS bound
			FROM odysseia.attachments a
			WHERE a.id = ANY($1::uuid[]) AND a.status = 'ready'
		),
		removed AS (
			DELETE FROM odysseia.attachments a
			USING chosen
			WHERE a.id = chosen.id AND NOT chosen.bound
			RETURNING a.id
		),
		expired AS (
			UPDATE odysseia.attachments a
			SET status = 'expired', expired_at = transaction_timestamp()
			FROM chosen
			WHERE a.id = chosen.id AND chosen.bound
			RETURNING a.id
		)
		SELECT (SELECT count(*) FROM removed), (SELECT count(*) FROM expired)`, ids).Scan(&deleted, &expired)
	return deleted, expired, err
}
