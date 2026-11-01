package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"odysseia.local/backend/internal/blob"
	"odysseia.local/backend/internal/emoji"
	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/identity"
	"odysseia.local/backend/internal/message"
)

// EmojiSettings wires the public bucket. It is separate from conversation
// attachments. A nil store makes uploads fail closed.
type EmojiSettings struct {
	Store  blob.PublicStore
	Limits emoji.Limits
	Owners []string
}

func (s *Forums) UseEmoji(settings EmojiSettings) error {
	if settings.Store == nil {
		return errors.New("emoji store is required")
	}
	if err := settings.Limits.Valid(); err != nil {
		return err
	}
	owners := make(map[string]struct{}, len(settings.Owners))
	for _, id := range settings.Owners {
		parsed, err := uuid.Parse(id)
		if err != nil || parsed == uuid.Nil || parsed.String() != id {
			return errors.New("emoji site owner must be a canonical uuid")
		}
		if _, ok := owners[id]; ok {
			return errors.New("emoji site owner list contains a duplicate")
		}
		owners[id] = struct{}{}
	}
	s.emojiStore = settings.Store
	s.emojiLimits = settings.Limits
	s.emojiOwners = owners
	return nil
}

func (s *Forums) CreateSiteEmoji(ctx context.Context, actor, requestID, name string, image emoji.Image) (emoji.Emoji, error) {
	return s.createEmoji(ctx, actor, requestID, "site", "", name, image)
}

func (s *Forums) CreateForumEmoji(ctx context.Context, actor, forumID, requestID, name string, image emoji.Image) (emoji.Emoji, error) {
	if !canonicalEmojiID(forumID) {
		return emoji.Emoji{}, emoji.ErrInvalid
	}
	return s.createEmoji(ctx, actor, requestID, "forum", forumID, name, image)
}

func (s *Forums) ListSiteEmoji(ctx context.Context, actor string) ([]emoji.Emoji, error) {
	if _, err := uuid.Parse(actor); err != nil {
		return nil, emoji.ErrInvalid
	}
	return s.listEmoji(ctx, "site", "")
}

func (s *Forums) ListForumEmoji(ctx context.Context, actor, forumID string) ([]emoji.Emoji, error) {
	if !canonicalEmojiID(forumID) {
		return nil, emoji.ErrInvalid
	}
	if err := s.forumEmojiMember(ctx, actor, forumID); err != nil {
		return nil, err
	}
	return s.listEmoji(ctx, "forum", forumID)
}

func (s *Forums) GetEmoji(ctx context.Context, actor, id string) (emoji.Emoji, error) {
	if _, err := uuid.Parse(actor); err != nil || !canonicalEmojiID(id) {
		return emoji.Emoji{}, emoji.ErrInvalid
	}
	item, err := s.loadEmoji(ctx, s.pool, id)
	if err != nil {
		return emoji.Emoji{}, err
	}
	if item.Scope == "forum" {
		if err := s.forumEmojiMember(ctx, actor, item.ForumID); err != nil {
			return emoji.Emoji{}, err
		}
	}
	return item, nil
}

func (s *Forums) DeleteSiteEmoji(ctx context.Context, actor, id string) error {
	if !canonicalEmojiID(id) {
		return emoji.ErrInvalid
	}
	if !s.emojiOwner(actor) {
		return emoji.ErrForbidden
	}
	return s.deleteEmoji(ctx, actor, id, "site", "")
}

func (s *Forums) DeleteForumEmoji(ctx context.Context, actor, forumID, id string) error {
	if !canonicalEmojiID(forumID) || !canonicalEmojiID(id) {
		return emoji.ErrInvalid
	}
	if err := s.forumEmojiManager(ctx, actor, forumID); err != nil {
		return err
	}
	return s.deleteEmoji(ctx, actor, id, "forum", forumID)
}

