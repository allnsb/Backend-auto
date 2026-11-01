package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"odysseia.local/backend/internal/conversation"
	"odysseia.local/backend/internal/message"
	"odysseia.local/backend/internal/telemetry"
)

const (
	eventConversationCreated       = "conversation.created"
	eventConversationMessage       = "conversation.message.created"
	eventConversationMemberAdded   = "conversation.member.added"
	eventConversationMemberRemoved = "conversation.member.removed"
	eventConversationRead          = "conversation.read"
	eventConversationOwnerChanged  = "conversation.owner.changed"
	eventConversationDissolved     = "conversation.dissolved"
)

const conversationSelectSQL = `SELECT c.id::text, c.kind, COALESCE(c.title, ''), COALESCE(c.owner_id::text, ''),
	c.created_by::text, c.created_at, s.last_position, m.last_read_position,
	GREATEST(s.last_position - m.last_read_position, 0), COALESCE(msg.created_at, c.created_at)
	FROM odysseia.conversation_members m
	JOIN odysseia.conversations c ON c.id = m.conversation_id
	JOIN odysseia.message_streams s ON s.id = c.id
	LEFT JOIN odysseia.messages msg ON msg.stream_id = c.id AND s.last_position > 0 AND msg.position = s.last_position`

// One inbox publication per addressed member. scope_id is the conversation,
// so the existing scope hash keeps that conversation's fan-out on one partition.
// includeRemoved is delivered even after removed_at is set; onlyUser restricts
// the row to that member. Read events pass onlyUser and do not inform peers.
const insertInboxSQL = `INSERT INTO odysseia.realtime_outbox(method, payload)
SELECT 'publish', jsonb_build_object(
	'channel', 'inbox:#' || m.user_id::text,
	'idempotency_key', ids.event_id,
	'data', jsonb_build_object(
		'id', ids.event_id,
		'version', 1,
		'type', $2::text,
		'scope_id', $1::text,
		'position', $3::text,
		'data', $4::jsonb
	)
)
FROM odysseia.conversation_members m
CROSS JOIN LATERAL (SELECT gen_random_uuid()::text AS event_id) ids
WHERE m.conversation_id = $1::uuid
  AND (m.removed_at IS NULL OR m.user_id = $5::uuid)
  AND ($6::uuid IS NULL OR m.user_id = $6::uuid)`

// UseConversationMemberLimit sets the group ceiling for this process.
// Zero keeps the database maximum. The value cannot exceed that maximum.
func (s *Forums) UseConversationMemberLimit(limit int) error {
	if limit == 0 {
		s.conversationMemberLimit = conversation.MaxMembers
		return nil
	}
	if limit < 1 || limit > conversation.MaxMembers {
		return fmt.Errorf("conversation member limit must be from 1 to %d", conversation.MaxMembers)
	}
	s.conversationMemberLimit = limit
	return nil
}

func (s *Forums) memberLimit() int {
	if s.conversationMemberLimit == 0 {
		return conversation.MaxMembers
	}
	return s.conversationMemberLimit
}

// OpenDM returns a direct conversation. owned is true only when this request
// key is stored on the returned row. A replay does not restore a removed
// member. Opening an existing pair does not restore the other person either;
// a new pair is created only after the shared-forum check, and only with a new key.
func (s *Forums) OpenDM(ctx context.Context, actor, requestID string, draft conversation.DMDraft) (conversation.Conversation, bool, error) {
	if err := draft.Validate(actor); err != nil {
		return conversation.Conversation{}, false, err
	}
	if err := validConversationID(requestID); err != nil {
		return conversation.Conversation{}, false, err
	}
	low, high, err := conversation.OrderPair(actor, draft.UserID)
	if err != nil {
		return conversation.Conversation{}, false, err
	}
	digest, err := requestDigest(draft)
	if err != nil {
		return conversation.Conversation{}, false, err
	}
	// A reused key is a conflict even when the new pair shares no forum.
	// Otherwise the caller could not tell a changed retry from a hidden peer.
	if err := s.rejectChangedRequest(ctx, actor, requestID, digest); err != nil {
		return conversation.Conversation{}, false, err
	}
	// An active member may reopen without another forum check. Starting a pair
	// the caller is not in, including after they left, checks the intersection again.
	active, err := s.liveDMActive(ctx, low, high, actor)
	if err != nil {
		return conversation.Conversation{}, false, err
	}
	if !active {
		shared, err := s.sharesForum(ctx, actor, draft.UserID)
		if err != nil {
			return conversation.Conversation{}, false, err
		}
		if !shared {
			return conversation.Conversation{}, false, conversation.ErrNotFound
		}
	}
	release, err := s.writes.acquire(ctx, writeScope{streamID: "dm:" + low + ":" + high})
	if err != nil {
		return conversation.Conversation{}, false, err
	}
	defer release()
	result, owned, err := s.openDM(ctx, actor, requestID, draft.UserID, low, high, digest)
	if constraintName(err) == "conversations_dm_pair" {
		return s.openDM(ctx, actor, requestID, draft.UserID, low, high, digest)
	}
	return result, owned, err
}

