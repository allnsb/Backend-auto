package identity

import "testing"

func TestHighestPriorityRoleSuppliesTheOnlyGrants(t *testing.T) {
	member := Group{ID: MemberGroupID, Name: "member", Priority: 100, Grants: GrantsFromFlags(true, true, false, false, false)}
	moderator := Group{ID: ModeratorGroupID, Name: "moderator", Priority: 200, Grants: GrantsFromFlags(true, true, true, true, false)}
	observer := Group{ID: "00000000-0000-4000-8000-000000000009", Name: "observer", Priority: 400, Grants: GrantsFromFlags(true, false, false, false, false)}

	winner, ok := Resolve([]Group{member, moderator, observer})
	if !ok {
		t.Fatal("expected a winning role")
	}
	if winner.ID != observer.ID || winner.Priority != 400 {
		t.Fatalf("winner = %+v", winner)
	}
	if winner.Allows(GrantPost) || winner.Allows(GrantDelete) || winner.Allows(GrantManageMembers) {
		t.Fatalf("lower roles leaked grants into %+v", winner.Grants)
	}
	if !winner.Allows(GrantRead) {
		t.Fatal("winning role lost its own read grant")
	}
	observer.Grants[0] = "mutated"
	if winner.Grants[0] != GrantRead {
		t.Fatal("resolved grants alias the caller's slice")
	}

	tiedLow := Group{ID: "00000000-0000-4000-8000-000000000008", Name: "tied-low", Priority: 50, Grants: []string{GrantDelete}}
	tiedHigh := Group{ID: "00000000-0000-4000-8000-000000000007", Name: "tied-high", Priority: 50, Grants: []string{GrantPost}}
	tied, ok := Resolve([]Group{tiedLow, tiedHigh})
	if !ok || tied.ID != tiedHigh.ID || !tied.Allows(GrantPost) || tied.Allows(GrantDelete) {
		t.Fatalf("equal priority did not keep one role's grants: %+v", tied)
	}
	if _, ok := Resolve(nil); ok {
		t.Fatal("empty role set resolved")
	}
}