func (s *Forums) createEmoji(ctx context.Context, actor, requestID, scope, forumID, name string, image emoji.Image) (emoji.Emoji, error) {
	if s.emojiStore == nil {
		return emoji.Emoji{}, errors.New("emoji storage is not configured")
	}
	if _, err := uuid.Parse(actor); err != nil || !canonicalEmojiID(requestID) {
		return emoji.Emoji{}, emoji.ErrInvalid
	}
	if err := emoji.ValidName(name); err != nil {
		return emoji.Emoji{}, err
	}
	if err := image.Check(s.emojiLimits); err != nil {
		return emoji.Emoji{}, err
	}
	if scope == "site" && !s.emojiOwner(actor) {
		return emoji.Emoji{}, emoji.ErrForbidden
	}
	if scope == "forum" {
		if err := s.forumEmojiManager(ctx, actor, forumID); err != nil {
			return emoji.Emoji{}, err
		}
	}
	// Put before the row. A failed insert leaves a content-addressed object;
	// a later upload of the same bytes reuses the key. The row is never
	// advertised without the object.
	if err := s.emojiStore.Put(ctx, blob.PublicObject{Key: image.Key, ContentType: image.ContentType, Body: image.Bytes}); err != nil {
		return emoji.Emoji{}, err
	}
	digest := emoji.RequestHash(scope, forumID, name, image.Sum)
	var result emoji.Emoji
	err := writeTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockEmoji(ctx, tx, "emoji-request:"+actor+":"+requestID); err != nil {
			return err
		}
		scopeKey := "emoji-site"
		if scope == "forum" {
			scopeKey = "emoji-forum:" + forumID
		}
		if err := lockEmoji(ctx, tx, scopeKey); err != nil {
			return err
		}
		if scope == "forum" {
			if err := s.forumEmojiManagerTx(ctx, tx, actor, forumID); err != nil {
				return err
			}
		}
		existing, found, err := findEmojiRequest(ctx, tx, actor, requestID, s.emojiURL())
		if err != nil {
			return err
		}
		if found {
			if existing.hash != digest || existing.item.Scope != scope || existing.item.ForumID != forumID {
				return emoji.ErrConflict
			}
			if existing.deleted {
				return emoji.ErrNotFound
			}
			result = existing.item
			return nil
		}
		limit := emoji.MaxSiteLive
		if scope == "forum" {
			limit = emoji.MaxForumLive
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM odysseia.emojis
			WHERE scope = $1 AND deleted_at IS NULL AND forum_id IS NOT DISTINCT FROM $2::uuid`, scope, forumArg(forumID)).Scan(&count); err != nil {
			return err
		}
		if count >= limit {
			return emoji.ErrLimit
		}
		result = emoji.Emoji{
			ID: uuid.NewString(), Name: name, Animated: image.Animated, Scope: scope, ForumID: forumID,
			ContentType: image.ContentType, URL: s.emojiStore.PublicURL(image.Key),
			Width: image.Width, Height: image.Height, Bytes: len(image.Bytes),
		}
		_, err = tx.Exec(ctx, `INSERT INTO odysseia.emojis
			(id, scope, forum_id, name, animated, content_type, byte_size, width, height, sha256, object_key, created_by, request_id, request_hash)
			VALUES ($1, $2, NULLIF($3, '')::uuid, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			result.ID, scope, forumID, name, image.Animated, image.ContentType, len(image.Bytes), image.Width, image.Height,
			image.Sum[:], image.Key, actor, requestID, digest[:])
		return emojiWriteError(err)
	})
	if err != nil {
		return emoji.Emoji{}, err
	}
	return result, nil
}

func (s *Forums) listEmoji(ctx context.Context, scope, forumID string) ([]emoji.Emoji, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text, scope, COALESCE(forum_id::text, ''), name, animated, content_type,
			byte_size, width, height, object_key
		FROM odysseia.emojis
		WHERE scope = $1 AND deleted_at IS NULL AND forum_id IS NOT DISTINCT FROM $2::uuid
		ORDER BY name, id`, scope, forumArg(forumID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []emoji.Emoji{}
	for rows.Next() {
		item, err := scanEmoji(rows, s.emojiURL())
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Forums) loadEmoji(ctx context.Context, q rowQueryer, id string) (emoji.Emoji, error) {
	row := q.QueryRow(ctx, `SELECT id::text, scope, COALESCE(forum_id::text, ''), name, animated, content_type,
			byte_size, width, height, object_key
		FROM odysseia.emojis WHERE id = $1 AND deleted_at IS NULL`, id)
	item, err := scanEmoji(row, s.emojiURL())
	if errors.Is(err, pgx.ErrNoRows) {
		return emoji.Emoji{}, emoji.ErrNotFound
	}
	return item, err
}

func (s *Forums) deleteEmoji(ctx context.Context, actor, id, scope, forumID string) error {
	return writeTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		if scope == "forum" {
			if err := s.forumEmojiManagerTx(ctx, tx, actor, forumID); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE odysseia.emojis SET deleted_at = transaction_timestamp()
			WHERE id = $1 AND scope = $2 AND deleted_at IS NULL AND forum_id IS NOT DISTINCT FROM $3::uuid`, id, scope, forumArg(forumID))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return emoji.ErrNotFound
		}
		return nil
	})
}