func (s *Forums) openDM(ctx context.Context, actor, requestID, peer, low, high string, digest [32]byte) (conversation.Conversation, bool, error) {
	var result conversation.Conversation
	var owned bool
	err := writeTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockDMPair(ctx, tx, low, high); err != nil {
			return err
		}
		if err := lockConversationRequest(ctx, tx, actor, requestID); err != nil {
			return err
		}
		previous, found, err := previousConversation(ctx, tx, actor, requestID, digest)
		if err != nil || found {
			result, owned = previous, found
			return err
		}
		current, found, err := findDM(ctx, tx, low, high)
		if err != nil {
			return err
		}
		if found {
			active, err := memberActive(ctx, tx, current, actor)
			if err != nil {
				return err
			}
			if active {
				result, err = loadConversation(ctx, tx, actor, current)
				return err
			}
		}
		shared, err := sharesForumTx(ctx, tx, actor, peer)
		if err != nil {
			return err
		}
		if !shared {
			return conversation.ErrNotFound
		}
		if found {
			if err := releaseDMPair(ctx, tx, current); err != nil {
				return err
			}
		}
		result, err = insertDM(ctx, tx, actor, low, high, requestID, digest)
		owned = err == nil
		return err
	})
	return result, owned, err
}

func (s *Forums) CreateGroup(ctx context.Context, actor, requestID string, draft conversation.GroupDraft) (conversation.Conversation, error) {
	if err := draft.Normalize(actor, s.memberLimit()); err != nil {
		return conversation.Conversation{}, err
	}
	if err := validConversationID(requestID); err != nil {
		return conversation.Conversation{}, err
	}
	digest, err := requestDigest(draft)
	if err != nil {
		return conversation.Conversation{}, err
	}
	if err := s.rejectChangedRequest(ctx, actor, requestID, digest); err != nil {
		return conversation.Conversation{}, err
	}
	if len(draft.MemberIDs) > 0 {
		shared, err := s.sharesForums(ctx, actor, draft.MemberIDs)
		if err != nil {
			return conversation.Conversation{}, err
		}
		if !shared {
			return conversation.Conversation{}, conversation.ErrNotFound
		}
	}
	release, err := s.writes.acquire(ctx, writeScope{streamID: "group:" + actor + ":" + requestID})
	if err != nil {
		return conversation.Conversation{}, err
	}
	defer release()
	var result conversation.Conversation
	err = writeTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockConversationRequest(ctx, tx, actor, requestID); err != nil {
			return err
		}
		previous, found, err := previousConversation(ctx, tx, actor, requestID, digest)
		if err != nil || found {
			result = previous
			return err
		}
		if len(draft.MemberIDs) > 0 {
			shared, err := sharesForumsTx(ctx, tx, actor, draft.MemberIDs)
			if err != nil {
				return err
			}
			if !shared {
				return conversation.ErrNotFound
			}
		}
		result, err = insertGroup(ctx, tx, actor, requestID, draft, digest)
		return err
	})
	return result, err
}

func (s *Forums) GetConversation(ctx context.Context, actor, id string) (conversation.Conversation, error) {
	if err := validConversationID(actor, id); err != nil {
		return conversation.Conversation{}, err
	}
	defer telemetry.Start(ctx, telemetry.AccessCheck)()
	return loadConversation(ctx, s.pool, actor, id)
}

func (s *Forums) ListConversations(ctx context.Context, actor string, after time.Time, afterID string, limit int) ([]conversation.Conversation, error) {
	if err := validConversationID(actor); err != nil || limit < 1 || limit > conversation.MaxPageSize {
		return nil, conversation.ErrInvalid
	}
	var cursorAt any
	var cursorID any
	if afterID != "" {
		if err := validConversationID(afterID); err != nil || after.IsZero() {
			return nil, conversation.ErrInvalid
		}
		cursorAt, cursorID = after, afterID
	}
	rows, err := s.pool.Query(ctx, conversationSelectSQL+`
		WHERE m.user_id = $1 AND m.removed_at IS NULL AND c.dissolved_at IS NULL
		  AND ($2::timestamptz IS NULL OR (COALESCE(msg.created_at, c.created_at), c.id) < ($2::timestamptz, $3::uuid))
		ORDER BY COALESCE(msg.created_at, c.created_at) DESC, c.id DESC
		LIMIT $4`, actor, cursorAt, cursorID, limit+1)
	if err != nil {
		return nil, err
	}
	return collectConversations(rows)
}

