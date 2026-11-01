package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/identity"
)

// Collaboration stores identity-group membership and per-forum roles.
// Message writes continue to authorize current forum membership on their own path.
type Collaboration struct {
	pool *pgxpool.Pool
}

func NewCollaboration(pool *pgxpool.Pool) *Collaboration {
	return &Collaboration{pool: pool}
}

func (s *Collaboration) ListGroups(ctx context.Context) ([]identity.Group, error) {
	rows, err := s.pool.Query(ctx, groupSelectSQL+` ORDER BY priority, id`)
	if err != nil {
		return nil, err
	}
	return collectGroups(rows)
}

func (s *Collaboration) AssignGroup(ctx context.Context, actorID, groupID, userID string) error {
	if err := validIDs(actorID, groupID, userID); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		managed, err := beginGroupChange(ctx, tx, actorID, userID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO odysseia.user_groups (user_id, group_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, userID, groupID)
		var pgError *pgconn.PgError
		if errors.As(err, &pgError) && pgError.Code == "23503" {
			return forum.ErrNotFound
		}
		if err != nil {
			return err
		}
		return requireManagedForums(ctx, tx, managed)
	})
}

func (s *Collaboration) RemoveGroup(ctx context.Context, actorID, groupID, userID string) (bool, error) {
	if err := validIDs(actorID, groupID, userID); err != nil {
		return false, err
	}
	var removed bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		managed, err := beginGroupChange(ctx, tx, actorID, userID)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM odysseia.user_groups WHERE user_id=$1 AND group_id=$2`, userID, groupID)
		if err != nil {
			return err
		}
		removed = tag.RowsAffected() == 1
		return requireManagedForums(ctx, tx, managed)
	})
	return removed && err == nil, err
}

// Directory changes can alter a member's winning role in many forums. The
// exclusive control lock serializes them with forum role/member changes,
// which take its shared form before their forum row lock. Message writes do
// not participate. Capture only previously managed forums, so an unrelated
// pre-existing unmanaged forum does not prevent a directory change.
func beginGroupChange(ctx context.Context, tx pgx.Tx, actorID, userID string) ([]string, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('odysseia:identity-management', 0))`); err != nil {
		return nil, err
	}
	if _, err := requirePlatformGrant(ctx, tx, actorID, identity.GrantManageGroups); err != nil {
		return nil, err
	}
	var managed []string
	err := tx.QueryRow(ctx, `SELECT ARRAY(
		SELECT m.forum_id::text FROM odysseia.forum_members m
		WHERE m.user_id = $1 AND COALESCE((
			SELECT g.can_manage_members FROM odysseia.identity_groups g
			WHERE g.id = m.role_id OR g.id IN (
				SELECT group_id FROM odysseia.user_groups WHERE user_id = m.user_id)
			ORDER BY g.priority DESC, g.id LIMIT 1
		), false))`, userID).Scan(&managed)
	return managed, err
}

func requireManagedForums(ctx context.Context, tx pgx.Tx, forumIDs []string) error {
	if len(forumIDs) == 0 {
		return nil
	}
	var lost bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM unnest($1::uuid[]) f(id) WHERE NOT EXISTS (
			SELECT 1 FROM odysseia.forum_members m WHERE m.forum_id = f.id
			AND COALESCE((
				SELECT g.can_manage_members FROM odysseia.identity_groups g
				WHERE g.id = m.role_id OR g.id IN (
					SELECT group_id FROM odysseia.user_groups WHERE user_id = m.user_id)
				ORDER BY g.priority DESC, g.id LIMIT 1
			), false)
		))`, forumIDs).Scan(&lost)
	if err != nil {
		return err
	}
	if lost {
		return forum.ErrLastManager
	}
	return nil
}

func (s *Collaboration) ListMembers(ctx context.Context, actorID, forumID, after string, limit int) ([]forum.Member, error) {
	if err := validIDs(actorID, forumID); err != nil {
		return nil, err
	}
	if after == "" {
		after = "00000000-0000-0000-0000-000000000000"
	} else if err := forum.ValidID(after); err != nil {
		return nil, err
	}
	if limit < 1 || limit > forum.MaxPageSize {
		return nil, forum.ErrInvalid
	}
	var items []forum.Member
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, _, err := requireForumMember(ctx, tx, forumID, actorID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT m.user_id::text, m.role_id::text, g.name, g.priority
			FROM odysseia.forum_members m
			JOIN odysseia.identity_groups g ON g.id = m.role_id
			WHERE m.forum_id=$1 AND m.user_id > $2::uuid
			ORDER BY m.user_id LIMIT $3`, forumID, after, limit+1)
		if err != nil {
			return err
		}
		items, err = collectMembers(rows)
		return err
	})
	return items, err
}