func (s *Forums) emojiOwner(actor string) bool {
	_, ok := s.emojiOwners[actor]
	return ok
}

func (s *Forums) emojiURL() func(string) string {
	if s.emojiStore == nil {
		return func(string) string { return "" }
	}
	return s.emojiStore.PublicURL
}

func (s *Forums) forumEmojiMember(ctx context.Context, actor, forumID string) error {
	var priority int
	err := s.pool.QueryRow(ctx, `SELECT g.priority FROM odysseia.forum_members m
		JOIN odysseia.identity_groups g ON g.id = m.role_id
		WHERE m.forum_id = $1 AND m.user_id = $2`, forumID, actor).Scan(&priority)
	if errors.Is(err, pgx.ErrNoRows) {
		return emoji.ErrNotFound
	}
	return err
}

func (s *Forums) forumEmojiManager(ctx context.Context, actor, forumID string) error {
	return s.forumEmojiManagerQuery(ctx, s.pool, actor, forumID)
}

func (s *Forums) forumEmojiManagerTx(ctx context.Context, tx pgx.Tx, actor, forumID string) error {
	if err := lockForum(ctx, tx, forumID); err != nil {
		return emojiPermissionError(err)
	}
	return s.forumEmojiManagerQuery(ctx, tx, actor, forumID)
}

func (s *Forums) forumEmojiManagerQuery(ctx context.Context, q rowQueryer, actor, forumID string) error {
	winner, _, err := requireForumMember(ctx, q, forumID, actor)
	if err != nil {
		return emojiPermissionError(err)
	}
	if winner.Priority < identity.ModeratorPriority {
		return emoji.ErrForbidden
	}
	return nil
}

func emojiPermissionError(err error) error {
	if errors.Is(err, forum.ErrNotFound) {
		return emoji.ErrNotFound
	}
	return err
}

type emojiRequest struct {
	item    emoji.Emoji
	hash    [32]byte
	deleted bool
}

func findEmojiRequest(ctx context.Context, tx pgx.Tx, actor, requestID string, urlFor func(string) string) (emojiRequest, bool, error) {
	var found emojiRequest
	var stored []byte
	var key string
	var deleted *string
	err := tx.QueryRow(ctx, `SELECT id::text, scope, COALESCE(forum_id::text, ''), name, animated, content_type,
			byte_size, width, height, object_key, request_hash, deleted_at::text
		FROM odysseia.emojis WHERE created_by = $1 AND request_id = $2`, actor, requestID).Scan(
		&found.item.ID, &found.item.Scope, &found.item.ForumID, &found.item.Name, &found.item.Animated,
		&found.item.ContentType, &found.item.Bytes, &found.item.Width, &found.item.Height, &key, &stored, &deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return emojiRequest{}, false, nil
	}
	if err != nil {
		return emojiRequest{}, false, err
	}
	if len(stored) != 32 {
		return emojiRequest{}, false, errors.New("emoji request hash has the wrong length")
	}
	copy(found.hash[:], stored)
	found.deleted = deleted != nil
	found.item.URL = urlFor(key)
	return found, true, nil
}

func lockEmoji(ctx context.Context, tx pgx.Tx, key string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key)
	return err
}

func scanEmoji(row pgx.Row, urlFor func(string) string) (emoji.Emoji, error) {
	var item emoji.Emoji
	var key string
	err := row.Scan(&item.ID, &item.Scope, &item.ForumID, &item.Name, &item.Animated, &item.ContentType,
		&item.Bytes, &item.Width, &item.Height, &key)
	if err != nil {
		return emoji.Emoji{}, err
	}
	item.URL = urlFor(key)
	return item, nil
}

