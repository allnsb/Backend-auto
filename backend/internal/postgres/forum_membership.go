package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"odysseia.local/backend/internal/forum"
)

// RemoveMember commits membership, its audit record and a queued disconnect together.
// It requires maintenance DB privileges and returns after commit, before asynchronous delivery.
func (s *Forums) RemoveMember(ctx context.Context, command forum.MemberRemoval) (forum.RemovedMember, error) {
	if err := command.Validate(); err != nil {
		return forum.RemovedMember{}, err
	}
	var result forum.RemovedMember
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockForum(ctx, tx, command.ForumID); err != nil {
			return err
		}
		previous, err := previousMemberRemoval(ctx, tx, command.OperationID)
		if err == nil && previous.MemberRemoval != command {
			return forum.ErrConflict
		}
		if err == nil {
			result = previous
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		deleted, err := tx.Exec(ctx, `DELETE FROM odysseia.forum_members WHERE forum_id=$1 AND user_id=$2`, command.ForumID, command.UserID)
		if err != nil {
			return err
		}
		result = forum.RemovedMember{MemberRemoval: command, Removed: deleted.RowsAffected() == 1}
		err = tx.QueryRow(ctx, `INSERT INTO odysseia.forum_member_removals
			(operation_id, forum_id, user_id, operator_label, reason, removed)
			VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT(operation_id) DO NOTHING RETURNING created_at`, command.OperationID, command.ForumID, command.UserID, command.Operator, command.Reason, result.Removed).Scan(&result.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return forum.ErrConflict
		}
		if err != nil {
			return forumError(err)
		}
		return enqueueForumDisconnect(ctx, tx, command.UserID)
	})
	return result, err
}

func previousMemberRemoval(ctx context.Context, tx pgx.Tx, operationID string) (forum.RemovedMember, error) {
	var value forum.RemovedMember
	err := tx.QueryRow(ctx, `SELECT operation_id::text,forum_id::text,user_id::text,operator_label,reason,removed,created_at
		FROM odysseia.forum_member_removals WHERE operation_id=$1`, operationID).Scan(&value.OperationID, &value.ForumID, &value.UserID, &value.Operator, &value.Reason, &value.Removed, &value.CreatedAt)
	return value, err
}