func (s *Collaboration) AddMember(ctx context.Context, actorID, forumID string, change forum.MemberChange) (forum.Member, error) {
	if err := validIDs(actorID, forumID); err != nil {
		return forum.Member{}, err
	}
	if err := change.Validate(); err != nil {
		return forum.Member{}, err
	}
	if change.RoleID == "" {
		change.RoleID = identity.MemberGroupID
	}
	var member forum.Member
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockForum(ctx, tx, forumID); err != nil {
			return err
		}
		actor, err := requireForumGrant(ctx, tx, forumID, actorID, identity.GrantManageMembers)
		if err != nil {
			return err
		}
		role, err := groupByID(ctx, tx, change.RoleID)
		if err != nil {
			return err
		}
		if !identity.Below(actor.Priority, role.Priority) {
			return forum.ErrForbidden
		}
		err = tx.QueryRow(ctx, `INSERT INTO odysseia.forum_members (forum_id, user_id, role_id)
			SELECT $1, $2, g.id FROM odysseia.identity_groups g WHERE g.id=$3
			ON CONFLICT DO NOTHING
			RETURNING user_id::text, role_id::text`, forumID, change.UserID, change.RoleID).Scan(&member.UserID, &member.RoleID)
		if errors.Is(err, pgx.ErrNoRows) {
			return memberInsertMiss(ctx, tx, forumID, change)
		}
		var pgError *pgconn.PgError
		if errors.As(err, &pgError) && pgError.Code == "23503" {
			return forum.ErrNotFound
		}
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT name, priority FROM odysseia.identity_groups WHERE id=$1`, member.RoleID).Scan(&member.RoleName, &member.Priority)
	})
	return member, err
}

func (s *Collaboration) SetMemberRole(ctx context.Context, actorID, forumID, userID, roleID string) (forum.Member, error) {
	if err := validIDs(actorID, forumID, userID, roleID); err != nil {
		return forum.Member{}, err
	}
	var member forum.Member
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockForum(ctx, tx, forumID); err != nil {
			return err
		}
		actor, err := requireForumGrant(ctx, tx, forumID, actorID, identity.GrantManageMembers)
		if err != nil {
			return err
		}
		target, _, err := requireForumMember(ctx, tx, forumID, userID)
		if err != nil {
			return err
		}
		next, err := groupByID(ctx, tx, roleID)
		if err != nil {
			return err
		}
		if !identity.Below(actor.Priority, next.Priority) {
			return forum.ErrForbidden
		}
		if err := requireRemainingManager(ctx, tx, forumID, userID, next.ID, false); err != nil {
			return err
		}
		if !identity.Below(actor.Priority, target.Priority) {
			return forum.ErrForbidden
		}
		err = tx.QueryRow(ctx, `UPDATE odysseia.forum_members m
			SET role_id = g.id
			FROM odysseia.identity_groups g
			WHERE m.forum_id=$1 AND m.user_id=$2 AND g.id=$3
			RETURNING m.user_id::text, m.role_id::text, g.name, g.priority`, forumID, userID, roleID).Scan(&member.UserID, &member.RoleID, &member.RoleName, &member.Priority)
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var memberExists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM odysseia.forum_members WHERE forum_id=$1 AND user_id=$2)`, forumID, userID).Scan(&memberExists); err != nil {
			return err
		}
		if !memberExists {
			return forum.ErrNotFound
		}
		return forum.ErrInvalid
	})
	return member, err
}

