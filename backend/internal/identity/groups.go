package identity

// Grant names are the UI gates for this slice. Topic and channel writes still
// follow forum membership; message deletion has no route yet.
const (
	GrantRead          = "read"
	GrantPost          = "post"
	GrantDelete        = "delete"
	GrantManageMembers = "manage_members"
	GrantManageGroups  = "manage_groups"
	MemberGroupID      = "00000000-0000-4000-8000-000000000001"
	ModeratorGroupID   = "00000000-0000-4000-8000-000000000002"
	OwnerGroupID       = "00000000-0000-4000-8000-000000000003"
	// ModeratorPriority matches the seeded moderator row. Owner is 300.
	// Forum emoji management accepts this priority and anything higher.
	ModeratorPriority = 200
)

// Group is one configured identity role. Priority is comparable across groups;
// a higher number outranks a lower one.
type Group struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Priority int      `json:"priority"`
	Grants   []string `json:"grants"`
}

// Effective is the single winning role for a forum member.
type Effective struct {
	ForumID   string   `json:"forum_id"`
	GroupID   string   `json:"group_id"`
	GroupName string   `json:"group_name"`
	Priority  int      `json:"priority"`
	Grants    []string `json:"grants"`
}

func (g Group) Allows(grant string) bool {
	for _, item := range g.Grants {
		if item == grant {
			return true
		}
	}
	return false
}

// Below reports whether subjectPriority is strictly under actorPriority.
// Member changes compare effective priorities: catalog groups and the stored
// forum role, highest wins. A role being granted uses that role's own priority.
// An equal priority is out of reach.
func Below(actorPriority, subjectPriority int) bool {
	return subjectPriority < actorPriority
}

// Resolve keeps every grant from the single highest-priority role.
// A lower role does not add or remove grants. Equal priorities keep the
// lexicographically smaller group ID; the catalog also rejects duplicate priorities.
func Resolve(groups []Group) (Group, bool) {
	if len(groups) == 0 {
		return Group{}, false
	}
	winner := groups[0]
	for _, candidate := range groups[1:] {
		if candidate.Priority > winner.Priority || (candidate.Priority == winner.Priority && candidate.ID < winner.ID) {
			winner = candidate
		}
	}
	winner.Grants = append([]string(nil), winner.Grants...)
	return winner, true
}

// GrantsFromFlags emits the stable grant order used by the API.
func GrantsFromFlags(read, post, delete, manageMembers, manageGroups bool) []string {
	grants := make([]string, 0, 5)
	if read {
		grants = append(grants, GrantRead)
	}
	if post {
		grants = append(grants, GrantPost)
	}
	if delete {
		grants = append(grants, GrantDelete)
	}
	if manageMembers {
		grants = append(grants, GrantManageMembers)
	}
	if manageGroups {
		grants = append(grants, GrantManageGroups)
	}
	return grants
}
