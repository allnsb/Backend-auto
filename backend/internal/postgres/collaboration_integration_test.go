//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/identity"
)

func testCollaboration(t *testing.T, ctx context.Context, migrationURL string, runtimePool *pgxpool.Pool) {
	t.Helper()
	ownerPool, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerPool.Close()
	_, err = ownerPool.Exec(ctx, `GRANT USAGE ON SCHEMA odysseia TO odysseia_runtime;
		GRANT SELECT, INSERT ON odysseia.users TO odysseia_runtime;
		GRANT SELECT ON odysseia.forums, odysseia.identity_groups TO odysseia_runtime;
		GRANT SELECT, INSERT, UPDATE, DELETE ON odysseia.forum_members TO odysseia_runtime;
		GRANT SELECT, INSERT, DELETE ON odysseia.user_groups TO odysseia_runtime;
		GRANT INSERT ON odysseia.forum_member_removals TO odysseia_runtime;
		GRANT INSERT (method, payload) ON odysseia.realtime_outbox TO odysseia_runtime;
		GRANT USAGE ON SEQUENCE odysseia.realtime_outbox_id_seq TO odysseia_runtime;`)
	if err != nil {
		t.Fatal(err)
	}
	users := NewUsers(runtimePool)
	alice, err := users.Ensure(ctx, identity.ExternalID{Issuer: "https://collab.example", Subject: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := users.Ensure(ctx, identity.ExternalID{Issuer: "https://collab.example", Subject: "alice"})
	if err != nil || again.ID != alice.ID {
		t.Fatalf("second ensure = %+v err=%v", again, err)
	}
	bob, err := users.Ensure(ctx, identity.ExternalID{Issuer: "https://collab.example", Subject: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	board, err := NewForums(ownerPool).Provision(ctx, "collaboration board", []string{alice.ID})
	if err != nil {
		t.Fatal(err)
	}
	store := NewCollaboration(runtimePool)
	effective, err := store.EffectivePermissions(ctx, alice.ID, board.ID)
	if err != nil {
		t.Fatal(err)
	}
	if effective.GroupID != identity.MemberGroupID || !containsGrant(effective.Grants, identity.GrantPost) || containsGrant(effective.Grants, identity.GrantDelete) {
		t.Fatalf("default member grants = %+v", effective)
	}
	if _, err := store.AddMember(ctx, alice.ID, board.ID, forum.MemberChange{UserID: bob.ID}); !errors.Is(err, forum.ErrForbidden) {
		t.Fatalf("member add without manage grant: %v", err)
	}
	observerID := "00000000-0000-4000-8000-000000000009"
	_, err = ownerPool.Exec(ctx, `INSERT INTO odysseia.identity_groups
		(id, name, priority, can_read, can_post, can_delete, can_manage_members, can_manage_groups)
		VALUES ($1, 'observer', 400, true, false, false, false, false)`, observerID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ownerPool.Exec(ctx, `INSERT INTO odysseia.user_groups (user_id, group_id) VALUES ($1, $2)`, alice.ID, observerID)
	if err != nil {
		t.Fatal(err)
	}
	effective, err = store.EffectivePermissions(ctx, alice.ID, board.ID)
	if err != nil {
		t.Fatal(err)
	}
	if effective.GroupID != observerID || containsGrant(effective.Grants, identity.GrantPost) || containsGrant(effective.Grants, identity.GrantManageMembers) {
		t.Fatalf("highest role did not replace lower grants: %+v", effective)
	}
	if _, err := store.AddMember(ctx, alice.ID, board.ID, forum.MemberChange{UserID: bob.ID}); !errors.Is(err, forum.ErrForbidden) {
		t.Fatalf("observer should not manage members: %v", err)
	}
	_, err = ownerPool.Exec(ctx, `DELETE FROM odysseia.user_groups WHERE user_id=$1 AND group_id=$2`, alice.ID, observerID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ownerPool.Exec(ctx, `INSERT INTO odysseia.user_groups (user_id, group_id) VALUES ($1, $2)`, alice.ID, identity.OwnerGroupID)
	if err != nil {
		t.Fatal(err)
	}
	effective, err = store.EffectivePermissions(ctx, alice.ID, board.ID)
	if err != nil || effective.GroupID != identity.OwnerGroupID || !containsGrant(effective.Grants, identity.GrantManageMembers) {
		t.Fatalf("owner grants = %+v err=%v", effective, err)
	}
	added, err := store.AddMember(ctx, alice.ID, board.ID, forum.MemberChange{UserID: bob.ID})
	if err != nil || added.RoleID != identity.MemberGroupID {
		t.Fatalf("add member = %+v err=%v", added, err)
	}
	if _, err := store.AddMember(ctx, alice.ID, board.ID, forum.MemberChange{UserID: bob.ID}); !errors.Is(err, forum.ErrExists) {
		t.Fatalf("duplicate member: %v", err)
	}
	updated, err := store.SetMemberRole(ctx, alice.ID, board.ID, bob.ID, identity.ModeratorGroupID)
	if err != nil || updated.RoleID != identity.ModeratorGroupID || updated.RoleName != "moderator" {
		t.Fatalf("set role = %+v err=%v", updated, err)
	}
	items, err := store.ListMembers(ctx, alice.ID, board.ID, "", 50)
	if err != nil || len(items) != 2 {
		t.Fatalf("members = %+v err=%v", items, err)
	}
	removed, err := store.RemoveMember(ctx, alice.ID, board.ID, bob.ID)
	if err != nil || !removed {
		t.Fatalf("remove = %v err=%v", removed, err)
	}
	var audits, disconnects int
	if err := ownerPool.QueryRow(ctx, `SELECT count(*) FROM odysseia.forum_member_removals WHERE forum_id=$1 AND user_id=$2 AND removed`, board.ID, bob.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit rows = %d err=%v", audits, err)
	}
	if err := ownerPool.QueryRow(ctx, `SELECT count(*) FROM odysseia.realtime_outbox WHERE method='disconnect' AND payload->>'user'=$1`, bob.ID).Scan(&disconnects); err != nil || disconnects != 1 {
		t.Fatalf("disconnects = %d err=%v", disconnects, err)
	}
	if _, err := store.ListMembers(ctx, bob.ID, board.ID, "", 50); !errors.Is(err, forum.ErrNotFound) {
		t.Fatalf("removed member still listed: %v", err)
	}
	if err := store.AssignGroup(ctx, alice.ID, identity.ModeratorGroupID, bob.ID); err != nil {
		t.Fatal(err)
	}
	removedGroup, err := store.RemoveGroup(ctx, alice.ID, identity.ModeratorGroupID, bob.ID)
	if err != nil || !removedGroup {
		t.Fatalf("remove group = %v err=%v", removedGroup, err)
	}
	groups, err := store.ListGroups(ctx)
	if err != nil || len(groups) < 3 || groups[0].Priority > groups[len(groups)-1].Priority {
		t.Fatalf("groups = %+v err=%v", groups, err)
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