func (s *Forums) ListConversationMessages(ctx context.Context, actor, id string, after int64, limit int) ([]message.Message, error) {
	if err := validConversationID(actor, id); err != nil || limit < 1 || limit > conversation.MaxPageSize || after < 0 {
		return nil, conversation.ErrInvalid
	}
	var streamID string
	var last int64
	finish := telemetry.Start(ctx, telemetry.AccessCheck)
	err := s.pool.QueryRow(ctx, `SELECT c.id::text, s.last_position
		FROM odysseia.conversations c
		JOIN odysseia.message_streams s ON s.id = c.id
		JOIN odysseia.conversation_members m ON m.conversation_id = c.id
		WHERE c.id = $1 AND m.user_id = $2 AND m.removed_at IS NULL AND c.dissolved_at IS NULL`, id, actor).Scan(&streamID, &last)
	finish()
	if err != nil {
		return nil, conversationError(err)
	}
	if after >= last {
		return []message.Message{}, nil
	}
	through := after + min(last-after, int64(limit+1))
	rows, err := s.pool.Query(ctx, `SELECT m.id::text, m.stream_id::text, m.position, m.author_id::text, m.body,
		COALESCE(m.quote_id::text, ''), m.created_at
		FROM odysseia.messages m
		WHERE m.stream_id = $1 AND m.position > $2 AND m.position <= $3
		ORDER BY m.position LIMIT $4`, streamID, after, through, limit+1)
	if err != nil {
		return nil, err
	}
	items, err := collectMessages(rows)
	if err != nil {
		return nil, err
	}
	if err := fillAttachments(ctx, s.pool, items); err != nil {
		return nil, err
	}
	if err := s.presentMessages(ctx, items); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Forums) SendConversationMessage(ctx context.Context, actor, id, requestID string, draft conversation.MessageDraft) (message.Message, error) {
	if err := draft.Validate(s.maxAttachments); err != nil {
		return message.Message{}, err
	}
	if err := validConversationID(actor, id, requestID); err != nil {
		return message.Message{}, err
	}
	digest, err := requestDigest(draft)
	if err != nil {
		return message.Message{}, err
	}
	if err := s.visibleConversation(ctx, actor, id); err != nil {
		return message.Message{}, err
	}
	release, err := s.writes.acquire(ctx, writeScope{streamID: id})
	if err != nil {
		return message.Message{}, err
	}
	defer release()
	var result message.Message
	err = writeTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		state, err := lockConversationMessage(ctx, tx, actor, id, requestID, digest, draft.QuoteID)
		if err != nil {
			return err
		}
		if state.found {
			loaded := []message.Message{state.previous}
			if err := fillAttachments(ctx, tx, loaded); err != nil {
				return err
			}
			result = loaded[0]
			return nil
		}
		if draft.QuoteID != "" && !state.quoteOK {
			return conversation.ErrReference
		}
		attached, err := lockAttachments(ctx, tx, actor, id, draft.AttachmentIDs)
		if err != nil {
			return err
		}
		result, err = s.appendConversationMessage(ctx, tx, actor, id, draft, requestID, digest[:], state, attached)
		return err
	})
	if err != nil {
		return message.Message{}, err
	}
	shown := []message.Message{result}
	if err := s.presentMessages(ctx, shown); err != nil {
		return message.Message{}, err
	}
	return shown[0], nil
}

