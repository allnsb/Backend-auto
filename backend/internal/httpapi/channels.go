package httpapi

import (
	"context"
	"net/http"

	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/identity"
	"odysseia.local/backend/internal/message"
)

type ChannelBackend interface {
	GetChannel(context.Context, string, string) (forum.Channel, error)
	ListChannels(context.Context, string, string, int64, int) ([]forum.Channel, error)
	CreateChannel(context.Context, string, string, string, forum.ChannelDraft) (forum.Channel, error)
	ListChannelMessages(context.Context, string, string, int64, int) ([]message.Message, error)
	SendChannelMessage(context.Context, string, string, string, forum.ChannelMessageDraft) (message.Message, error)
}

func (api *API) RegisterChannels(backend ChannelBackend) {
	api.mux.HandleFunc("GET /v1/forums/{forum_id}/channels", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		id, err := canonicalID(r.PathValue("forum_id"))
		if err != nil {
			return err
		}
		after, limit, err := readPage(r, "channels", id)
		if err != nil {
			return err
		}
		items, err := backend.ListChannels(r.Context(), actor.ID, id, after, limit)
		if err != nil {
			return err
		}
		return writePage(w, "channels", id, items, limit, func(c forum.Channel) int64 { return c.Position })
	}))
	api.mux.HandleFunc("POST /v1/forums/{forum_id}/channels", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		id, err := canonicalID(r.PathValue("forum_id"))
		if err != nil {
			return err
		}
		key, err := idempotencyKey(r)
		if err != nil {
			return err
		}
		draft, err := readJSON[forum.ChannelDraft](w, r)
		if err != nil {
			return err
		}
		result, err := backend.CreateChannel(r.Context(), actor.ID, id, key, draft)
		if err != nil {
			return err
		}
		w.Header().Set("Location", "/v1/channels/"+result.ID)
		return writeJSON(w, 201, result)
	}))
	api.mux.HandleFunc("GET /v1/channels/{channel_id}", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		id, err := canonicalID(r.PathValue("channel_id"))
		if err != nil {
			return err
		}
		result, err := backend.GetChannel(r.Context(), actor.ID, id)
		if err != nil {
			return err
		}
		return writeJSON(w, 200, result)
	}))
	api.mux.HandleFunc("GET /v1/channels/{channel_id}/messages", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		id, err := canonicalID(r.PathValue("channel_id"))
		if err != nil {
			return err
		}
		after, limit, err := readPage(r, "channel_messages", id)
		if err != nil {
			return err
		}
		items, err := backend.ListChannelMessages(r.Context(), actor.ID, id, after, limit)
		if err != nil {
			return err
		}
		return writePage(w, "channel_messages", id, items, limit, func(m message.Message) int64 { return m.Sequence })
	}))
	api.mux.HandleFunc("POST /v1/channels/{channel_id}/messages", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		id, err := canonicalID(r.PathValue("channel_id"))
		if err != nil {
			return err
		}
		key, err := idempotencyKey(r)
		if err != nil {
			return err
		}
		draft, err := readJSON[forum.ChannelMessageDraft](w, r)
		if err != nil {
			return err
		}
		draft.QuoteID, err = optionalID(draft.QuoteID)
		if err != nil {
			return err
		}
		result, err := backend.SendChannelMessage(r.Context(), actor.ID, id, key, draft)
		if err != nil {
			return err
		}
		return writeJSON(w, 201, result)
	}))
}

func optionalID(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	return canonicalID(value)
}
