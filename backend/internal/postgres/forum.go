package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"odysseia.local/backend/internal/conversation"
	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/message"
	"odysseia.local/backend/internal/telemetry"
)

type Forums struct {
	pool                    *pgxpool.Pool
	writes                  *writeAdmission
	pages                   *messagePageCache
	conversationMemberLimit int
}

func NewForums(pool *pgxpool.Pool) *Forums {
	// Leave about a third of this shared pool outside the write budget for
	// identity and reads (16 -> 10 writers). This is tunable headroom, not a
	// separate reserved pool or a distributed quota. A pool of one has no reserve.
	poolLimit := int64(pool.Config().MaxConns)
	readReserve := (poolLimit + 2) / 3
	writers := max(int64(1), poolLimit-readReserve)
	return &Forums{
		pool: pool, writes: newWriteAdmission(writers, forumWriteQueueLimit),
		conversationMemberLimit: conversation.MaxMembers,
	}
}

const topicColumns = `t.id::text, t.forum_id::text, t.position, t.author_id::text, t.title, t.mode, t.created_at`
const entryColumns = `m.id::text, m.stream_id::text, m.position, m.author_id::text, m.body,
	COALESCE(m.quote_id::text, ''), m.created_at, COALESCE(e.root_id::text, '')`
const insertForumEntrySQL = `INSERT INTO odysseia.forum_entries(stream_id,message_id,root_id) VALUES ($1,$2,NULLIF($3,'')::uuid)`
const messagePageSQL = `SELECT ` + entryColumns + ` FROM odysseia.messages m
	LEFT JOIN odysseia.forum_entries e ON e.message_id=m.id
	WHERE m.stream_id=$1 AND m.position>$2 AND m.position<=$3 ORDER BY m.position LIMIT $4`

func (s *Forums) Provision(ctx context.Context, title string, memberIDs []string) (forum.Forum, error) {
	if err := forum.ValidateTitle(title); err != nil {
		return forum.Forum{}, err
	}
	if len(memberIDs) == 0 {
		return forum.Forum{}, forum.ErrInvalid
	}
	result := forum.Forum{ID: uuid.NewString(), Title: title}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO odysseia.forums(id, title) VALUES ($1, $2)`, result.ID, title); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO odysseia.forum_members(forum_id, user_id)
			SELECT $1, unnest($2::uuid[]) ON CONFLICT DO NOTHING`, result.ID, memberIDs)
		return err
	})
	return result, err
}

func (s *Forums) GetForum(ctx context.Context, actor, id string) (forum.Forum, error) {
	defer telemetry.Start(ctx, telemetry.AccessCheck)()
	var result forum.Forum
	err := s.pool.QueryRow(ctx, `SELECT f.id::text, f.title FROM odysseia.forums f
		JOIN odysseia.forum_members a ON a.forum_id = f.id WHERE f.id = $1 AND a.user_id = $2`, id, actor).Scan(&result.ID, &result.Title)
	return result, forumError(err)
}

func (s *Forums) GetTopic(ctx context.Context, actor, id string) (forum.Topic, error) {
	defer telemetry.Start(ctx, telemetry.AccessCheck)()
	return scanTopic(s.pool.QueryRow(ctx, `SELECT `+topicColumns+` FROM odysseia.topics t
		JOIN odysseia.forum_members a ON a.forum_id = t.forum_id WHERE t.id = $1 AND a.user_id = $2`, id, actor))
}

func (s *Forums) ListTopics(ctx context.Context, actor, forumID string, after int64, limit int) ([]forum.Topic, error) {
	if _, err := s.GetForum(ctx, actor, forumID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+topicColumns+` FROM odysseia.topics t
		JOIN odysseia.forum_members a ON a.forum_id = t.forum_id
		WHERE t.forum_id = $1 AND a.user_id = $2 AND t.position > $3 ORDER BY t.position LIMIT $4`, forumID, actor, after, limit+1)
	if err != nil {
		return nil, err
	}
	return pgx.AppendRows(make([]forum.Topic, 0, limit+1), rows, func(row pgx.CollectableRow) (forum.Topic, error) { return scanTopic(row) })
}