func (s *Collaboration) RemoveMember(ctx context.Context, actorID, forumID, userID string) (bool, error) {
	if err := validIDs(actorID, forumID, userID); err != nil {
		return false, err
	}
	var removed bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockForum(ctx, tx, forumID); err != nil {
			return err
		}
		actor, err := requireForumGrant(ctx, tx, forumID, actorID, identity.GrantManageMembers)
		if err != nil {
			return err
		}
		target, _, err := requireForumMember(ctx, tx, forumID, userID)
		if err != nil {
			return err
		}
		if err := requireRemainingManager(ctx, tx, forumID, userID, identity.MemberGroupID, true); err != nil {
			return err
		}
		if !identity.Below(actor.Priority, target.Priority) {
			return forum.ErrForbidden
		}
		tag, err := tx.Exec(ctx, `DELETE FROM odysseia.forum_members WHERE forum_id=$1 AND user_id=$2`, forumID, userID)
		if err != nil {
			return err
		}
		removed = tag.RowsAffected() == 1
		if !removed {
			return forum.ErrNotFound
		}
		var created bool
		err = tx.QueryRow(ctx, `INSERT INTO odysseia.forum_member_removals
			(operation_id, forum_id, user_id, operator_label, reason, removed)
			VALUES (gen_random_uuid(), $1, $2, $3, 'forum member removed', true)
			RETURNING true`, forumID, userID, actorID).Scan(&created)
		if err != nil {
			return err
		}
		return enqueueForumDisconnect(ctx, tx, userID)
	})
	return removed, err
}

func (s *Collaboration) EffectivePermissions(ctx context.Context, actorID, forumID string) (identity.Effective, error) {
	if err := validIDs(actorID, forumID); err != nil {
		return identity.Effective{}, err
	}
	var effective identity.Effective
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		winner, _, err := requireForumMember(ctx, tx, forumID, actorID)
		if err != nil {
			return err
		}
		effective = identity.Effective{
			ForumID: forumID, GroupID: winner.ID, GroupName: winner.Name,
			Priority: winner.Priority, Grants: winner.Grants,
		}
		return nil
	})
	return effective, err
}

const groupSelectSQL = `SELECT id::text, name, priority, can_read, can_post, can_delete, can_manage_members, can_manage_groups FROM odysseia.identity_groups`

func requireForumMember(ctx context.Context, tx rowQueryer, forumID, userID string) (identity.Group, bool, error) {
	winner, member, err := forumCandidates(ctx, tx, forumID, userID)
	if err != nil {
		return identity.Group{}, false, err
	}
	if !member {
		return identity.Group{}, false, forum.ErrNotFound
	}
	return winner, true, nil
}

func requireForumGrant(ctx context.Context, tx pgx.Tx, forumID, userID, grant string) (identity.Group, error) {
	winner, _, err := requireForumMember(ctx, tx, forumID, userID)
	if err != nil {
		return identity.Group{}, err
	}
	if !winner.Allows(grant) {
		return identity.Group{}, forum.ErrForbidden
	}
	return winner, nil
}

func requirePlatformGrant(ctx context.Context, tx pgx.Tx, userID, grant string) (identity.Group, error) {
	rows, err := tx.Query(ctx, `SELECT g.id::text, g.name, g.priority, g.can_read, g.can_post, g.can_delete,
		g.can_manage_members, g.can_manage_groups
		FROM odysseia.identity_groups g
		JOIN odysseia.user_groups ug ON ug.group_id = g.id
		WHERE ug.user_id = $1`, userID)
	if err != nil {
		return identity.Group{}, err
	}
	groups, err := collectGroups(rows)
	if err != nil {
		return identity.Group{}, err
	}
	winner, ok := identity.Resolve(groups)
	if !ok || !winner.Allows(grant) {
		return identity.Group{}, forum.ErrForbidden
	}
	return winner, nil
}

func forumCandidates(ctx context.Context, tx rowQueryer, forumID, userID string) (identity.Group, bool, error) {
	rows, err := tx.Query(ctx, `SELECT
		EXISTS (SELECT 1 FROM odysseia.forum_members WHERE forum_id=$1 AND user_id=$2),
		g.id::text, g.name, g.priority, g.can_read, g.can_post, g.can_delete, g.can_manage_members, g.can_manage_groups
		FROM odysseia.identity_groups g
		WHERE g.id IN (
			SELECT role_id FROM odysseia.forum_members WHERE forum_id=$1 AND user_id=$2
			UNION
			SELECT group_id FROM odysseia.user_groups WHERE user_id=$2
		)`, forumID, userID)
	if err != nil {
		return identity.Group{}, false, err
	}
	defer rows.Close()
	var member bool
	groups := make([]identity.Group, 0, 4)
	for rows.Next() {
		var group identity.Group
		var read, post, delete, manageMembers, manageGroups bool
		if err := rows.Scan(&member, &group.ID, &group.Name, &group.Priority, &read, &post, &delete, &manageMembers, &manageGroups); err != nil {
			return identity.Group{}, false, err
		}
		group.Grants = identity.GrantsFromFlags(read, post, delete, manageMembers, manageGroups)
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return identity.Group{}, false, err
	}
	if len(groups) == 0 {
		return identity.Group{}, false, nil
	}
	winner, _ := identity.Resolve(groups)
	return winner, member, nil
}