func (s *Forums) AddConversationMember(ctx context.Context, actor, id, userID string) (conversation.Member, error) {
	if err := validConversationID(actor, id, userID); err != nil || actor == userID {
		return conversation.Member{}, conversation.ErrInvalid
	}
	if err := s.visibleConversation(ctx, actor, id); err != nil {
		return conversation.Member{}, err
	}
	release, err := s.writes.acquire(ctx, writeScope{streamID: id})
	if err != nil {
		return conversation.Member{}, err
	}
	defer release()
	var member conversation.Member
	err = writeTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		locked, err := lockConversation(ctx, tx, actor, id)
		if err != nil {
			return err
		}
		if locked.kind != "group" {
			return conversation.ErrInvalid
		}
		if locked.role != "owner" {
			return conversation.ErrForbidden
		}
		shared, err := sharesForumTx(ctx, tx, actor, userID)
		if err != nil {
			return err
		}
		if !shared {
			return conversation.ErrNotFound
		}
		var active int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM odysseia.conversation_members
			WHERE conversation_id = $1 AND removed_at IS NULL`, id).Scan(&active); err != nil {
			return err
		}
		existing, err := memberState(ctx, tx, id, userID)
		if err != nil {
			return err
		}
		if existing.active {
			member = existing.member
			return nil
		}
		if active >= s.memberLimit() {
			return conversation.ErrInvalid
		}
		member, err = activateMember(ctx, tx, id, userID, "member", existing.present)
		if err != nil {
			return err
		}
		return enqueueInbox(ctx, tx, id, eventConversationMemberAdded, locked.last, memberData{UserID: userID, Role: "member"}, "", "")
	})
	return member, err
}

func (s *Forums) RemoveConversationMember(ctx context.Context, actor, id, userID string) error {
	if err := validConversationID(actor, id, userID); err != nil {
		return conversation.ErrInvalid
	}
	if err := s.visibleConversation(ctx, actor, id); err != nil {
		return err
	}
	release, err := s.writes.acquire(ctx, writeScope{streamID: id})
	if err != nil {
		return err
	}
	defer release()
	return writeTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		locked, err := lockConversation(ctx, tx, actor, id)
		if err != nil {
			return err
		}
		target, err := memberState(ctx, tx, id, userID)
		if err != nil {
			return err
		}
		if !target.active {
			return conversation.ErrNotFound
		}
		if locked.kind == "dm" && userID != actor {
			return conversation.ErrForbidden
		}
		if locked.kind == "group" && userID != actor && locked.role != "owner" {
			return conversation.ErrForbidden
		}
		if target.member.Role == "owner" {
			return conversation.ErrLastManager
		}
		if _, err := tx.Exec(ctx, `UPDATE odysseia.conversation_members SET removed_at = statement_timestamp()
			WHERE conversation_id = $1 AND user_id = $2 AND removed_at IS NULL`, id, userID); err != nil {
			return err
		}
		return enqueueInbox(ctx, tx, id, eventConversationMemberRemoved, locked.last, memberData{UserID: userID, Role: target.member.Role}, userID, "")
	})
}

func (s *Forums) TransferConversationOwner(ctx context.Context, actor, id, userID string) (conversation.Conversation, error) {
	if err := validConversationID(actor, id, userID); err != nil || actor == userID {
		return conversation.Conversation{}, conversation.ErrInvalid
	}
	if err := s.visibleConversation(ctx, actor, id); err != nil {
		return conversation.Conversation{}, err
	}
	release, err := s.writes.acquire(ctx, writeScope{streamID: id})
	if err != nil {
		return conversation.Conversation{}, err
	}
	defer release()
	err = writeTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		locked, err := lockConversation(ctx, tx, actor, id)
		if err != nil {
			return err
		}
		if locked.kind != "group" {
			return conversation.ErrInvalid
		}
		if locked.role != "owner" {
			return conversation.ErrForbidden
		}
		target, err := memberState(ctx, tx, id, userID)
		if err != nil {
			return err
		}
		if !target.active {
			return conversation.ErrInvalid
		}
		if _, err := tx.Exec(ctx, `UPDATE odysseia.conversation_members SET role = 'member'
			WHERE conversation_id = $1 AND user_id = $2`, id, actor); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE odysseia.conversation_members SET role = 'owner'
			WHERE conversation_id = $1 AND user_id = $2 AND removed_at IS NULL`, id, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE odysseia.conversations SET owner_id = $2 WHERE id = $1`, id, userID); err != nil {
			return err
		}
		return enqueueInbox(ctx, tx, id, eventConversationOwnerChanged, locked.last, ownerData{OwnerID: userID}, "", "")
	})
	if err != nil {
		return conversation.Conversation{}, err
	}
	return loadConversation(ctx, s.pool, actor, id)
}

func (s *Forums) DissolveConversation(ctx context.Context, actor, id string) error {
	if err := validConversationID(actor, id); err != nil {
		return conversation.ErrInvalid
	}
	if err := s.visibleConversation(ctx, actor, id); err != nil {
		return err
	}
	release, err := s.writes.acquire(ctx, writeScope{streamID: id})
	if err != nil {
		return err
	}
	defer release()
	return writeTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		locked, err := lockConversation(ctx, tx, actor, id)
		if err != nil {
			return err
		}
		if locked.kind != "group" {
			return conversation.ErrInvalid
		}
		if locked.role != "owner" {
			return conversation.ErrForbidden
		}
		if err := enqueueInbox(ctx, tx, id, eventConversationDissolved, locked.last, dissolvedData{ID: id}, "", ""); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE odysseia.conversation_members
			SET role = 'member', removed_at = statement_timestamp()
			WHERE conversation_id = $1 AND removed_at IS NULL`, id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE odysseia.conversations SET dissolved_at = statement_timestamp() WHERE id = $1`, id)
		return err
	})
}

func (s *Forums) MarkConversationRead(ctx context.Context, actor, id string, position int64) (conversation.ReadState, error) {
	if err := validConversationID(actor, id); err != nil {
		return conversation.ReadState{}, conversation.ErrInvalid
	}
	if err := s.visibleConversation(ctx, actor, id); err != nil {
		return conversation.ReadState{}, err
	}
	release, err := s.writes.acquire(ctx, writeScope{streamID: id})
	if err != nil {
		return conversation.ReadState{}, err
	}
	defer release()
	var state conversation.ReadState
	err = writeTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		locked, err := lockConversation(ctx, tx, actor, id)
		if err != nil {
			return err
		}
		next, advanced, err := conversation.NextRead(locked.read, position, locked.last)
		if err != nil || !advanced {
			state = conversation.ReadState{Position: locked.read, Unread: locked.last - locked.read}
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE odysseia.conversation_members SET last_read_position = $3
			WHERE conversation_id = $1 AND user_id = $2 AND last_read_position < $3`, id, actor, next); err != nil {
			return err
		}
		state = conversation.ReadState{Position: next, Unread: locked.last - next}
		return enqueueInbox(ctx, tx, id, eventConversationRead, next, readData{Position: next}, "", actor)
	})
	if state.Unread < 0 {
		state.Unread = 0
	}
	return state, err
}

