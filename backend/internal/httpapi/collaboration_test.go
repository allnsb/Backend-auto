package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/identity"
)

func TestCollaborationRoutesAndPermissionGate(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(openAPI)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	actor := identity.User{ID: "dc9de165-b912-4dcb-8620-8dfdb30176b7", CreatedAt: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)}
	other := "6d2f0c2e-1b7a-4e4e-9c1a-2f6b0d9a7c31"
	forumID := "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	store := &collaborationFake{actor: actor.ID}
	api := NewAPI(func(_ context.Context, raw string) (identity.Authentication, error) {
		if raw != "valid-access-token" {
			return identity.Authentication{}, identity.ErrNotFound
		}
		return identity.Authentication{ExternalID: identity.ExternalID{Issuer: "https://issuer.example", Subject: "verified-subject"}, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}, func(_ context.Context, got identity.ExternalID) (identity.User, error) {
		if got.Subject != "verified-subject" {
			t.Fatalf("directory saw %s", got.Subject)
		}
		return actor, nil
	})
	api.RegisterCollaboration(store)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer valid-access-token")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		return w
	}
	denied := call(http.MethodPost, "/v1/forums/"+forumID+"/members", `{"user_id":"`+other+`"}`)
	if denied.Code != http.StatusForbidden || store.adds != 0 {
		t.Fatalf("member without manage grant status=%d adds=%d body=%s", denied.Code, store.adds, denied.Body.String())
	}
	assertProblem(t, doc, denied, "permission_denied")
	store.allow = true
	created := call(http.MethodPost, "/v1/forums/"+forumID+"/members", `{"user_id":"`+other+`"}`)
	if created.Code != http.StatusCreated || store.adds != 1 || store.roleID != identity.MemberGroupID {
		t.Fatalf("add member status=%d adds=%d role=%s", created.Code, store.adds, store.roleID)
	}
	assertSchema(t, doc, "ForumMember", created.Body.Bytes())
	listed := call(http.MethodGet, "/v1/forums/"+forumID+"/members", "")
	if listed.Code != http.StatusOK {
		t.Fatal(listed.Body.String())
	}
	assertSchema(t, doc, "ForumMemberPage", listed.Body.Bytes())
	permissions := call(http.MethodGet, "/v1/forums/"+forumID+"/permissions", "")
	if permissions.Code != http.StatusOK {
		t.Fatal(permissions.Body.String())
	}
	assertSchema(t, doc, "EffectivePermissions", permissions.Body.Bytes())
	var effective identity.Effective
	if err := json.Unmarshal(permissions.Body.Bytes(), &effective); err != nil {
		t.Fatal(err)
	}
	if effective.GroupID != identity.OwnerGroupID || !containsGrant(effective.Grants, identity.GrantDelete) || !containsGrant(effective.Grants, identity.GrantPost) {
		t.Fatalf("effective = %+v", effective)
	}
	if containsGrant(effective.Grants, "from-lower-role") {
		t.Fatal("effective grants included a grant outside the winning role")
	}
	role := call(http.MethodPut, "/v1/forums/"+forumID+"/members/"+other+"/role", `{"role_id":"`+identity.ModeratorGroupID+`"}`)
	if role.Code != http.StatusOK || store.roleID != identity.ModeratorGroupID {
		t.Fatalf("set role status=%d role=%s body=%s", role.Code, store.roleID, role.Body.String())
	}
	removed := call(http.MethodDelete, "/v1/forums/"+forumID+"/members/"+other, "")
	if removed.Code != http.StatusOK || !store.removed {
		t.Fatalf("remove status=%d body=%s", removed.Code, removed.Body.String())
	}
	groups := call(http.MethodGet, "/v1/identity/groups", "")
	if groups.Code != http.StatusOK {
		t.Fatal(groups.Body.String())
	}
	assertSchema(t, doc, "IdentityGroupPage", groups.Body.Bytes())
	assigned := call(http.MethodPut, "/v1/identity/groups/"+identity.OwnerGroupID+"/members/"+other, "")
	if assigned.Code != http.StatusOK || store.assignedUser != other {
		t.Fatalf("assign status=%d user=%s", assigned.Code, store.assignedUser)
	}
	store.allow = false
	blocked := call(http.MethodDelete, "/v1/identity/groups/"+identity.OwnerGroupID+"/members/"+other, "")
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("group removal status=%d", blocked.Code)
	}
}

func containsGrant(grants []string, grant string) bool {
	for _, item := range grants {
		if item == grant {
			return true
		}
	}
	return false
}

func assertProblem(t *testing.T, doc *openapi3.T, w *httptest.ResponseRecorder, code string) {
	t.Helper()
	var problem Problem
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != code || problem.Status != w.Code {
		t.Fatalf("problem = %+v", problem)
	}
	assertSchema(t, doc, "Problem", w.Body.Bytes())
}

