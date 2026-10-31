package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/identity"
)

// CollaborationBackend is the membership and identity-group surface used by HTTP.
type CollaborationBackend interface {
	ListGroups(context.Context) ([]identity.Group, error)
	AssignGroup(context.Context, string, string, string) error
	RemoveGroup(context.Context, string, string, string) (bool, error)
	ListMembers(context.Context, string, string, string, int) ([]forum.Member, error)
	AddMember(context.Context, string, string, forum.MemberChange) (forum.Member, error)
	SetMemberRole(context.Context, string, string, string, string) (forum.Member, error)
	RemoveMember(context.Context, string, string, string) (bool, error)
	EffectivePermissions(context.Context, string, string) (identity.Effective, error)
}

func (api *API) RegisterCollaboration(backend CollaborationBackend) {
	api.mux.HandleFunc("GET /v1/identity/groups", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		items, err := backend.ListGroups(r.Context())
		if err != nil {
			return err
		}
		if items == nil {
			items = []identity.Group{}
		}
		return writeJSON(w, http.StatusOK, struct {
			Items []identity.Group `json:"items"`
		}{Items: items})
	}))
	api.mux.HandleFunc("PUT /v1/identity/groups/{group_id}/members/{user_id}", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		groupID, userID, err := groupMemberIDs(r)
		if err != nil {
			return err
		}
		if err := backend.AssignGroup(r.Context(), actor.ID, groupID, userID); err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, struct {
			UserID  string `json:"user_id"`
			GroupID string `json:"group_id"`
		}{UserID: userID, GroupID: groupID})
	}))
	api.mux.HandleFunc("DELETE /v1/identity/groups/{group_id}/members/{user_id}", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		groupID, userID, err := groupMemberIDs(r)
		if err != nil {
			return err
		}
		removed, err := backend.RemoveGroup(r.Context(), actor.ID, groupID, userID)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, struct {
			Removed bool `json:"removed"`
		}{Removed: removed})
	}))
	api.mux.HandleFunc("GET /v1/forums/{forum_id}/members", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		forumID, err := canonicalID(r.PathValue("forum_id"))
		if err != nil {
			return err
		}
		after, limit, err := memberPage(r)
		if err != nil {
			return err
		}
		items, err := backend.ListMembers(r.Context(), actor.ID, forumID, after, limit)
		if err != nil {
			return err
		}
		response := struct {
			Items []forum.Member `json:"items"`
			Next  string         `json:"next,omitempty"`
		}{Items: items}
		if response.Items == nil {
			response.Items = []forum.Member{}
		}
		if len(items) > limit {
			response.Items = items[:limit]
			response.Next = items[limit-1].UserID
		}
		return writeJSON(w, http.StatusOK, response)
	}))
	api.mux.HandleFunc("POST /v1/forums/{forum_id}/members", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		forumID, err := canonicalID(r.PathValue("forum_id"))
		if err != nil {
			return err
		}
		change, err := readJSON[forum.MemberChange](w, r)
		if err != nil {
			return err
		}
		change.UserID, err = canonicalID(change.UserID)
		if err != nil {
			return err
		}
		if change.RoleID != "" {
			change.RoleID, err = canonicalID(change.RoleID)
			if err != nil {
				return err
			}
		}
		member, err := backend.AddMember(r.Context(), actor.ID, forumID, change)
		if err != nil {
			return err
		}
		w.Header().Set("Location", "/v1/forums/"+forumID+"/members/"+member.UserID)
		return writeJSON(w, http.StatusCreated, member)
	}))
	api.mux.HandleFunc("PUT /v1/forums/{forum_id}/members/{user_id}/role", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		forumID, err := canonicalID(r.PathValue("forum_id"))
		if err != nil {
			return err
		}
		userID, err := canonicalID(r.PathValue("user_id"))
		if err != nil {
			return err
		}
		body, err := readJSON[struct {
			RoleID string `json:"role_id"`
		}](w, r)
		if err != nil {
			return err
		}
		roleID, err := canonicalID(body.RoleID)
		if err != nil {
			return err
		}
		member, err := backend.SetMemberRole(r.Context(), actor.ID, forumID, userID, roleID)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, member)
	}))
	api.mux.HandleFunc("DELETE /v1/forums/{forum_id}/members/{user_id}", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		forumID, err := canonicalID(r.PathValue("forum_id"))
		if err != nil {
			return err
		}
		userID, err := canonicalID(r.PathValue("user_id"))
		if err != nil {
			return err
		}
		removed, err := backend.RemoveMember(r.Context(), actor.ID, forumID, userID)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, struct {
			Removed bool `json:"removed"`
		}{Removed: removed})
	}))
	api.mux.HandleFunc("GET /v1/forums/{forum_id}/permissions", api.authenticated(func(w http.ResponseWriter, r *http.Request, actor identity.Actor) error {
		forumID, err := canonicalID(r.PathValue("forum_id"))
		if err != nil {
			return err
		}
		effective, err := backend.EffectivePermissions(r.Context(), actor.ID, forumID)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, effective)
	}))
}

func groupMemberIDs(r *http.Request) (string, string, error) {
	groupID, err := canonicalID(r.PathValue("group_id"))
	if err != nil {
		return "", "", err
	}
	userID, err := canonicalID(r.PathValue("user_id"))
	if err != nil {
		return "", "", err
	}
	return groupID, userID, nil
}

func memberPage(r *http.Request) (string, int, error) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > forum.MaxPageSize {
			return "", 0, errBadRequest
		}
		limit = parsed
	}
	after := r.URL.Query().Get("after")
	if after == "" {
		return "", limit, nil
	}
	canonical, err := canonicalID(after)
	if err != nil {
		return "", 0, err
	}
	return canonical, limit, nil
}