func (s *Forums) ListUnread(ctx context.Context, actor string) (conversation.UnreadSummary, error) {
	if err := validConversationID(actor); err != nil {
		return conversation.UnreadSummary{}, conversation.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT c.id::text, s.last_position - m.last_read_position
		FROM odysseia.conversation_members m
		JOIN odysseia.conversations c ON c.id = m.conversation_id
		JOIN odysseia.message_streams s ON s.id = c.id
		WHERE m.user_id = $1 AND m.removed_at IS NULL AND c.dissolved_at IS NULL
		  AND s.last_position > m.last_read_position
		ORDER BY c.id`, actor)
	if err != nil {
		return conversation.UnreadSummary{}, err
	}
	defer rows.Close()
	summary := conversation.UnreadSummary{Items: []conversation.Unread{}}
	for rows.Next() {
		var item conversation.Unread
		if err := rows.Scan(&item.ConversationID, &item.Unread); err != nil {
			return conversation.UnreadSummary{}, err
		}
		summary.Items = append(summary.Items, item)
		summary.Total += item.Unread
	}
	return summary, rows.Err()
}

type lockedConversation struct {
	id, kind, role string
	last, read     int64
}

type memberLookup struct {
	member  conversation.Member
	present bool
	active  bool
}

type lockedMessage struct {
	previous message.Message
	found    bool
	quoteOK  bool
	last     int64
	at       time.Time
}

func (s *Forums) visibleConversation(ctx context.Context, actor, id string) error {
	return visibleConversationQuery(ctx, s.pool, actor, id)
}

func visibleConversationQuery(ctx context.Context, q rowQueryer, actor, id string) error {
	var found string
	err := q.QueryRow(ctx, `SELECT c.id::text FROM odysseia.conversations c
		JOIN odysseia.conversation_members m ON m.conversation_id = c.id
		WHERE c.id = $1 AND m.user_id = $2 AND m.removed_at IS NULL AND c.dissolved_at IS NULL`, id, actor).Scan(&found)
	return conversationError(err)
}

func (s *Forums) rejectChangedRequest(ctx context.Context, actor, requestID string, digest [32]byte) error {
	var stored []byte
	err := s.pool.QueryRow(ctx, `SELECT request_hash FROM odysseia.conversations WHERE created_by = $1 AND request_id = $2`, actor, requestID).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(stored, digest[:]) {
		return conversation.ErrConflict
	}
	return nil
}

func (s *Forums) liveDMActive(ctx context.Context, low, high, actor string) (bool, error) {
	var active bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM odysseia.conversations c
		JOIN odysseia.conversation_members m ON m.conversation_id = c.id
		WHERE c.dm_low = $1 AND c.dm_high = $2 AND m.user_id = $3 AND m.removed_at IS NULL AND c.dissolved_at IS NULL
	)`, low, high, actor).Scan(&active)
	return active, err
}

func (s *Forums) sharesForum(ctx context.Context, actor, peer string) (bool, error) {
	return sharesForumQuery(ctx, s.pool, actor, peer)
}

func (s *Forums) sharesForums(ctx context.Context, actor string, peers []string) (bool, error) {
	return sharesForumsQuery(ctx, s.pool, actor, peers)
}

type rowQueryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func sharesForumTx(ctx context.Context, tx pgx.Tx, actor, peer string) (bool, error) {
	return sharesForumQuery(ctx, tx, actor, peer)
}

func sharesForumsTx(ctx context.Context, tx pgx.Tx, actor string, peers []string) (bool, error) {
	return sharesForumsQuery(ctx, tx, actor, peers)
}

func sharesForumQuery(ctx context.Context, q rowQueryer, actor, peer string) (bool, error) {
	var userExists, shared bool
	err := q.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM odysseia.users WHERE id = $2),
		EXISTS (
			SELECT 1 FROM odysseia.forum_members a
			JOIN odysseia.forum_members b ON b.forum_id = a.forum_id
			WHERE a.user_id = $1 AND b.user_id = $2
		)`, actor, peer).Scan(&userExists, &shared)
	return userExists && shared, err
}

func sharesForumsQuery(ctx context.Context, q rowQueryer, actor string, peers []string) (bool, error) {
	var matched int
	err := q.QueryRow(ctx, `SELECT count(*) FROM unnest($2::uuid[]) AS wanted(id)
		WHERE EXISTS (SELECT 1 FROM odysseia.users u WHERE u.id = wanted.id)
		  AND EXISTS (
			SELECT 1 FROM odysseia.forum_members a
			JOIN odysseia.forum_members b ON b.forum_id = a.forum_id
			WHERE a.user_id = $1 AND b.user_id = wanted.id
		  )`, actor, peers).Scan(&matched)
	return err == nil && matched == len(peers), err
}

func lockDMPair(ctx context.Context, tx pgx.Tx, low, high string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`, low, high)
	return err
}

