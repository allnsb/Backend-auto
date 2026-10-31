package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"odysseia.local/backend/internal/conversation"
	"odysseia.local/backend/internal/identity"
	"odysseia.local/backend/internal/message"
)

// ConversationBackend is the text conversation slice consumed by HTTP.
type ConversationBackend interface {
	OpenDM(ctx context.Context, actor, requestID string, draft conversation.DMDraft) (conversation.Conversation, bool, error)
	CreateGroup(ctx context.Context, actor, requestID string, draft conversation.GroupDraft) (conversation.Conversation, error)
	GetConversation(ctx context.Context, actor, id string) (conversation.Conversation, error)
	ListConversations(ctx context.Context, actor string, after time.Time, afterID string, limit int) ([]conversation.Conversation, error)
	ListConversationMessages(ctx context.Context, actor, id string, after int64, limit int) ([]message.Message, error)
	SendConversationMessage(ctx context.Context, actor, id, requestID string, draft conversation.MessageDraft) (message.Message, error)
	AddConversationMember(ctx context.Context, actor, id, userID string) (conversation.Member, error)
	RemoveConversationMember(ctx context.Context, actor, id, userID string) error
	TransferConversationOwner(ctx context.Context, actor, id, userID string) (conversation.Conversation, error)
	DissolveConversation(ctx context.Context, actor, id string) error
	MarkConversationRead(ctx context.Context, actor, id string, position int64) (conversation.ReadState, error)
	ListUnread(ctx context.Context, actor string) (conversation.UnreadSummary, error)
}

func (api *API) RegisterConversations(backend ConversationBackend) {
	api.mux.HandleFunc("POST /v1/conversations/dm", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		requestID, err := idempotencyKey(r)
		if err != nil {
			return err
		}
		draft, err := readJSON[conversation.DMDraft](w, r)
		if err != nil {
			return err
		}
		draft.UserID, err = canonicalID(draft.UserID)
		if err != nil {
			return err
		}
		result, owned, err := backend.OpenDM(r.Context(), actor.ID, requestID, draft)
		if err != nil {
			return err
		}
		w.Header().Set("Location", "/v1/conversations/"+result.ID)
		status := http.StatusOK
		if owned {
			status = http.StatusCreated
		}
		return writeJSON(w, status, result)
	}))
	api.mux.HandleFunc("POST /v1/conversations/groups", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		requestID, err := idempotencyKey(r)
		if err != nil {
			return err
		}
		draft, err := readJSON[conversation.GroupDraft](w, r)
		if err != nil {
			return err
		}
		for index, id := range draft.MemberIDs {
			draft.MemberIDs[index], err = canonicalID(id)
			if err != nil {
				return err
			}
		}
		result, err := backend.CreateGroup(r.Context(), actor.ID, requestID, draft)
		if err != nil {
			return err
		}
		w.Header().Set("Location", "/v1/conversations/"+result.ID)
		return writeJSON(w, http.StatusCreated, result)
	}))
	api.mux.HandleFunc("GET /v1/conversations", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		after, afterID, limit, err := readActivityPage(r, "conversations", actor.ID)
		if err != nil {
			return err
		}
		items, err := backend.ListConversations(r.Context(), actor.ID, after, afterID, limit)
		if err != nil {
			return err
		}
		return writeActivityPage(w, "conversations", actor.ID, items, limit)
	}))
	api.mux.HandleFunc("GET /v1/conversations/{conversation_id}", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		id, err := canonicalID(r.PathValue("conversation_id"))
		if err != nil {
			return err
		}
		result, err := backend.GetConversation(r.Context(), actor.ID, id)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, result)
	}))
	api.mux.HandleFunc("GET /v1/conversations/{conversation_id}/messages", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		id, err := canonicalID(r.PathValue("conversation_id"))
		if err != nil {
			return err
		}
		after, limit, err := readPage(r, "conversation_messages", id)
		if err != nil {
			return err
		}
		items, err := backend.ListConversationMessages(r.Context(), actor.ID, id, after, limit)
		if err != nil {
			return err
		}
		return writePage(w, "conversation_messages", id, items, limit, func(item message.Message) int64 { return item.Sequence })
	}))
	api.mux.HandleFunc("POST /v1/conversations/{conversation_id}/messages", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		id, err := canonicalID(r.PathValue("conversation_id"))
		if err != nil {
			return err
		}
		requestID, err := idempotencyKey(r)
		if err != nil {
			return err
		}
		draft, err := readJSON[conversation.MessageDraft](w, r)
		if err != nil {
			return err
		}
		draft.QuoteID, err = optionalID(draft.QuoteID)
		if err != nil {
			return err
		}
		result, err := backend.SendConversationMessage(r.Context(), actor.ID, id, requestID, draft)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusCreated, result)
	}))
	api.mux.HandleFunc("PUT /v1/conversations/{conversation_id}/members/{user_id}", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		if r.ContentLength != 0 {
			return errBadRequest
		}
		id, userID, err := conversationUser(r)
		if err != nil {
			return err
		}
		member, err := backend.AddConversationMember(r.Context(), actor.ID, id, userID)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, member)
	}))
	api.mux.HandleFunc("DELETE /v1/conversations/{conversation_id}/members/{user_id}", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		if r.ContentLength != 0 {
			return errBadRequest
		}
		id, userID, err := conversationUser(r)
		if err != nil {
			return err
		}
		if err := backend.RemoveConversationMember(r.Context(), actor.ID, id, userID); err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, struct {
			Removed bool `json:"removed"`
		}{Removed: true})
	}))
	api.mux.HandleFunc("PUT /v1/conversations/{conversation_id}/owner", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		id, err := canonicalID(r.PathValue("conversation_id"))
		if err != nil {
			return err
		}
		body, err := readJSON[struct {
			UserID string `json:"user_id"`
		}](w, r)
		if err != nil {
			return err
		}
		userID, err := canonicalID(body.UserID)
		if err != nil {
			return err
		}
		result, err := backend.TransferConversationOwner(r.Context(), actor.ID, id, userID)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, result)
	}))
	api.mux.HandleFunc("PUT /v1/conversations/{conversation_id}/read", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		id, err := canonicalID(r.PathValue("conversation_id"))
		if err != nil {
			return err
		}
		body, err := readJSON[struct {
			Position int64 `json:"position,string"`
		}](w, r)
		if err != nil {
			return err
		}
		result, err := backend.MarkConversationRead(r.Context(), actor.ID, id, body.Position)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, result)
	}))
	api.mux.HandleFunc("DELETE /v1/conversations/{conversation_id}", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		if r.ContentLength != 0 {
			return errBadRequest
		}
		id, err := canonicalID(r.PathValue("conversation_id"))
		if err != nil {
			return err
		}
		if err := backend.DissolveConversation(r.Context(), actor.ID, id); err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, struct {
			Dissolved bool `json:"dissolved"`
		}{Dissolved: true})
	}))
	api.mux.HandleFunc("GET /v1/me/unread", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		result, err := backend.ListUnread(r.Context(), actor.ID)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, result)
	}))
	api.mux.HandleFunc("POST /v1/me/inbox/subscription", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		if r.ContentLength != 0 {
			return errBadRequest
		}
		return writeJSON(w, http.StatusOK, subscription{Channel: conversation.InboxChannel(actor.ID)})
	}))
}

