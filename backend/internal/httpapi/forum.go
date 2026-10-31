package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/google/uuid"

	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/identity"
)

var errBadRequest = errors.New("invalid request")
var errBodyTooLarge = errors.New("request body is too large")
var errRequestTimeout = errors.New("request body timed out")

// ForumBackend is the capability consumed by HTTP, independent of SQL or realtime.
type ForumBackend interface {
	GetForum(context.Context, string, string) (forum.Forum, error)
	GetTopic(context.Context, string, string) (forum.Topic, error)
	ListTopics(context.Context, string, string, int64, int) ([]forum.Topic, error)
	ListMessages(context.Context, string, string, int64, int) ([]forum.Entry, error)
	CreateTopic(context.Context, string, string, string, forum.TopicDraft) (forum.CreatedTopic, error)
	Reply(context.Context, string, string, string, forum.ReplyDraft) (forum.Entry, error)
}

func (api *API) RegisterForums(backend ForumBackend) {
	api.mux.HandleFunc("GET /v1/forums/{forum_id}", api.authenticated(func(w http.ResponseWriter, r *http.Request, user identity.Actor) error {
		id, err := canonicalID(r.PathValue("forum_id"))
		if err != nil {
			return err
		}
		result, err := backend.GetForum(r.Context(), user.ID, id)
		if err != nil {
			return err
		}
		return writeJSON(w, 200, result)
	}))
	api.mux.HandleFunc("GET /v1/forums/{forum_id}/topics", api.authenticated(func(w http.ResponseWriter, r *http.Request, user identity.Actor) error {
		id, err := canonicalID(r.PathValue("forum_id"))
		if err != nil {
			return err
		}
		after, limit, err := readPage(r, "topics", id)
		if err != nil {
			return err
		}
		items, err := backend.ListTopics(r.Context(), user.ID, id, after, limit)
		if err != nil {
			return err
		}
		return writePage(w, "topics", id, items, limit, func(t forum.Topic) int64 { return t.Position })
	}))
	api.mux.HandleFunc("POST /v1/forums/{forum_id}/topics", api.authenticated(func(w http.ResponseWriter, r *http.Request, user identity.Actor) error {
		id, err := canonicalID(r.PathValue("forum_id"))
		if err != nil {
			return err
		}
		key, err := idempotencyKey(r)
		if err != nil {
			return err
		}
		draft, err := readJSON[forum.TopicDraft](w, r)
		if err != nil {
			return err
		}
		result, err := backend.CreateTopic(r.Context(), user.ID, id, key, draft)
		if err != nil {
			return err
		}
		w.Header().Set("Location", "/v1/topics/"+result.Topic.ID)
		return writeJSON(w, 201, result)
	}))
	api.mux.HandleFunc("GET /v1/topics/{topic_id}", api.authenticated(func(w http.ResponseWriter, r *http.Request, user identity.Actor) error {
		id, err := canonicalID(r.PathValue("topic_id"))
		if err != nil {
			return err
		}
		result, err := backend.GetTopic(r.Context(), user.ID, id)
		if err != nil {
			return err
		}
		return writeJSON(w, 200, result)
	}))
	api.mux.HandleFunc("GET /v1/topics/{topic_id}/messages", api.authenticated(func(w http.ResponseWriter, r *http.Request, user identity.Actor) error {
		id, err := canonicalID(r.PathValue("topic_id"))
		if err != nil {
			return err
		}
		after, limit, err := readPage(r, "messages", id)
		if err != nil {
			return err
		}
		items, err := backend.ListMessages(r.Context(), user.ID, id, after, limit)
		if err != nil {
			return err
		}
		return writePage(w, "messages", id, items, limit, func(e forum.Entry) int64 { return e.Sequence })
	}))
	api.mux.HandleFunc("POST /v1/topics/{topic_id}/messages", api.authenticated(func(w http.ResponseWriter, r *http.Request, user identity.Actor) error {
		id, err := canonicalID(r.PathValue("topic_id"))
		if err != nil {
			return err
		}
		key, err := idempotencyKey(r)
		if err != nil {
			return err
		}
		draft, err := readJSON[forum.ReplyDraft](w, r)
		if err != nil {
			return err
		}
		draft.RootID, err = optionalID(draft.RootID)
		if err != nil {
			return err
		}
		draft.QuoteID, err = optionalID(draft.QuoteID)
		if err != nil {
			return err
		}
		result, err := backend.Reply(r.Context(), user.ID, id, key, draft)
		if err != nil {
			return err
		}
		return writeJSON(w, 201, result)
	}))
}

func canonicalID(value string) (string, error) {
	id, err := uuid.Parse(value)
	if err != nil {
		return "", errBadRequest
	}
	return id.String(), nil
}

func idempotencyKey(r *http.Request) (string, error) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) != 1 {
		return "", errBadRequest
	}
	return canonicalID(values[0])
}

func readJSON[T any](w http.ResponseWriter, r *http.Request) (T, error) {
	var value T
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256*1024))
	var maxError *http.MaxBytesError
	if errors.As(err, &maxError) {
		return value, errBodyTooLarge
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return value, errRequestTimeout
	}
	if err != nil || !utf8.Valid(data) {
		return value, errBadRequest
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, errBadRequest
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return value, errBadRequest
	}
	return value, nil
}

type pageCursor struct {
	Version  int    `json:"v"`
	Kind     string `json:"kind"`
	Scope    string `json:"scope"`
	Position int64  `json:"position,string"`
}

func readPage(r *http.Request, kind, scope string) (int64, int, error) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > forum.MaxPageSize {
			return 0, 0, errBadRequest
		}
		limit = parsed
	}
	raw := r.URL.Query().Get("after")
	if raw == "" {
		return 0, limit, nil
	}
	if len(raw) > 512 {
		return 0, 0, errBadRequest
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, 0, errBadRequest
	}
	var cursor pageCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return 0, 0, errBadRequest
	}
	if cursor.Version != 1 || cursor.Kind != kind || cursor.Scope != scope {
		return 0, 0, errBadRequest
	}
	if cursor.Position < 1 {
		return 0, 0, errBadRequest
	}
	return cursor.Position, limit, nil
}

func writePage[T any](w http.ResponseWriter, kind, scope string, items []T, limit int, position func(T) int64) error {
	response := struct {
		Items []T    `json:"items"`
		Next  string `json:"next,omitempty"`
	}{Items: items}
	if len(items) > limit {
		response.Items = items[:limit]
		cursor := pageCursor{Version: 1, Kind: kind, Scope: scope, Position: position(items[limit-1])}
		encoded, err := json.Marshal(cursor)
		if err != nil {
			return err
		}
		response.Next = base64.RawURLEncoding.EncodeToString(encoded)
	}
	return writeJSON(w, 200, response)
}