func lockConversationRequest(ctx context.Context, tx pgx.Tx, actor, requestID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('odysseia-conversation-request'), hashtext($1))`, actor+requestID)
	return err
}

func previousConversation(ctx context.Context, tx pgx.Tx, actor, requestID string, digest [32]byte) (conversation.Conversation, bool, error) {
	var id string
	var stored []byte
	err := tx.QueryRow(ctx, `SELECT id::text, request_hash FROM odysseia.conversations
		WHERE created_by = $1 AND request_id = $2`, actor, requestID).Scan(&id, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return conversation.Conversation{}, false, nil
	}
	if err != nil {
		return conversation.Conversation{}, false, err
	}
	if !bytes.Equal(stored, digest[:]) {
		return conversation.Conversation{}, false, conversation.ErrConflict
	}
	// Lock the conversation before deciding. A replay must not clear removed_at,
	// raise the member ceiling, or emit member.added. Non-members get the same
	// 404 as any other hidden conversation.
	var active bool
	err = tx.QueryRow(ctx, `SELECT m.user_id IS NOT NULL AND m.removed_at IS NULL AND c.dissolved_at IS NULL
		FROM odysseia.conversations c
		LEFT JOIN odysseia.conversation_members m ON m.conversation_id = c.id AND m.user_id = $2
		WHERE c.id = $1
		FOR UPDATE OF c`, id, actor).Scan(&active)
	if err != nil {
		return conversation.Conversation{}, false, conversationError(err)
	}
	if !active {
		return conversation.Conversation{}, false, conversation.ErrNotFound
	}
	result, err := loadConversation(ctx, tx, actor, id)
	return result, true, err
}

func findDM(ctx context.Context, tx pgx.Tx, low, high string) (string, bool, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM odysseia.conversations WHERE dm_low = $1 AND dm_high = $2`, low, high).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

func memberActive(ctx context.Context, tx pgx.Tx, conversationID, userID string) (bool, error) {
	var active bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM odysseia.conversation_members
		WHERE conversation_id = $1 AND user_id = $2 AND removed_at IS NULL
	)`, conversationID, userID).Scan(&active)
	return active, err
}

// releaseDMPair frees the canonical pair without restoring removed members and
// without dissolving the old conversation for whoever is still in it.
func releaseDMPair(ctx context.Context, tx pgx.Tx, conversationID string) error {
	_, err := tx.Exec(ctx, `UPDATE odysseia.conversations
		SET dm_low = NULL, dm_high = NULL
		WHERE id = $1 AND kind = 'dm' AND dm_low IS NOT NULL`, conversationID)
	return err
}

func insertDM(ctx context.Context, tx pgx.Tx, actor, low, high, requestID string, digest [32]byte) (conversation.Conversation, error) {
	id := uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO odysseia.message_streams(id, kind, last_position) VALUES ($1, 'conversation', 0)`, id); err != nil {
		return conversation.Conversation{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO odysseia.conversations
		(id, kind, dm_low, dm_high, created_by, request_id, request_hash)
		VALUES ($1, 'dm', $2, $3, $4, $5, $6)`, id, low, high, actor, requestID, digest[:]); err != nil {
		return conversation.Conversation{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO odysseia.conversation_members(conversation_id, user_id, role)
		SELECT $1, member_id, 'member' FROM unnest($2::uuid[]) AS member_id`, id, []string{low, high}); err != nil {
		return conversation.Conversation{}, err
	}
	result, err := loadConversation(ctx, tx, actor, id)
	if err != nil {
		return conversation.Conversation{}, err
	}
	err = enqueueInbox(ctx, tx, id, eventConversationCreated, 0, createdData{
		ID: id, Kind: "dm", CreatedBy: actor, MemberIDs: []string{low, high},
	}, "", "")
	return result, err
}

