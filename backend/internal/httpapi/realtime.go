package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/identity"
	"odysseia.local/backend/internal/realtime"
)

// SubscriptionBackend checks the current visibility of each product resource.
type SubscriptionBackend interface {
	GetForum(context.Context, string, string) (forum.Forum, error)
	GetTopic(context.Context, string, string) (forum.Topic, error)
	GetChannel(context.Context, string, string) (forum.Channel, error)
}

// RegisterRealtime serves identity tokens, descriptors and the private callback.
// Descriptors grant no access; subscriptions and recovery recheck membership.
func (api *API) RegisterRealtime(tokens *realtime.Signer, forums SubscriptionBackend, proxySecret string) error {
	if len(proxySecret) < 32 {
		return errors.New("realtime proxy secret must contain at least 32 bytes")
	}
	api.mux.HandleFunc("POST /v1/realtime/connection-token", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		if r.ContentLength != 0 {
			return errBadRequest
		}
		token, err := tokens.Connection(actor)
		return writeRealtimeToken(w, token, err)
	}))
	api.mux.HandleFunc("POST /v1/forums/{forum_id}/subscription", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		if r.ContentLength != 0 {
			return errBadRequest
		}
		id, err := canonicalID(r.PathValue("forum_id"))
		if err != nil {
			return err
		}
		if _, err := forums.GetForum(r.Context(), actor.ID, id); err != nil {
			return err
		}
		return writeJSON(w, 200, subscription{Channel: "forum:" + id})
	}))
	api.mux.HandleFunc("POST /v1/topics/{topic_id}/subscription", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		if r.ContentLength != 0 {
			return errBadRequest
		}
		id, err := canonicalID(r.PathValue("topic_id"))
		if err != nil {
			return err
		}
		if _, err := forums.GetTopic(r.Context(), actor.ID, id); err != nil {
			return err
		}
		return writeJSON(w, 200, subscription{Channel: "topic:" + id})
	}))
	api.mux.HandleFunc("POST /_internal/realtime/subscribe", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		keys := r.Header.Values("X-Odysseia-Proxy-Key")
		if len(keys) != 1 || subtle.ConstantTimeCompare([]byte(keys[0]), []byte(proxySecret)) != 1 {
			writeProblem(w, 401, "invalid_token")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		request, err := readJSON[subscribeRequest](w, r)
		if err != nil {
			writeAPIError(ctx, w, err)
			return
		}
		err = authorizeSubscription(ctx, forums, request)
		if errors.Is(err, forum.ErrNotFound) {
			_ = writeJSON(w, 200, map[string]any{"error": map[string]any{"code": 103, "message": "permission denied"}})
			return
		}
		if err != nil {
			writeAPIError(ctx, w, err)
			return
		}
		_ = writeJSON(w, 200, map[string]any{"result": struct{}{}})
	})
	api.mux.HandleFunc("POST /v1/channels/{channel_id}/subscription", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		if r.ContentLength != 0 {
			return errBadRequest
		}
		id, err := canonicalID(r.PathValue("channel_id"))
		if err != nil {
			return err
		}
		if _, err := forums.GetChannel(r.Context(), actor.ID, id); err != nil {
			return err
		}
		return writeJSON(w, 200, subscription{Channel: "channel:" + id})
	}))
	return nil
}

type subscription struct {
	Channel string `json:"channel"`
}

// Provider fields are decoded here; client-supplied data never supplies the actor or grants.
type subscribeRequest struct {
	Client    string            `json:"client"`
	Transport string            `json:"transport"`
	Protocol  string            `json:"protocol"`
	Encoding  string            `json:"encoding"`
	User      string            `json:"user"`
	Channel   string            `json:"channel"`
	Token     string            `json:"token"`
	Meta      json.RawMessage   `json:"meta"`
	Data      json.RawMessage   `json:"data"`
	B64Data   string            `json:"b64data"`
	Labels    map[string]string `json:"labels"`
}

func authorizeSubscription(ctx context.Context, forums SubscriptionBackend, request subscribeRequest) error {
	actor, err := canonicalID(request.User)
	if err != nil || request.Token != "" {
		return forum.ErrNotFound
	}
	kind, rawID, ok := strings.Cut(request.Channel, ":")
	id, err := canonicalID(rawID)
	if !ok || err != nil || rawID != id {
		return forum.ErrNotFound
	}
	switch kind {
	case "forum":
		_, err = forums.GetForum(ctx, actor, id)
	case "topic":
		_, err = forums.GetTopic(ctx, actor, id)
	case "channel":
		_, err = forums.GetChannel(ctx, actor, id)
	default:
		return forum.ErrNotFound
	}
	return err
}

func writeRealtimeToken(w http.ResponseWriter, token realtime.Token, err error) error {
	if errors.Is(err, realtime.ErrExpired) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="odysseia", error="invalid_token"`)
		writeProblem(w, 401, "invalid_token")
		return nil
	}
	if err != nil {
		return err
	}
	return writeJSON(w, 200, token)
}