func lockForum(ctx context.Context, tx pgx.Tx, forumID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended('odysseia:identity-management', 0))`); err != nil {
		return err
	}
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM odysseia.forums WHERE id=$1 FOR UPDATE`, forumID).Scan(&id)
	return forumError(err)
}

func groupByID(ctx context.Context, tx pgx.Tx, groupID string) (identity.Group, error) {
	group, err := scanGroup(tx.QueryRow(ctx, groupSelectSQL+` WHERE id=$1`, groupID))
	if errors.Is(err, pgx.ErrNoRows) {
		return identity.Group{}, forum.ErrInvalid
	}
	return group, err
}

func scanGroup(row pgx.Row) (identity.Group, error) {
	var group identity.Group
	var read, post, delete, manageMembers, manageGroups bool
	err := row.Scan(&group.ID, &group.Name, &group.Priority, &read, &post, &delete, &manageMembers, &manageGroups)
	if err != nil {
		return identity.Group{}, err
	}
	group.Grants = identity.GrantsFromFlags(read, post, delete, manageMembers, manageGroups)
	return group, nil
}

// requireRemainingManager keeps at least one member whose winning role grants
// manage_members. Hierarchy uses that same winning priority for the target.
func requireRemainingManager(ctx context.Context, tx pgx.Tx, forumID, userID, nextRoleID string, remove bool) error {
	var managers int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM odysseia.forum_members m
		WHERE m.forum_id=$1
		  AND (NOT $4::bool OR m.user_id <> $2::uuid)
		  AND COALESCE((
		    SELECT g.can_manage_members
		    FROM odysseia.identity_groups g
		    WHERE g.id = CASE WHEN m.user_id=$2::uuid AND NOT $4::bool THEN $3::uuid ELSE m.role_id END
		       OR g.id IN (SELECT ug.group_id FROM odysseia.user_groups ug WHERE ug.user_id=m.user_id)
		    ORDER BY g.priority DESC, g.id
		    LIMIT 1
		  ), false)`, forumID, userID, nextRoleID, remove).Scan(&managers)
	if err != nil {
		return err
	}
	if managers == 0 {
		return forum.ErrLastManager
	}
	return nil
}

func memberInsertMiss(ctx context.Context, tx pgx.Tx, forumID string, change forum.MemberChange) error {
	var memberExists, roleExists bool
	err := tx.QueryRow(ctx, `SELECT
		EXISTS(SELECT 1 FROM odysseia.forum_members WHERE forum_id=$1 AND user_id=$2),
		EXISTS(SELECT 1 FROM odysseia.identity_groups WHERE id=$3)`, forumID, change.UserID, change.RoleID).Scan(&memberExists, &roleExists)
	if err != nil {
		return err
	}
	if !roleExists {
		return forum.ErrInvalid
	}
	if memberExists {
		return forum.ErrExists
	}
	return forum.ErrNotFound
}

func validIDs(values ...string) error {
	for _, value := range values {
		if err := forum.ValidID(value); err != nil {
			return err
		}
	}
	return nil
}

func collectGroups(rows pgx.Rows) ([]identity.Group, error) {
	defer rows.Close()
	groups := make([]identity.Group, 0, 4)
	for rows.Next() {
		var group identity.Group
		var read, post, delete, manageMembers, manageGroups bool
		if err := rows.Scan(&group.ID, &group.Name, &group.Priority, &read, &post, &delete, &manageMembers, &manageGroups); err != nil {
			return nil, err
		}
		group.Grants = identity.GrantsFromFlags(read, post, delete, manageMembers, manageGroups)
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func collectMembers(rows pgx.Rows) ([]forum.Member, error) {
	defer rows.Close()
	items := make([]forum.Member, 0, 8)
	for rows.Next() {
		var member forum.Member
		if err := rows.Scan(&member.UserID, &member.RoleID, &member.RoleName, &member.Priority); err != nil {
			return nil, err
		}
		items = append(items, member)
	}
	return items, rows.Err()
}