func insertGroup(ctx context.Context, tx pgx.Tx, actor, requestID string, draft conversation.GroupDraft, digest [32]byte) (conversation.Conversation, error) {
	id := uuid.NewString()
	members := append([]string{actor}, draft.MemberIDs...)
	if _, err := tx.Exec(ctx, `INSERT INTO odysseia.message_streams(id, kind, last_position) VALUES ($1, 'conversation', 0)`, id); err != nil {
		return conversation.Conversation{}, err
	}
	// owner_id references the member row through a deferred foreign key, so the
	// conversation row can be inserted before that membership row.
	if _, err := tx.Exec(ctx, `INSERT INTO odysseia.conversations
		(id, kind, owner_id, title, created_by, request_id, request_hash)
		VALUES ($1, 'group', $2, $3, $2, $4, $5)`, id, actor, draft.Title, requestID, digest[:]); err != nil {
		return conversation.Conversation{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO odysseia.conversation_members(conversation_id, user_id, role)
		SELECT $1, member_id, CASE WHEN member_id = $2::uuid THEN 'owner' ELSE 'member' END
		FROM unnest($3::uuid[]) AS member_id`, id, actor, members); err != nil {
		return conversation.Conversation{}, err
	}
	result, err := loadConversation(ctx, tx, actor, id)
	if err != nil {
		return conversation.Conversation{}, err
	}
	err = enqueueInbox(ctx, tx, id, eventConversationCreated, 0, createdData{
		ID: id, Kind: "group", Title: draft.Title, OwnerID: actor, CreatedBy: actor, MemberIDs: members,
	}, "", "")
	return result, err
}

func lockConversation(ctx context.Context, tx pgx.Tx, actor, id string) (lockedConversation, error) {
	var locked lockedConversation
	err := tx.QueryRow(ctx, `SELECT c.id::text, c.kind, m.role, s.last_position, m.last_read_position
		FROM odysseia.message_streams s
		JOIN odysseia.conversations c ON c.id = s.id
		JOIN odysseia.conversation_members m ON m.conversation_id = c.id
		WHERE s.id = $1 AND m.user_id = $2 AND m.removed_at IS NULL AND c.dissolved_at IS NULL
		FOR UPDATE OF s, c`, id, actor).Scan(&locked.id, &locked.kind, &locked.role, &locked.last, &locked.read)
	return locked, conversationError(err)
}

func lockConversationMessage(ctx context.Context, tx pgx.Tx, actor, id, requestID string, digest [32]byte, quoteID string) (lockedMessage, error) {
	var state lockedMessage
	batch := &pgx.Batch{}
	batch.Queue(`SELECT s.id::text, s.last_position, transaction_timestamp()
		FROM odysseia.message_streams s
		JOIN odysseia.conversations c ON c.id = s.id
		JOIN odysseia.conversation_members m ON m.conversation_id = c.id
		WHERE s.id = $1 AND m.user_id = $2 AND m.removed_at IS NULL AND c.dissolved_at IS NULL
		FOR UPDATE OF s, c`, id, actor).QueryRow(func(row pgx.Row) error {
		var streamID string
		return conversationError(row.Scan(&streamID, &state.last, &state.at))
	})
	batch.Queue(`SELECT m.id::text, m.stream_id::text, m.position, m.author_id::text, m.body,
		COALESCE(m.quote_id::text, ''), m.created_at, m.request_hash
		FROM odysseia.messages m
		WHERE m.stream_id = $1 AND m.author_id = $2 AND m.request_id = $3`, id, actor, requestID).QueryRow(func(row pgx.Row) error {
		var stored []byte
		err := row.Scan(&state.previous.ID, &state.previous.StreamID, &state.previous.Sequence, &state.previous.AuthorID, &state.previous.Body, &state.previous.QuoteID, &state.previous.CreatedAt, &stored)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if !bytes.Equal(stored, digest[:]) {
			return conversation.ErrConflict
		}
		state.found = true
		return nil
	})
	if quoteID != "" {
		batch.Queue(`SELECT EXISTS (SELECT 1 FROM odysseia.messages WHERE stream_id = $1 AND id = $2)`, id, quoteID).QueryRow(func(row pgx.Row) error {
			return row.Scan(&state.quoteOK)
		})
	}
	return state, tx.SendBatch(ctx, batch).Close()
}

func (s *Forums) appendConversationMessage(ctx context.Context, tx pgx.Tx, actor, id string, draft conversation.MessageDraft, requestID string, digest []byte, state lockedMessage, attached []message.Attachment) (message.Message, error) {
	msg := message.Message{
		ID: uuid.NewString(), StreamID: id, Sequence: state.last + 1, AuthorID: actor,
		Body: draft.Body, QuoteID: draft.QuoteID, Attachments: attached, CreatedAt: state.at,
	}
	refs, err := s.bindEmoji(ctx, tx, draft.Body, "")
	if err != nil {
		return message.Message{}, err
	}
	msg.Emojis = refs
	payload, err := json.Marshal(msg)
	if err != nil {
		return message.Message{}, err
	}
	batch := &pgx.Batch{}
	batch.Queue(`WITH next_position AS (
		UPDATE odysseia.message_streams SET last_position = last_position + 1 WHERE id = $2 RETURNING last_position
	)
	INSERT INTO odysseia.messages(id, stream_id, position, author_id, body, quote_id, request_id, request_hash, created_at)
	SELECT $1, $2, last_position, $3, $4, NULLIF($5, '')::uuid, $6, $7, $8 FROM next_position
	RETURNING position, created_at`, msg.ID, id, actor, draft.Body, draft.QuoteID, requestID, digest, msg.CreatedAt).QueryRow(func(row pgx.Row) error {
		var position int64
		var createdAt time.Time
		if err := row.Scan(&position, &createdAt); err != nil {
			return err
		}
		if position != msg.Sequence || !createdAt.Equal(msg.CreatedAt) {
			return errors.New("inserted conversation message differs from the locked counter or database transaction time")
		}
		return nil
	})
	if len(attached) > 0 {
		linkIDs := make([]string, len(attached))
		positions := make([]int32, len(attached))
		for index, item := range attached {
			linkIDs[index] = item.ID
			positions[index] = int32(index)
		}
		batch.Queue(`INSERT INTO odysseia.message_attachments (message_id, attachment_id, position)
			SELECT $1, link.id, link.pos::smallint
			FROM unnest($2::uuid[], $3::int[]) AS link(id, pos)`, msg.ID, linkIDs, positions)
	}
	// The author's own message advances that author's cursor. Peers keep their
	// cursors, and this statement does not update the conversations row.
	batch.Queue(`UPDATE odysseia.conversation_members SET last_read_position = $3
		WHERE conversation_id = $1 AND user_id = $2 AND last_read_position < $3`, id, actor, msg.Sequence)
	batch.Queue(insertInboxSQL, id, eventConversationMessage, strconv.FormatInt(msg.Sequence, 10), string(payload), nil, nil)
	finish := telemetry.Start(ctx, telemetry.AtomicMessageBatch)
	err = tx.SendBatch(ctx, batch).Close()
	finish()
	return msg, err
}

func memberState(ctx context.Context, tx pgx.Tx, conversationID, userID string) (memberLookup, error) {
	var state memberLookup
	var removed *time.Time
	err := tx.QueryRow(ctx, `SELECT user_id::text, role, joined_at, removed_at
		FROM odysseia.conversation_members WHERE conversation_id = $1 AND user_id = $2`, conversationID, userID).Scan(&state.member.UserID, &state.member.Role, &state.member.JoinedAt, &removed)
	if errors.Is(err, pgx.ErrNoRows) {
		return memberLookup{}, nil
	}
	if err != nil {
		return memberLookup{}, err
	}
	state.present = true
	state.active = removed == nil
	return state, nil
}

func activateMember(ctx context.Context, tx pgx.Tx, conversationID, userID, role string, present bool) (conversation.Member, error) {
	var member conversation.Member
	query := `INSERT INTO odysseia.conversation_members(conversation_id, user_id, role)
		VALUES ($1, $2, $3) RETURNING user_id::text, role, joined_at`
	if present {
		// Rejoining keeps last_read_position. The read cursor never moves backward,
		// including across a removal; messages posted while absent stay unread.
		query = `UPDATE odysseia.conversation_members
			SET removed_at = NULL, role = $3, joined_at = statement_timestamp()
			WHERE conversation_id = $1 AND user_id = $2
			RETURNING user_id::text, role, joined_at`
	}
	err := tx.QueryRow(ctx, query, conversationID, userID, role).Scan(&member.UserID, &member.Role, &member.JoinedAt)
	return member, err
}

func loadConversation(ctx context.Context, q rowQueryer, actor, id string) (conversation.Conversation, error) {
	item, err := scanConversation(q.QueryRow(ctx, conversationSelectSQL+`
		WHERE c.id = $1 AND m.user_id = $2 AND m.removed_at IS NULL AND c.dissolved_at IS NULL`, id, actor))
	if err != nil {
		return conversation.Conversation{}, err
	}
	rows, err := q.Query(ctx, `SELECT user_id::text, role, joined_at
		FROM odysseia.conversation_members
		WHERE conversation_id = $1 AND removed_at IS NULL
		ORDER BY joined_at, user_id`, id)
	if err != nil {
		return conversation.Conversation{}, err
	}
	defer rows.Close()
	item.Members = []conversation.Member{}
	for rows.Next() {
		var member conversation.Member
		if err := rows.Scan(&member.UserID, &member.Role, &member.JoinedAt); err != nil {
			return conversation.Conversation{}, err
		}
		item.Members = append(item.Members, member)
	}
	return item, rows.Err()
}

func enqueueInbox(ctx context.Context, tx pgx.Tx, conversationID, eventType string, position int64, data any, includeRemoved, onlyUser string) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	var removed, only any
	if includeRemoved != "" {
		removed = includeRemoved
	}
	if onlyUser != "" {
		only = onlyUser
	}
	_, err = tx.Exec(ctx, insertInboxSQL, conversationID, eventType, strconv.FormatInt(position, 10), string(raw), removed, only)
	return err
}

type createdData struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Title     string   `json:"title,omitempty"`
	OwnerID   string   `json:"owner_id,omitempty"`
	CreatedBy string   `json:"created_by"`
	MemberIDs []string `json:"member_ids"`
}

type memberData struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
}

type readData struct {
	Position int64 `json:"position,string"`
}

type ownerData struct {
	OwnerID string `json:"owner_id"`
}

type dissolvedData struct {
	ID string `json:"id"`
}

func scanConversation(row pgx.Row) (conversation.Conversation, error) {
	var item conversation.Conversation
	err := row.Scan(&item.ID, &item.Kind, &item.Title, &item.OwnerID, &item.CreatedBy, &item.CreatedAt, &item.LastPosition, &item.LastReadPosition, &item.Unread, &item.ActivityAt)
	return item, conversationError(err)
}

func collectConversations(rows pgx.Rows) ([]conversation.Conversation, error) {
	defer rows.Close()
	items := []conversation.Conversation{}
	for rows.Next() {
		item, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func collectMessages(rows pgx.Rows) ([]message.Message, error) {
	defer rows.Close()
	items := []message.Message{}
	for rows.Next() {
		var item message.Message
		if err := rows.Scan(&item.ID, &item.StreamID, &item.Sequence, &item.AuthorID, &item.Body, &item.QuoteID, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func conversationError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return conversation.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("conversation storage: %w", err)
	}
	return nil
}

func constraintName(err error) string {
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && pgError.Code == "23505" {
		return pgError.ConstraintName
	}
	return ""
}

func validConversationID(values ...string) error {
	for _, value := range values {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil || id.String() != value {
			return conversation.ErrInvalid
		}
	}
	return nil
}