func emojiWriteError(err error) error {
	switch constraintName(err) {
	case "emojis_site_name_live", "emojis_forum_name_live":
		return emoji.ErrNameTaken
	case "emojis_request":
		return emoji.ErrConflict
	default:
		return err
	}
}

func forumArg(forumID string) any {
	if forumID == "" {
		return nil
	}
	return forumID
}

func canonicalEmojiID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil && parsed.String() == value
}

// bindEmoji checks tokens inside the message transaction. placeForum is the
// forum of a topic or channel. An empty placeForum is a conversation, which
// accepts site emoji only. The returned refs go on the outbox payload. The
// stored body keeps the tokens. Write paths still reject more than
// MaxPerMessage tokens; reads do not.
func (s *Forums) bindEmoji(ctx context.Context, tx pgx.Tx, body, placeForum string) ([]message.CustomEmoji, error) {
	tokens, err := emoji.Parse(body)
	if err != nil || len(tokens) == 0 {
		return nil, err
	}
	if s.emojiStore == nil {
		return nil, errors.New("emoji storage is not configured")
	}
	live, err := s.liveEmoji(ctx, tx, emoji.IDs(tokens))
	if err != nil {
		return nil, err
	}
	refs, err := emoji.Resolve(tokens, live, placeForum)
	if err != nil {
		return nil, err
	}
	return customEmoji(refs), nil
}

// projectMessages rewrites deleted emoji after the page cache. The cached body
// stays canonical; the response is the reader projection.
func (s *Forums) projectMessages(ctx context.Context, items []*message.Message) error {
	var ids []string
	seen := map[string]struct{}{}
	for _, item := range items {
		tokens, err := emoji.Parse(item.Body)
		if err != nil {
			// A body Parse rejects, including one stored before the 50-token cap,
			// stays as written. That row must not fail the rest of the page.
			continue
		}
		for _, token := range tokens {
			if _, ok := seen[token.ID]; ok {
				continue
			}
			seen[token.ID] = struct{}{}
			ids = append(ids, token.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	live, err := s.liveEmoji(ctx, s.pool, ids)
	if err != nil {
		return err
	}
	for _, item := range items {
		body, refs := emoji.Project(item.Body, live)
		item.Body = body
		item.Emojis = customEmoji(refs)
	}
	return nil
}

func (s *Forums) presentEntries(ctx context.Context, entries []forum.Entry) error {
	ptrs := make([]*message.Message, len(entries))
	for index := range entries {
		ptrs[index] = &entries[index].Message
	}
	return s.projectMessages(ctx, ptrs)
}

func (s *Forums) presentMessages(ctx context.Context, items []message.Message) error {
	ptrs := make([]*message.Message, len(items))
	for index := range items {
		ptrs[index] = &items[index]
	}
	return s.projectMessages(ctx, ptrs)
}

func (s *Forums) liveEmoji(ctx context.Context, q rowQueryer, ids []string) (map[string]emoji.Ref, error) {
	rows, err := q.Query(ctx, `SELECT id::text, scope, COALESCE(forum_id::text, ''), name, animated, content_type,
			byte_size, width, height, object_key
		FROM odysseia.emojis WHERE deleted_at IS NULL AND id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	live := map[string]emoji.Ref{}
	urlFor := s.emojiURL()
	for rows.Next() {
		item, err := scanEmoji(rows, urlFor)
		if err != nil {
			return nil, err
		}
		live[item.ID] = item.Ref()
	}
	return live, rows.Err()
}

func customEmoji(refs []emoji.Ref) []message.CustomEmoji {
	if len(refs) == 0 {
		return nil
	}
	out := make([]message.CustomEmoji, len(refs))
	for i, ref := range refs {
		out[i] = message.CustomEmoji{
			ID: ref.ID, Name: ref.Name, Animated: ref.Animated, Scope: ref.Scope, ForumID: ref.ForumID,
			ContentType: ref.ContentType, URL: ref.URL, Width: ref.Width, Height: ref.Height, Bytes: ref.Bytes,
		}
	}
	return out
}