func assertSchema(t *testing.T, doc *openapi3.T, name string, body []byte) {
	t.Helper()
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	if err := doc.Components.Schemas[name].Value.VisitJSON(value); err != nil {
		t.Fatalf("%s schema: %v body=%s", name, err, body)
	}
}

type collaborationFake struct {
	actor        string
	allow        bool
	adds         int
	roleID       string
	removed      bool
	assignedUser string
}

func (f *collaborationFake) ListGroups(context.Context) ([]identity.Group, error) {
	return []identity.Group{{
		ID: identity.MemberGroupID, Name: "member", Priority: 100,
		Grants: identity.GrantsFromFlags(true, true, false, false, false),
	}, {
		ID: identity.OwnerGroupID, Name: "owner", Priority: 300,
		Grants: identity.GrantsFromFlags(true, true, true, true, true),
	}}, nil
}

func (f *collaborationFake) AssignGroup(_ context.Context, actorID, _, userID string) error {
	if actorID != f.actor || !f.allow {
		return forum.ErrForbidden
	}
	f.assignedUser = userID
	return nil
}

func (f *collaborationFake) RemoveGroup(_ context.Context, actorID, _, _ string) (bool, error) {
	if actorID != f.actor || !f.allow {
		return false, forum.ErrForbidden
	}
	return true, nil
}

func (f *collaborationFake) ListMembers(context.Context, string, string, string, int) ([]forum.Member, error) {
	return []forum.Member{{
		UserID: f.actor, RoleID: identity.MemberGroupID, RoleName: "member", Priority: 100,
	}}, nil
}

func (f *collaborationFake) AddMember(_ context.Context, actorID, _ string, change forum.MemberChange) (forum.Member, error) {
	if actorID != f.actor || !f.allow {
		return forum.Member{}, forum.ErrForbidden
	}
	f.adds++
	roleID := change.RoleID
	if roleID == "" {
		roleID = identity.MemberGroupID
	}
	f.roleID = roleID
	return forum.Member{UserID: change.UserID, RoleID: roleID, RoleName: "member", Priority: 100}, nil
}

func (f *collaborationFake) SetMemberRole(_ context.Context, actorID, _, userID, roleID string) (forum.Member, error) {
	if actorID != f.actor || !f.allow {
		return forum.Member{}, forum.ErrForbidden
	}
	f.roleID = roleID
	return forum.Member{UserID: userID, RoleID: roleID, RoleName: "moderator", Priority: 200}, nil
}

func (f *collaborationFake) RemoveMember(_ context.Context, actorID, _, _ string) (bool, error) {
	if actorID != f.actor || !f.allow {
		return false, forum.ErrForbidden
	}
	f.removed = true
	return true, nil
}

func (f *collaborationFake) EffectivePermissions(_ context.Context, _, forumID string) (identity.Effective, error) {
	winner, _ := identity.Resolve([]identity.Group{
		{ID: identity.MemberGroupID, Name: "member", Priority: 100, Grants: identity.GrantsFromFlags(true, true, false, false, false)},
		{ID: identity.ModeratorGroupID, Name: "moderator", Priority: 200, Grants: []string{identity.GrantDelete, "from-lower-role"}},
		{ID: identity.OwnerGroupID, Name: "owner", Priority: 300, Grants: identity.GrantsFromFlags(true, true, true, true, true)},
	})
	return identity.Effective{
		ForumID: forumID, GroupID: winner.ID, GroupName: winner.Name, Priority: winner.Priority, Grants: winner.Grants,
	}, nil
}

func TestAuthenticatedDirectoryBootstrapsTheLocalUser(t *testing.T) {
	created := identity.User{ID: "dc9de165-b912-4dcb-8620-8dfdb30176b7", CreatedAt: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)}
	calls := 0
	api := NewAPI(func(_ context.Context, raw string) (identity.Authentication, error) {
		if raw != "valid-access-token" {
			return identity.Authentication{}, io.EOF
		}
		return identity.Authentication{ExternalID: identity.ExternalID{Issuer: "https://issuer.example", Subject: "new-subject"}, ExpiresAt: time.Now().Add(time.Minute)}, nil
	}, func(_ context.Context, external identity.ExternalID) (identity.User, error) {
		calls++
		if external.Subject != "new-subject" {
			t.Fatalf("bootstrap subject = %s", external.Subject)
		}
		return created, nil
	})
	for range 2 {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
		r.Header.Set("Authorization", "Bearer valid-access-token")
		api.ServeHTTP(w, r)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), created.ID) {
			t.Fatalf("bootstrap status=%d body=%s", w.Code, w.Body.String())
		}
	}
	if calls != 2 {
		t.Fatalf("directory calls = %d", calls)
	}
}
