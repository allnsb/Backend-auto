package httpapi

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"

	"odysseia.local/backend/internal/emoji"
	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/identity"
)

func TestEmojiUploadRejectionAndPermission(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(openAPI)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	actor := identity.User{ID: "dc9de165-b912-4dcb-8620-8dfdb30176b7", CreatedAt: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)}
	forumID := "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	store := &emojiFake{err: emoji.ErrForbidden}
	api := NewAPI(func(context.Context, string) (identity.Authentication, error) {
		return identity.Authentication{ExternalID: identity.ExternalID{Issuer: "https://issuer.example", Subject: "member"}, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}, func(context.Context, identity.ExternalID) (identity.User, error) {
		return actor, nil
	})
	api.RegisterEmoji(store, emoji.Limits{MaxBytes: 256 * 1024, MaxEdge: 128})
	call := func(name string, file []byte, contentType string) *httptest.ResponseRecorder {
		t.Helper()
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)
		if err := writer.WriteField("name", name); err != nil {
			t.Fatal(err)
		}
		part, err := writer.CreateFormFile("file", "upload.bin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(file); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/forums/"+forumID+"/emojis", body)
		request.Header.Set("Authorization", "Bearer valid-access-token")
		request.Header.Set("Content-Type", writer.FormDataContentType())
		request.Header.Set("Idempotency-Key", uuid.NewString())
		recorder := httptest.NewRecorder()
		api.ServeHTTP(recorder, request)
		return recorder
	}
	jpegBody := call("wave", mustJPEGBytes(t), "image/jpeg")
	if jpegBody.Code != http.StatusUnsupportedMediaType || store.creates != 0 {
		t.Fatalf("jpeg status=%d creates=%d body=%s", jpegBody.Code, store.creates, jpegBody.Body.String())
	}
	assertProblem(t, doc, jpegBody, "unsupported_media_type")
	wide := call("wave", mustPNGBytes(t, 300, 1), "image/png")
	if wide.Code != http.StatusUnprocessableEntity || store.creates != 0 {
		t.Fatalf("edge status=%d body=%s", wide.Code, wide.Body.String())
	}
	assertProblem(t, doc, wide, "invalid_input")
	named := call("bad-name", mustPNGBytes(t, 8, 8), "image/png")
	if named.Code != http.StatusUnprocessableEntity || store.creates != 0 {
		t.Fatalf("name status=%d body=%s", named.Code, named.Body.String())
	}
	denied := call("wave", mustPNGBytes(t, 8, 8), "image/png")
	if denied.Code != http.StatusForbidden || store.creates != 1 {
		t.Fatalf("permission status=%d creates=%d body=%s", denied.Code, store.creates, denied.Body.String())
	}
	assertProblem(t, doc, denied, "permission_denied")
}

func TestMessageEmojiReferenceMapsToInvalidEmoji(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(openAPI)
	if err != nil {
		t.Fatal(err)
	}
	actor := identity.User{ID: "dc9de165-b912-4dcb-8620-8dfdb30176b7", CreatedAt: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)}
	api := NewAPI(func(context.Context, string) (identity.Authentication, error) {
		return identity.Authentication{ExternalID: identity.ExternalID{Issuer: "https://issuer.example", Subject: "member"}, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}, func(context.Context, identity.ExternalID) (identity.User, error) {
		return actor, nil
	})
	api.RegisterForums(replyStub{err: emoji.ErrUnavailable})
	request := httptest.NewRequest(http.MethodPost, "/v1/topics/"+uuid.NewString()+"/messages", bytes.NewBufferString(`{"body":"<:wave:6d2f0c2e-1b7a-4e4e-9c1a-2f6b0d9a7c31>"}`))
	request.Header.Set("Authorization", "Bearer valid-access-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", uuid.NewString())
	recorder := httptest.NewRecorder()
	api.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	assertProblem(t, doc, recorder, "invalid_emoji")
}

type emojiFake struct {
	err     error
	creates int
}

func (f *emojiFake) CreateSiteEmoji(context.Context, string, string, string, emoji.Image) (emoji.Emoji, error) {
	f.creates++
	return emoji.Emoji{}, f.err
}
func (f *emojiFake) ListSiteEmoji(context.Context, string) ([]emoji.Emoji, error) {
	return []emoji.Emoji{}, nil
}
func (f *emojiFake) GetEmoji(context.Context, string, string) (emoji.Emoji, error) {
	return emoji.Emoji{}, emoji.ErrNotFound
}
func (f *emojiFake) DeleteSiteEmoji(context.Context, string, string) error { return f.err }
func (f *emojiFake) CreateForumEmoji(context.Context, string, string, string, string, emoji.Image) (emoji.Emoji, error) {
	f.creates++
	return emoji.Emoji{}, f.err
}
func (f *emojiFake) ListForumEmoji(context.Context, string, string) ([]emoji.Emoji, error) {
	return []emoji.Emoji{}, nil
}
func (f *emojiFake) DeleteForumEmoji(context.Context, string, string, string) error {
	return f.err
}

type replyStub struct{ err error }

func (s replyStub) GetForum(context.Context, string, string) (forum.Forum, error) {
	return forum.Forum{}, s.err
}
func (s replyStub) GetTopic(context.Context, string, string) (forum.Topic, error) {
	return forum.Topic{}, s.err
}
func (s replyStub) ListTopics(context.Context, string, string, int64, int) ([]forum.Topic, error) {
	return nil, s.err
}
func (s replyStub) ListMessages(context.Context, string, string, int64, int) ([]forum.Entry, error) {
	return nil, s.err
}
func (s replyStub) CreateTopic(context.Context, string, string, string, forum.TopicDraft) (forum.CreatedTopic, error) {
	return forum.CreatedTopic{}, s.err
}
func (s replyStub) Reply(context.Context, string, string, string, forum.ReplyDraft) (forum.Entry, error) {
	return forum.Entry{}, s.err
}

func mustJPEGBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mustPNGBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	img.SetNRGBA(0, 0, color.NRGBA{A: 255})
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