func conversationUser(r *http.Request) (string, string, error) {
	id, err := canonicalID(r.PathValue("conversation_id"))
	if err != nil {
		return "", "", err
	}
	userID, err := canonicalID(r.PathValue("user_id"))
	if err != nil {
		return "", "", err
	}
	return id, userID, nil
}

type activityCursor struct {
	Version int       `json:"v"`
	Kind    string    `json:"kind"`
	Scope   string    `json:"scope"`
	At      time.Time `json:"at"`
	ID      string    `json:"id"`
}

func readActivityPage(r *http.Request, kind, scope string) (time.Time, string, int, error) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > conversation.MaxPageSize {
			return time.Time{}, "", 0, errBadRequest
		}
		limit = parsed
	}
	raw := r.URL.Query().Get("after")
	if raw == "" {
		return time.Time{}, "", limit, nil
	}
	if len(raw) > 512 {
		return time.Time{}, "", 0, errBadRequest
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, "", 0, errBadRequest
	}
	var cursor activityCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return time.Time{}, "", 0, errBadRequest
	}
	if cursor.Version != 1 || cursor.Kind != kind || cursor.Scope != scope || cursor.At.IsZero() {
		return time.Time{}, "", 0, errBadRequest
	}
	id, err := canonicalID(cursor.ID)
	if err != nil {
		return time.Time{}, "", 0, errBadRequest
	}
	return cursor.At, id, limit, nil
}

func writeActivityPage(w http.ResponseWriter, kind, scope string, items []conversation.Conversation, limit int) error {
	response := struct {
		Items []conversation.Conversation `json:"items"`
		Next  string                      `json:"next,omitempty"`
	}{Items: items}
	if len(items) > limit {
		response.Items = items[:limit]
		last := items[limit-1]
		encoded, err := json.Marshal(activityCursor{Version: 1, Kind: kind, Scope: scope, At: last.ActivityAt, ID: last.ID})
		if err != nil {
			return err
		}
		response.Next = base64.RawURLEncoding.EncodeToString(encoded)
	}
	return writeJSON(w, http.StatusOK, response)
}
