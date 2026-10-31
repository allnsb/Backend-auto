package httpapi

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"

	"odysseia.local/backend/internal/emoji"
	"odysseia.local/backend/internal/identity"
	"odysseia.local/backend/internal/media"
)

// EmojiBackend is the catalog. HTTP does not know which bucket holds the bytes.
type EmojiBackend interface {
	CreateSiteEmoji(context.Context, string, string, string, emoji.Image) (emoji.Emoji, error)
	ListSiteEmoji(context.Context, string) ([]emoji.Emoji, error)
	GetEmoji(context.Context, string, string) (emoji.Emoji, error)
	DeleteSiteEmoji(context.Context, string, string) error
	CreateForumEmoji(context.Context, string, string, string, string, emoji.Image) (emoji.Emoji, error)
	ListForumEmoji(context.Context, string, string) ([]emoji.Emoji, error)
	DeleteForumEmoji(context.Context, string, string, string) error
}

func (api *API) RegisterEmoji(backend EmojiBackend, limits emoji.Limits) {
	api.emojiLimits = limits
	api.mux.HandleFunc("GET /v1/emojis", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		items, err := backend.ListSiteEmoji(r.Context(), actor.ID)
		if err != nil {
			return err
		}
		return writeJSON(w, 200, emoji.Page{Items: items})
	}))
	api.mux.HandleFunc("POST /v1/emojis", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		key, err := idempotencyKey(r)
		if err != nil {
			return err
		}
		name, image, err := api.readEmojiUpload(w, r)
		if err != nil {
			return err
		}
		result, err := backend.CreateSiteEmoji(r.Context(), actor.ID, key, name, image)
		if err != nil {
			return err
		}
		w.Header().Set("Location", "/v1/emojis/"+result.ID)
		return writeJSON(w, 201, result)
	}))
	api.mux.HandleFunc("GET /v1/emojis/{emoji_id}", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		id, err := canonicalID(r.PathValue("emoji_id"))
		if err != nil {
			return err
		}
		result, err := backend.GetEmoji(r.Context(), actor.ID, id)
		if err != nil {
			return err
		}
		return writeJSON(w, 200, result)
	}))
	api.mux.HandleFunc("DELETE /v1/emojis/{emoji_id}", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		id, err := canonicalID(r.PathValue("emoji_id"))
		if err != nil {
			return err
		}
		if err := backend.DeleteSiteEmoji(r.Context(), actor.ID, id); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}))
	api.mux.HandleFunc("GET /v1/forums/{forum_id}/emojis", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		id, err := canonicalID(r.PathValue("forum_id"))
		if err != nil {
			return err
		}
		items, err := backend.ListForumEmoji(r.Context(), actor.ID, id)
		if err != nil {
			return err
		}
		return writeJSON(w, 200, emoji.Page{Items: items})
	}))
	api.mux.HandleFunc("POST /v1/forums/{forum_id}/emojis", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		id, err := canonicalID(r.PathValue("forum_id"))
		if err != nil {
			return err
		}
		key, err := idempotencyKey(r)
		if err != nil {
			return err
		}
		name, image, err := api.readEmojiUpload(w, r)
		if err != nil {
			return err
		}
		result, err := backend.CreateForumEmoji(r.Context(), actor.ID, id, key, name, image)
		if err != nil {
			return err
		}
		w.Header().Set("Location", "/v1/emojis/"+result.ID)
		return writeJSON(w, 201, result)
	}))
	api.mux.HandleFunc("DELETE /v1/forums/{forum_id}/emojis/{emoji_id}", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		forumID, err := canonicalID(r.PathValue("forum_id"))
		if err != nil {
			return err
		}
		id, err := canonicalID(r.PathValue("emoji_id"))
		if err != nil {
			return err
		}
		if err := backend.DeleteForumEmoji(r.Context(), actor.ID, forumID, id); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}))
}

func (api *API) readEmojiUpload(w http.ResponseWriter, r *http.Request) (string, emoji.Image, error) {
	var none emoji.Image
	limits := api.emojiLimits
	if err := limits.Valid(); err != nil {
		return "", none, err
	}
	name, file, err := readEmojiParts(w, r, limits.MaxBytes)
	if err != nil {
		return "", none, err
	}
	if err := emoji.ValidName(name); err != nil {
		return "", none, err
	}
	image, err := emoji.Prepare(file, limits)
	if err != nil {
		return "", none, err
	}
	return name, image, nil
}

func readEmojiParts(w http.ResponseWriter, r *http.Request, maxBytes int64) (string, []byte, error) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		return "", nil, errBadRequest
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+16*1024)
	reader, err := r.MultipartReader()
	if err != nil {
		return "", nil, uploadReadError(err)
	}
	var name string
	var file []byte
	var sawName, sawFile bool
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", nil, uploadReadError(err)
		}
		switch part.FormName() {
		case "name":
			if sawName {
				return "", nil, errBadRequest
			}
			raw, err := readPart(part, 64)
			if err != nil {
				return "", nil, err
			}
			name = string(raw)
			sawName = true
		case "file":
			if sawFile {
				return "", nil, errBadRequest
			}
			raw, err := readPart(part, maxBytes+1)
			if err != nil {
				return "", nil, err
			}
			if int64(len(raw)) > maxBytes {
				return "", nil, media.ErrTooLarge
			}
			file = raw
			sawFile = true
		default:
			return "", nil, errBadRequest
		}
	}
	if !sawName || !sawFile || strings.TrimSpace(name) != name {
		return "", nil, errBadRequest
	}
	return name, file, nil
}

func readPart(part *multipart.Part, limit int64) ([]byte, error) {
	defer part.Close()
	raw, err := io.ReadAll(io.LimitReader(part, limit))
	if err != nil {
		return nil, uploadReadError(err)
	}
	return raw, nil
}

func uploadReadError(err error) error {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return errBodyTooLarge
	}
	return errBadRequest
}
