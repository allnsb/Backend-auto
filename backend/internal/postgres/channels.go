package postgres

import (
	"bytes"
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/message"
	"odysseia.local/backend/internal/telemetry"
)

const channelColumns = `c.id::text,c.forum_id::text,c.position,c.author_id::text,c.title,c.created_at`

func (s *Forums) GetChannel(ctx context.Context, actor, id string) (forum.Channel, error) {
	defer telemetry.Start(ctx, telemetry.AccessCheck)()
	return scanChannel(s.pool.QueryRow(ctx, `SELECT `+channelColumns+` FROM odysseia.channels c
		JOIN odysseia.forum_members a ON a.forum_id=c.forum_id WHERE c.id=$1 AND a.user_id=$2`, id, actor))
}

func (s *Forums) ListChannels(ctx context.Context, actor, forumID string, after int64, limit int) ([]forum.Channel, error) {
	if _, err := s.GetForum(ctx, actor, forumID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+channelColumns+` FROM odysseia.channels c
		JOIN odysseia.forum_members a ON a.forum_id=c.forum_id
		WHERE c.forum_id=$1 AND a.user_id=$2 AND c.position>$3 ORDER BY c.position LIMIT $4`, forumID, actor, after, limit+1)
	if err != nil {
		return nil, err
	}
	return pgx.AppendRows(make([]forum.Channel, 0, limit+1), rows, func(row pgx.CollectableRow) (forum.Channel, error) { return scanChannel(row) })
}

func (s *Forums) CreateChannel(ctx context.Context, actor, forumID, requestID string, draft forum.ChannelDraft) (forum.Channel, error) {
	if err := draft.Validate(); err != nil {
		return forum.Channel{}, err
	}
	digest, err := requestDigest(draft)
	if err != nil {
		return forum.Channel{}, err
	}
	board, err := s.GetForum(ctx, actor, forumID)
	if err != nil {
		return forum.Channel{}, err
	}
	forumID = board.ID
	release, err := s.writes.acquire(ctx, writeScope{forumID: forumID})
	if err != nil {
		return forum.Channel{}, err
	}
	defer release()
	var result forum.Channel
	err = writeTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		var id string
		err := tx.QueryRow(ctx, `SELECT f.id::text FROM odysseia.forums f
			JOIN odysseia.forum_members a ON a.forum_id=f.id WHERE f.id=$1 AND a.user_id=$2 FOR UPDATE OF f`, forumID, actor).Scan(&id)
		if err != nil {
			return forumError(err)
		}
		previous, found, err := previousChannel(ctx, tx, actor, forumID, requestID, digest)
		if err != nil {
			return err
		}
		if found {
			result = previous
			return nil
		}
		result = forum.Channel{ID: uuid.NewString(), ForumID: forumID, AuthorID: actor, Title: draft.Title}
		// Forum topic and channel creation share one event order. List positions
		// may have gaps; no second counter can publish conflicting forum positions.
		err = tx.QueryRow(ctx, `UPDATE odysseia.forums SET last_topic_position=last_topic_position+1 WHERE id=$1 RETURNING last_topic_position`, forumID).Scan(&result.Position)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO odysseia.message_streams(id,kind,last_position) VALUES ($1,'channel',0)`, result.ID); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `INSERT INTO odysseia.channels(id,forum_id,position,author_id,title,request_id,request_hash)
			VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING created_at`, result.ID, forumID, result.Position, actor, draft.Title, requestID, digest[:]).Scan(&result.CreatedAt)
		if err != nil {
			return err
		}
		return enqueueForumEvent(ctx, tx, "forum:"+forumID, "forum.channel.created", forumID, result.Position, result)
	})
	return result, err
}

func previousChannel(ctx context.Context, tx pgx.Tx, actor, forumID, requestID string, digest [32]byte) (forum.Channel, bool, error) {
	var id string
	var stored []byte
	err := tx.QueryRow(ctx, `SELECT id::text,request_hash FROM odysseia.channels WHERE forum_id=$1 AND author_id=$2 AND request_id=$3`, forumID, actor, requestID).Scan(&id, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return forum.Channel{}, false, nil
	}
	if err != nil {
		return forum.Channel{}, false, err
	}
	if !bytes.Equal(stored, digest[:]) {
		return forum.Channel{}, false, forum.ErrConflict
	}
	result, err := scanChannel(tx.QueryRow(ctx, `SELECT `+channelColumns+` FROM odysseia.channels c WHERE c.id=$1`, id))
	return result, true, err
}

func (s *Forums) ListChannelMessages(ctx context.Context, actor, channelID string, after int64, limit int) ([]message.Message, error) {
	var streamID string
	var last int64
	finish := telemetry.Start(ctx, telemetry.AccessCheck)
	err := s.pool.QueryRow(ctx, `SELECT c.id::text,s.last_position FROM odysseia.channels c
		JOIN odysseia.message_streams s ON s.id=c.id JOIN odysseia.forum_members a ON a.forum_id=c.forum_id
		WHERE c.id=$1 AND a.user_id=$2`, channelID, actor).Scan(&streamID, &last)
	finish()
	if err != nil {
		return nil, forumError(err)
	}
	entries, err := s.listStreamMessages(ctx, streamID, after, last, limit)
	if err != nil {
		return nil, err
	}
	result := make([]message.Message, len(entries))
	for index, entry := range entries {
		result[index] = entry.Message
	}
	return result, nil
}

func (s *Forums) SendChannelMessage(ctx context.Context, actor, channelID, requestID string, draft forum.ChannelMessageDraft) (message.Message, error) {
	if err := draft.Validate(); err != nil {
		return message.Message{}, err
	}
	digest, err := requestDigest(draft)
	if err != nil {
		return message.Message{}, err
	}
	channel, err := s.GetChannel(ctx, actor, channelID)
	if err != nil {
		return message.Message{}, err
	}
	channelID = channel.ID
	release, err := s.writes.acquire(ctx, writeScope{streamID: channelID})
	if err != nil {
		return message.Message{}, err
	}
	defer release()
	var result message.Message
	err = writeTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		reply := forum.ReplyDraft{Body: draft.Body, QuoteID: draft.QuoteID}
		state, err := lockStreamReply(ctx, tx, actor, channelID, requestID, digest, reply, channelMessageStream)
		if err != nil {
			return err
		}
		if state.found {
			result = state.previous.Message
			return nil
		}
		if err := reply.ValidateReferences(state.references); err != nil {
			return err
		}
		entry, err := appendStreamReply(ctx, tx, channelID, actor, reply, requestID, digest[:], channelMessageStream, state)
		result = entry.Message
		return err
	})
	return result, err
}

func scanChannel(row pgx.Row) (forum.Channel, error) {
	var result forum.Channel
	err := row.Scan(&result.ID, &result.ForumID, &result.Position, &result.AuthorID, &result.Title, &result.CreatedAt)
	return result, forumError(err)
}
