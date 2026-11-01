package postgres

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"odysseia.local/backend/internal/telemetry"
	"odysseia.local/backend/internal/work"
)

// Works stores author-private drafts. Its shared budget is supplied by process
// wiring; it does not access the forum store or publish private drafts to Outbox.
type Works struct {
	pool   *pgxpool.Pool
	writes *WriteBudget
}

func NewWorks(pool *pgxpool.Pool, budget *WriteBudget) (*Works, error) {
	if pool == nil || budget == nil || budget.limit < 1 || budget.limit > int64(pool.Config().MaxConns) {
		return nil, errors.New("works require a write budget within the pool maximum")
	}
	return &Works{pool: pool, writes: budget}, nil
}

const workColumns = `id::text, author_id::text, status, title, introduction, revision, created_at, updated_at`

func (s *Works) Create(ctx context.Context, actor, requestID string, draft work.Draft) (work.Work, bool, error) {
	if err := draft.Validate(); err != nil {
		return work.Work{}, false, err
	}
	digest, err := requestDigest(draft)
	if err != nil {
		return work.Work{}, false, err
	}
	release, err := s.writes.acquire(ctx, writeScope{workAuthorID: actor})
	if err != nil {
		return work.Work{}, false, err
	}
	defer release()
	defer telemetry.Start(ctx, telemetry.WorkDraftWrite)()
	created, err := scanWork(s.pool.QueryRow(ctx, `INSERT INTO odysseia.works
		(id,author_id,title,introduction,request_id,request_hash) VALUES($1,$2,$3,$4,$5,$6)
		ON CONFLICT (author_id,request_id) DO NOTHING RETURNING `+workColumns,
		uuid.NewString(), actor, draft.Title, draft.Introduction, requestID, digest[:]))
	if !errors.Is(err, work.ErrNotFound) {
		return created, err == nil, err
	}
	// A separate READ COMMITTED statement observes a concurrent winning insert.
	// The immutable creation hash survives edits; replay returns the current row.
	var found work.Work
	var stored []byte
	err = s.pool.QueryRow(ctx, `SELECT `+workColumns+`, request_hash FROM odysseia.works
		WHERE author_id=$1 AND request_id=$2`, actor, requestID).Scan(
		&found.ID, &found.AuthorID, &found.Status, &found.Title, &found.Introduction,
		&found.Revision, &found.CreatedAt, &found.UpdatedAt, &stored)
	if err != nil {
		return work.Work{}, false, err
	}
	if !bytes.Equal(stored, digest[:]) {
		return work.Work{}, false, work.ErrConflict
	}
	return found, false, nil
}

func (s *Works) Get(ctx context.Context, actor, id string) (work.Work, error) {
	return scanWork(s.pool.QueryRow(ctx, `SELECT `+workColumns+` FROM odysseia.works WHERE id=$1 AND author_id=$2`, id, actor))
}

func (s *Works) List(ctx context.Context, actor string, before time.Time, beforeID string, limit int) ([]work.Work, error) {
	if limit < 1 || limit > work.MaxPageSize {
		return nil, work.ErrInvalid
	}
	var cursorTime, cursorID any
	if !before.IsZero() {
		cursorTime, cursorID = before, beforeID
	}
	rows, err := s.pool.Query(ctx, `SELECT `+workColumns+` FROM odysseia.works
		WHERE author_id=$1 AND ($2::timestamptz IS NULL OR (created_at,id)<($2,$3::uuid))
		ORDER BY created_at DESC,id DESC LIMIT $4`, actor, cursorTime, cursorID, limit+1)
	if err != nil {
		return nil, err
	}
	return pgx.AppendRows(make([]work.Work, 0, limit+1), rows, func(row pgx.CollectableRow) (work.Work, error) { return scanWork(row) })
}

func (s *Works) Update(ctx context.Context, actor, id string, change work.Update) (work.Work, error) {
	if err := change.Validate(); err != nil {
		return work.Work{}, err
	}
	// Fail hidden/missing drafts before admission; authorization is still part
	// of the atomic conditional UPDATE, not a permission cached from this read.
	finish := telemetry.Start(ctx, telemetry.AccessCheck)
	var visible bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM odysseia.works WHERE id=$1 AND author_id=$2)`, id, actor).Scan(&visible)
	finish()
	if err != nil {
		return work.Work{}, err
	}
	if !visible {
		return work.Work{}, work.ErrNotFound
	}
	release, err := s.writes.acquire(ctx, writeScope{workID: id})
	if err != nil {
		return work.Work{}, err
	}
	defer release()
	defer telemetry.Start(ctx, telemetry.WorkDraftWrite)()
	updated, err := scanWork(s.pool.QueryRow(ctx, `UPDATE odysseia.works
		SET title=$3,introduction=$4,revision=revision+1,updated_at=statement_timestamp()
		WHERE id=$1 AND author_id=$2 AND revision=$5
		  AND (title,introduction) IS DISTINCT FROM ($3,$4)
		RETURNING `+workColumns, id, actor, change.Title, change.Introduction, change.Revision))
	if !errors.Is(err, work.ErrNotFound) {
		return updated, err
	}
	current, err := s.Get(ctx, actor, id)
	if err != nil {
		return work.Work{}, err
	}
	if current.Revision != change.Revision {
		return work.Work{}, work.ErrRevision
	}
	return current, nil // Identical input keeps the same revision and timestamp.
}

func scanWork(row pgx.Row) (work.Work, error) {
	var found work.Work
	err := row.Scan(&found.ID, &found.AuthorID, &found.Status, &found.Title, &found.Introduction,
		&found.Revision, &found.CreatedAt, &found.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return work.Work{}, work.ErrNotFound
	}
	return found, err
}