func (s *Forums) ListMessages(ctx context.Context, actor, topicID string, after int64, limit int) ([]forum.Entry, error) {
	var streamID string
	var last int64
	finish := telemetry.Start(ctx, telemetry.AccessCheck)
	err := s.pool.QueryRow(ctx, `SELECT t.id::text,s.last_position FROM odysseia.topics t
		JOIN odysseia.message_streams s ON s.id=t.id
		JOIN odysseia.forum_members a ON a.forum_id=t.forum_id
		WHERE t.id=$1 AND a.user_id=$2`, topicID, actor).Scan(&streamID, &last)
	finish()
	if err != nil {
		return nil, forumError(err)
	}
	return s.listStreamMessages(ctx, streamID, after, last, limit)
}

// Both topic layouts and fixed channels use the same canonical message pages.
// Their entry points establish current access and stream bounds before caching.
func (s *Forums) listStreamMessages(ctx context.Context, streamID string, after, last int64, limit int) ([]forum.Entry, error) {
	if after >= last {
		return []forum.Entry{}, nil
	}
	// Closed prefixes never change on append; an incomplete page gets a new
	// bound after another node writes. Future editing/deletion must version or
	// invalidate these pages before that capability is introduced.
	through := after + min(last-after, int64(limit+1))
	load := func(ctx context.Context) ([]forum.Entry, error) {
		return s.loadMessagePage(ctx, streamID, after, through, limit)
	}
	if s.pages == nil {
		return load(ctx)
	}
	key := fmt.Sprintf("%s/%d/%d/%d", streamID, after, through, limit)
	return s.pages.read(ctx, key, load)
}

// Callers establish current membership first. This query reads content only,
// at a bounded append snapshot; shared cache fills never carry authorization.
func (s *Forums) loadMessagePage(ctx context.Context, topicID string, after, through int64, limit int) ([]forum.Entry, error) {
	rows, err := s.pool.Query(ctx, messagePageSQL, topicID, after, through, limit+1)
	if err != nil {
		return nil, err
	}
	return pgx.AppendRows(make([]forum.Entry, 0, limit+1), rows, func(row pgx.CollectableRow) (forum.Entry, error) { return scanEntry(row) })
}

