//go:build integration

package postgres

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"odysseia.local/backend/internal/blob"
	"odysseia.local/backend/internal/emoji"
	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/identity"
)

func testEmoji(t *testing.T, ctx context.Context, migrationURL string, runtimePool *pgxpool.Pool) {
	t.Helper()
	ownerPool, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerPool.Close()
	_, err = ownerPool.Exec(ctx, `GRANT USAGE ON SCHEMA odysseia TO odysseia_runtime;
		GRANT SELECT ON odysseia.forums, odysseia.forum_members, odysseia.message_streams,
			odysseia.topics, odysseia.messages, odysseia.forum_entries, odysseia.identity_groups, odysseia.emojis TO odysseia_runtime;
		GRANT INSERT ON odysseia.message_streams, odysseia.topics, odysseia.messages, odysseia.forum_entries, odysseia.emojis TO odysseia_runtime;
		GRANT UPDATE (deleted_at) ON odysseia.emojis TO odysseia_runtime;
		GRANT INSERT (method, payload) ON odysseia.realtime_outbox TO odysseia_runtime;
		GRANT USAGE ON SEQUENCE odysseia.realtime_outbox_id_seq TO odysseia_runtime;
		GRANT UPDATE (last_topic_position) ON odysseia.forums TO odysseia_runtime;
		GRANT UPDATE (last_position) ON odysseia.message_streams TO odysseia_runtime;`)
	if err != nil {
		t.Fatal(err)
	}
	users := NewUsers(ownerPool)
	ownerUser, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://emoji-fixture.example", Subject: "directory-owner"})
	if err != nil {
		t.Fatal(err)
	}
	moderator, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://emoji-fixture.example", Subject: "moderator"})
	if err != nil {
		t.Fatal(err)
	}
	member, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://emoji-fixture.example", Subject: "member"})
	if err != nil {
		t.Fatal(err)
	}
	outsider, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://emoji-fixture.example", Subject: "outsider"})
	if err != nil {
		t.Fatal(err)
	}
	boards := NewForums(ownerPool)
	board, err := boards.Provision(ctx, "表情论坛", []string{ownerUser.ID, moderator.ID, member.ID})
	if err != nil {
		t.Fatal(err)
	}
	other, err := boards.Provision(ctx, "另一论坛", []string{outsider.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ownerPool.Exec(ctx, `UPDATE odysseia.forum_members SET role_id=$1 WHERE forum_id=$2 AND user_id=$3`, identity.ModeratorGroupID, board.ID, moderator.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ownerPool.Exec(ctx, `UPDATE odysseia.forum_members SET role_id=$1 WHERE forum_id=$2 AND user_id=$3`, identity.ModeratorGroupID, other.ID, outsider.ID); err != nil {
		t.Fatal(err)
	}
	store := newCachedForumFixture(t, runtimePool)
	files, err := blob.NewMemoryPublic("https://emoji.invalid/custom")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UseEmoji(EmojiSettings{Store: files, Limits: emoji.DefaultLimits(), Owners: []string{ownerUser.ID}}); err != nil {
		t.Fatal(err)
	}
	pngBytes := mustIntegrationPNG(t)
	if _, err := store.CreateForumEmoji(ctx, member.ID, board.ID, uuid.NewString(), "wave", mustEmojiImage(t, pngBytes)); !errors.Is(err, emoji.ErrForbidden) {
		t.Fatalf("member upload err=%v", err)
	}
	wave, err := store.CreateForumEmoji(ctx, moderator.ID, board.ID, uuid.NewString(), "wave", mustEmojiImage(t, pngBytes))
	if err != nil || !bytes.Contains([]byte(wave.URL), []byte("emoji/v1/")) || wave.Animated {
		t.Fatalf("wave=%+v err=%v", wave, err)
	}
	if _, err := store.CreateForumEmoji(ctx, moderator.ID, board.ID, uuid.NewString(), "wave", mustEmojiImage(t, pngBytes)); !errors.Is(err, emoji.ErrNameTaken) {
		t.Fatalf("duplicate name err=%v", err)
	}
	if _, err := store.CreateSiteEmoji(ctx, moderator.ID, uuid.NewString(), "sitewave", mustEmojiImage(t, pngBytes)); !errors.Is(err, emoji.ErrForbidden) {
		t.Fatalf("site permission err=%v", err)
	}
	foreign, err := store.CreateForumEmoji(ctx, outsider.ID, other.ID, uuid.NewString(), "foreign", mustEmojiImage(t, pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	gifBytes := mustIntegrationGIF(t)
	spin, err := store.CreateForumEmoji(ctx, moderator.ID, board.ID, uuid.NewString(), "spin", mustEmojiImage(t, gifBytes))
	if err != nil || !spin.Animated {
		t.Fatalf("spin=%+v err=%v", spin, err)
	}
	topic, err := store.CreateTopic(ctx, member.ID, board.ID, uuid.NewString(), forum.TopicDraft{Title: "表情", Body: "opening", Mode: "threaded"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Reply(ctx, member.ID, topic.Topic.ID, uuid.NewString(), forum.ReplyDraft{Body: "<:spin:" + spin.ID + ">"}); !errors.Is(err, emoji.ErrUnavailable) {
		t.Fatalf("static token for animated emoji err=%v", err)
	}
	if _, err := store.Reply(ctx, member.ID, topic.Topic.ID, uuid.NewString(), forum.ReplyDraft{Body: "<:foreign:" + foreign.ID + ">"}); !errors.Is(err, emoji.ErrUnavailable) {
		t.Fatalf("foreign forum emoji err=%v", err)
	}
	sent, err := store.Reply(ctx, member.ID, topic.Topic.ID, uuid.NewString(), forum.ReplyDraft{Body: "hello <:wave:" + wave.ID + ">"})
	if err != nil || len(sent.Emojis) != 1 || sent.Emojis[0].ID != wave.ID || sent.Emojis[0].URL != wave.URL {
		t.Fatalf("sent=%+v err=%v", sent, err)
	}
	page, err := store.ListMessages(ctx, member.ID, topic.Topic.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteForumEmoji(ctx, member.ID, board.ID, wave.ID); !errors.Is(err, emoji.ErrForbidden) {
		t.Fatalf("member delete err=%v", err)
	}
	if err := store.DeleteForumEmoji(ctx, moderator.ID, board.ID, wave.ID); err != nil {
		t.Fatal(err)
	}
	again, err := store.ListMessages(ctx, member.ID, topic.Topic.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	var shown string
	for _, item := range again {
		if item.ID == sent.ID {
			shown = item.Body
			if len(item.Emojis) != 0 {
				t.Fatalf("deleted emoji still listed: %+v", item.Emojis)
			}
		}
	}
	if shown != "hello :wave:" {
		t.Fatalf("degraded body=%q page=%d", shown, len(page))
	}
	var stored string
	if err := ownerPool.QueryRow(ctx, `SELECT body FROM odysseia.messages WHERE id=$1`, sent.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "hello <:wave:"+wave.ID+">" {
		t.Fatalf("stored body changed: %s", stored)
	}
}

func mustEmojiImage(t *testing.T, data []byte) emoji.Image {
	t.Helper()
	image, err := emoji.Prepare(data, emoji.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func mustIntegrationPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.SetNRGBA(0, 0, color.NRGBA{R: 1, G: 2, B: 3, A: 255})
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mustIntegrationGIF(t *testing.T) []byte {
	t.Helper()
	palette := color.Palette{color.Black, color.White}
	frame := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	g := &gif.GIF{Config: image.Config{Width: 2, Height: 2, ColorModel: palette}}
	g.Image = []*image.Paletted{frame, frame}
	g.Delay = []int{2, 2}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
