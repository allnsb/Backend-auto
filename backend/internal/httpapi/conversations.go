package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"odysseia.local/backend/internal/conversation"
	"odysseia.local/backend/internal/identity"
	"odysseia.local/backend/internal/media"
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
	UploadConversationAttachment(ctx context.Context, actor, conversationID string, prepared media.Prepared) (message.Attachment, error)
	OpenConversationAttachment(ctx context.Context, actor, conversationID, attachmentID string) (string, error)
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
	api.mux.HandleFunc("POST /v1/conversations/{conversation_id}/attachments", api.authenticatedWithin(2*time.Minute, func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		if err := extendUploadDeadline(w); err != nil {
			return err
		}
		id, err := canonicalID(r.PathValue("conversation_id"))
		if err != nil {
			return err
		}
		prepared, err := api.readAttachment(w, r)
		if err != nil {
			return err
		}
		result, err := backend.UploadConversationAttachment(r.Context(), actor.ID, id, prepared)
		if err != nil {
			return err
		}
		w.Header().Set("Location", "/v1/conversations/"+id+"/attachments/"+result.ID)
		return writeJSON(w, http.StatusCreated, result)
	}))
	api.mux.HandleFunc("GET /v1/conversations/{conversation_id}/attachments/{attachment_id}", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		id, err := canonicalID(r.PathValue("conversation_id"))
		if err != nil {
			return err
		}
		attachmentID, err := canonicalID(r.PathValue("attachment_id"))
		if err != nil {
			return err
		}
		target, err := backend.OpenConversationAttachment(r.Context(), actor.ID, id, attachmentID)
		if err != nil {
			return err
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.Redirect(w, r, target, http.StatusFound)
		return nil
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

// extendUploadDeadline replaces the server's 10s read timeout and 30s write
// timeout for this response only. Other routes keep those limits.
func extendUploadDeadline(w http.ResponseWriter) error {
	deadline := time.Now().Add(2 * time.Minute)
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if err := controller.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

func (api *API) readAttachment(w http.ResponseWriter, r *http.Request) (media.Prepared, error) {
	limit := api.mediaLimits.MaxBytes
	r.Body = http.MaxBytesReader(w, r.Body, limit+64*1024)
	if err := r.ParseMultipartForm(limit + 64*1024); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return media.Prepared{}, errBodyTooLarge
		}
		return media.Prepared{}, errBadRequest
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		return media.Prepared{}, errBadRequest
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return media.Prepared{}, errBadRequest
	}
	if int64(len(data)) > limit {
		return media.Prepared{}, errBodyTooLarge
	}
	declared := 0
	if raw := r.FormValue("duration_ms"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			return media.Prepared{}, errBadRequest
		}
		declared = parsed
	}
	return media.Inspect(data, declared, api.mediaLimits)
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
