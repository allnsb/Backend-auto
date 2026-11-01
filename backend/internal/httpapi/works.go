package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"odysseia.local/backend/internal/identity"
	"odysseia.local/backend/internal/work"
)

type WorkBackend interface {
	Create(context.Context, string, string, work.Draft) (work.Work, bool, error)
	Get(context.Context, string, string) (work.Work, error)
	List(context.Context, string, time.Time, string, int) ([]work.Work, error)
	Update(context.Context, string, string, work.Update) (work.Work, error)
}

// RegisterWorks exposes author-private drafts only. Neither a forum role nor
// a client-supplied author ID grants access to another user's draft.
func (api *API) RegisterWorks(store WorkBackend) {
	api.mux.HandleFunc("POST /v1/works", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		key, err := idempotencyKey(r)
		if err != nil {
			return err
		}
		draft, err := readJSON[work.Draft](w, r)
		if err != nil {
			return err
		}
		created, fresh, err := store.Create(r.Context(), actor.ID, key, draft)
		if err != nil {
			return err
		}
		w.Header().Set("Location", "/v1/works/"+created.ID)
		status := http.StatusOK
		if fresh {
			status = http.StatusCreated
		}
		return writeJSON(w, status, created)
	}))
	api.mux.HandleFunc("GET /v1/me/works", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		before, id, limit, err := readActivityPage(r, "works_created", actor.ID)
		if err != nil {
			return err
		}
		items, err := store.List(r.Context(), actor.ID, before, id, limit)
		if err != nil {
			return err
		}
		response := struct {
			Items []work.Work `json:"items"`
			Next  string      `json:"next,omitempty"`
		}{Items: items}
		if response.Items == nil {
			response.Items = []work.Work{}
		}
		if len(items) > limit {
			response.Items = items[:limit]
			last := items[limit-1]
			encoded, err := json.Marshal(activityCursor{Version: 1, Kind: "works_created", Scope: actor.ID, At: last.CreatedAt, ID: last.ID})
			if err != nil {
				return err
			}
			response.Next = base64.RawURLEncoding.EncodeToString(encoded)
		}
		return writeJSON(w, http.StatusOK, response)
	}))
	api.mux.HandleFunc("GET /v1/works/{work_id}", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		id, err := canonicalID(r.PathValue("work_id"))
		if err != nil {
			return err
		}
		found, err := store.Get(r.Context(), actor.ID, id)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, found)
	}))
	api.mux.HandleFunc("PUT /v1/works/{work_id}", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		id, err := canonicalID(r.PathValue("work_id"))
		if err != nil {
			return err
		}
		change, err := readJSON[work.Update](w, r)
		if err != nil {
			return err
		}
		updated, err := store.Update(r.Context(), actor.ID, id, change)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, updated)
	}))
}