func (s *Forums) CreateTopic(ctx context.Context, actor, forumID, requestID string, draft forum.TopicDraft) (forum.CreatedTopic, error) {
	if err := draft.Validate(); err != nil {
		return forum.CreatedTopic{}, err
	}
	digest, err := requestDigest(draft)
	if err != nil {
		return forum.CreatedTopic{}, err
	}
	// Check access before entering a counter-specific queue to keep private
	// counter activity from turning an outsider's 404 into a queue timeout.
	board, err := s.GetForum(ctx, actor, forumID)
	if err != nil {
		return forum.CreatedTopic{}, err
	}
	release, err := s.writes.acquire(ctx, writeScope{forumID: board.ID})
	if err != nil {
		return forum.CreatedTopic{}, err
	}
	defer release()
	var result forum.CreatedTopic
	err = writeTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		var id string
		err := tx.QueryRow(ctx, `SELECT f.id::text FROM odysseia.forums f
			JOIN odysseia.forum_members a ON a.forum_id = f.id WHERE f.id = $1 AND a.user_id = $2 FOR UPDATE OF f`, forumID, actor).Scan(&id)
		if err != nil {
			return forumError(err)
		}
		previous, found, err := previousTopic(ctx, tx, actor, forumID, requestID, digest)
		if err != nil {
			return err
		}
		if found {
			result = previous
			return nil
		}

		topic := forum.Topic{ID: uuid.NewString(), ForumID: forumID, AuthorID: actor, Title: draft.Title, Mode: draft.Mode}
		err = tx.QueryRow(ctx, `UPDATE odysseia.forums SET last_topic_position = last_topic_position + 1 WHERE id = $1 RETURNING last_topic_position`, forumID).Scan(&topic.Position)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO odysseia.message_streams(id, last_position) VALUES ($1, 1)`, topic.ID); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `INSERT INTO odysseia.topics(id, forum_id, position, author_id, title, mode, request_id, request_hash)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING created_at`, topic.ID, forumID, topic.Position, actor, topic.Title, topic.Mode, requestID, digest[:]).Scan(&topic.CreatedAt)
		if err != nil {
			return err
		}
		entry, err := insertEntry(ctx, tx, topic.ID, actor, 1, forum.ReplyDraft{Body: draft.Body}, "", nil)
		if err != nil {
			return err
		}
		result = forum.CreatedTopic{Topic: topic, OpeningMessage: entry}
		return enqueueForumEvent(ctx, tx, "forum:"+forumID, "forum.topic.created", forumID, topic.Position, topic)
	})
	return result, err
}

func (s *Forums) Reply(ctx context.Context, actor, topicID, requestID string, draft forum.ReplyDraft) (forum.Entry, error) {
	if err := draft.Validate(); err != nil {
		return forum.Entry{}, err
	}
	digest, err := requestDigest(draft)
	if err != nil {
		return forum.Entry{}, err
	}
	topic, err := s.GetTopic(ctx, actor, topicID)
	if err != nil {
		return forum.Entry{}, err
	}
	release, err := s.writes.acquire(ctx, writeScope{streamID: topic.ID})
	if err != nil {
		return forum.Entry{}, err
	}
	defer release()
	var result forum.Entry
	err = writeTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		state, err := lockReply(ctx, tx, actor, topicID, requestID, digest, draft)
		if err != nil {
			return err
		}
		if state.found {
			result = state.previous
			return nil
		}
		if err := draft.ValidateReferences(state.references); err != nil {
			return err
		}
		result, err = appendReply(ctx, tx, topicID, actor, draft, requestID, digest[:], state)
		return err
	})
	return result, err
}

func previousTopic(ctx context.Context, tx pgx.Tx, actor, forumID, requestID string, digest [32]byte) (forum.CreatedTopic, bool, error) {
	var id string
	var stored []byte
	err := tx.QueryRow(ctx, `SELECT id::text, request_hash FROM odysseia.topics WHERE forum_id=$1 AND author_id=$2 AND request_id=$3`, forumID, actor, requestID).Scan(&id, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return forum.CreatedTopic{}, false, nil
	}
	if err != nil {
		return forum.CreatedTopic{}, false, err
	}
	if !bytes.Equal(stored, digest[:]) {
		return forum.CreatedTopic{}, false, forum.ErrConflict
	}
	topic, err := scanTopic(tx.QueryRow(ctx, `SELECT `+topicColumns+` FROM odysseia.topics t WHERE t.id=$1`, id))
	if err != nil {
		return forum.CreatedTopic{}, false, err
	}
	entry, err := scanEntry(tx.QueryRow(ctx, `SELECT `+entryColumns+` FROM odysseia.messages m
		JOIN odysseia.forum_entries e ON e.message_id=m.id WHERE m.stream_id=$1 AND m.position=1`, id))
	return forum.CreatedTopic{Topic: topic, OpeningMessage: entry}, true, err
}

type lockedReply struct {
	previous        forum.Entry
	found           bool
	references      map[string]forum.Entry
	lastPosition    int64
	transactionTime time.Time
}

// All reads remain separate READ COMMITTED statements. Idempotency and optional
// reference metadata are read after the stream lock, with fresh snapshots, in
// one batch. A same-statement CTE would retain the pre-lock snapshot instead.
func lockReply(ctx context.Context, tx pgx.Tx, actor, topicID, requestID string, digest [32]byte, draft forum.ReplyDraft) (lockedReply, error) {
	return lockStreamReply(ctx, tx, actor, topicID, requestID, digest, draft, topicMessageStream)
}

type messageStream struct {
	lockSQL, channelPrefix, eventType string
	forumLayout                       bool
}

var topicMessageStream = messageStream{
	lockSQL: `SELECT t.id::text,s.last_position,transaction_timestamp() FROM odysseia.topics t
		JOIN odysseia.message_streams s ON s.id=t.id JOIN odysseia.forum_members a ON a.forum_id=t.forum_id
		WHERE t.id=$1 AND a.user_id=$2 FOR UPDATE OF s`,
	channelPrefix: "topic:", eventType: "forum.message.created", forumLayout: true,
}

var channelMessageStream = messageStream{
	lockSQL: `SELECT c.id::text,s.last_position,transaction_timestamp() FROM odysseia.channels c
		JOIN odysseia.message_streams s ON s.id=c.id JOIN odysseia.forum_members a ON a.forum_id=c.forum_id
		WHERE c.id=$1 AND a.user_id=$2 FOR UPDATE OF s`,
	channelPrefix: "channel:", eventType: "channel.message.created",
}

func lockStreamReply(ctx context.Context, tx pgx.Tx, actor, topicID, requestID string, digest [32]byte, draft forum.ReplyDraft, stream messageStream) (lockedReply, error) {
	var state lockedReply
	batch := &pgx.Batch{}
	finishLock := telemetry.Start(ctx, telemetry.StreamLock)
	finishReads := func() {}
	batch.Queue(stream.lockSQL, topicID, actor).QueryRow(func(row pgx.Row) error {
		var id string
		err := forumError(row.Scan(&id, &state.lastPosition, &state.transactionTime))
		finishLock()
		finishReads = telemetry.Start(ctx, telemetry.IdempotencyAndReferences)
		return err
	})
	batch.Queue(`SELECT `+entryColumns+`,m.request_hash FROM odysseia.messages m
		LEFT JOIN odysseia.forum_entries e ON e.message_id=m.id
		WHERE m.stream_id=$1 AND m.author_id=$2 AND m.request_id=$3`, topicID, actor, requestID).QueryRow(func(row pgx.Row) error {
		var err error
		state.previous, state.found, err = previousEntry(row, digest)
		return err
	})
	ids := make([]string, 0, 2)
	if draft.RootID != "" {
		ids = append(ids, draft.RootID)
	}
	if draft.QuoteID != "" && draft.QuoteID != draft.RootID {
		ids = append(ids, draft.QuoteID)
	}
	if len(ids) != 0 {
		batch.Queue(`SELECT m.id::text,COALESCE(e.root_id::text,'') FROM odysseia.messages m LEFT JOIN odysseia.forum_entries e ON e.message_id=m.id
			WHERE m.stream_id=$1 AND m.id=ANY($2::uuid[])`, topicID, ids).Query(func(rows pgx.Rows) error {
			var err error
			state.references, err = readReferences(rows)
			return err
		})
	}
	// Close runs callbacks in order, drains on failure and returns errors before
	// the caller continues or commits. No write is part of this read batch.
	err := tx.SendBatch(ctx, batch).Close()
	finishReads()
	return state, err
}

func previousEntry(row pgx.Row, digest [32]byte) (forum.Entry, bool, error) {
	var stored []byte
	var entry forum.Entry
	err := row.Scan(&entry.ID, &entry.StreamID, &entry.Sequence, &entry.AuthorID, &entry.Body, &entry.QuoteID, &entry.CreatedAt, &entry.RootID, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return forum.Entry{}, false, nil
	}
	if err != nil {
		return forum.Entry{}, false, err
	}
	if !bytes.Equal(stored, digest[:]) {
		return forum.Entry{}, false, forum.ErrConflict
	}
	return entry, true, nil
}

// Only existence and root ownership are needed for business validation. The
// batch owns the result stream and drains it before the transaction continues.
func readReferences(rows pgx.Rows) (map[string]forum.Entry, error) {
	result := make(map[string]forum.Entry, 2)
	defer rows.Close()
	for rows.Next() {
		var entry forum.Entry
		if err := rows.Scan(&entry.ID, &entry.RootID); err != nil {
			return nil, err
		}
		result[entry.ID] = entry
	}
	return result, rows.Err()
}

func insertEntry(ctx context.Context, tx pgx.Tx, topicID, actor string, position int64, draft forum.ReplyDraft, requestID string, digest []byte) (forum.Entry, error) {
	entry := forum.Entry{Message: message.Message{ID: uuid.NewString(), StreamID: topicID, Sequence: position, AuthorID: actor, Body: draft.Body, QuoteID: draft.QuoteID}, RootID: draft.RootID}
	err := tx.QueryRow(ctx, `INSERT INTO odysseia.messages(id,stream_id,position,author_id,body,quote_id,request_id,request_hash)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,'')::uuid,NULLIF($7,'')::uuid,$8) RETURNING created_at`, entry.ID, topicID, position, actor, draft.Body, draft.QuoteID, requestID, digest).Scan(&entry.CreatedAt)
	if err != nil {
		return forum.Entry{}, err
	}
	_, err = tx.Exec(ctx, insertForumEntrySQL, topicID, entry.ID, draft.RootID)
	return entry, err
}

// appendReply requires the current locked counter, DB transaction time and
// validated references. Body, relation and event then travel in one write batch.
func appendReply(ctx context.Context, tx pgx.Tx, topicID, actor string, draft forum.ReplyDraft, requestID string, digest []byte, state lockedReply) (forum.Entry, error) {
	return appendStreamReply(ctx, tx, topicID, actor, draft, requestID, digest, topicMessageStream, state)
}

func appendStreamReply(ctx context.Context, tx pgx.Tx, topicID, actor string, draft forum.ReplyDraft, requestID string, digest []byte, stream messageStream, state lockedReply) (forum.Entry, error) {
	entry := forum.Entry{Message: message.Message{ID: uuid.NewString(), StreamID: topicID, Sequence: state.lastPosition + 1, AuthorID: actor, Body: draft.Body, QuoteID: draft.QuoteID, CreatedAt: state.transactionTime}, RootID: draft.RootID}
	var data any = entry.Message
	if stream.forumLayout {
		data = entry
	}
	payload, err := forumEventPayload(stream.channelPrefix+topicID, stream.eventType, topicID, entry.Sequence, data)
	if err != nil {
		return forum.Entry{}, err
	}
	// The lock supplies the next position and the same DB transaction timestamp
	// previously provided by the INSERT default. Encoding stays in one Go helper;
	// no app-clock timestamp, duplicate SQL envelope or cross-request batch.
	batch := &pgx.Batch{}
	batch.Queue(`WITH next_position AS (
		UPDATE odysseia.message_streams SET last_position = last_position + 1 WHERE id=$2 RETURNING last_position
	)
	INSERT INTO odysseia.messages(id,stream_id,position,author_id,body,quote_id,request_id,request_hash,created_at)
	SELECT $1,$2,last_position,$3,$4,NULLIF($5,'')::uuid,NULLIF($6,'')::uuid,$7,$8 FROM next_position
	RETURNING position,created_at`, entry.ID, topicID, actor, draft.Body, draft.QuoteID, requestID, digest, entry.CreatedAt).QueryRow(func(row pgx.Row) error {
		var position int64
		var createdAt time.Time
		if err := row.Scan(&position, &createdAt); err != nil {
			return err
		}
		if position != entry.Sequence || !createdAt.Equal(entry.CreatedAt) {
			return errors.New("inserted message differs from locked counter or database transaction time")
		}
		return nil
	})
	if stream.forumLayout {
		batch.Queue(insertForumEntrySQL, topicID, entry.ID, draft.RootID)
	}
	batch.Queue(insertForumEventSQL, payload)
	// Close drains every callback, including late relation/outbox failures,
	// before the enclosing transaction is allowed to commit.
	finish := telemetry.Start(ctx, telemetry.AtomicMessageBatch)
	err = tx.SendBatch(ctx, batch).Close()
	finish()
	return entry, err
}

func requestDigest(value any) ([32]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}

func scanTopic(row pgx.Row) (forum.Topic, error) {
	var topic forum.Topic
	err := row.Scan(&topic.ID, &topic.ForumID, &topic.Position, &topic.AuthorID, &topic.Title, &topic.Mode, &topic.CreatedAt)
	return topic, forumError(err)
}

func scanEntry(row pgx.Row) (forum.Entry, error) {
	var entry forum.Entry
	err := row.Scan(&entry.ID, &entry.StreamID, &entry.Sequence, &entry.AuthorID, &entry.Body, &entry.QuoteID, &entry.CreatedAt, &entry.RootID)
	return entry, forumError(err)
}

func forumError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return forum.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("forum storage: %w", err)
	}
	return nil
}
