package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"odysseia.local/backend/internal/identity"
)

type Users struct {
	pool *pgxpool.Pool
}

func NewUsers(pool *pgxpool.Pool) *Users {
	return &Users{pool: pool}
}

func (s *Users) Find(ctx context.Context, external identity.ExternalID) (identity.User, error) {
	var user identity.User
	err := s.pool.QueryRow(ctx, `SELECT id::text, created_at FROM odysseia.users
		WHERE issuer = $1 AND subject = $2`, external.Issuer, external.Subject).Scan(&user.ID, &user.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return identity.User{}, identity.ErrNotFound
	}
	if err != nil {
		return identity.User{}, fmt.Errorf("find local user: %w", err)
	}
	return user, nil
}

// Provision is an explicit operator command. A repeated external identity reuses its local ID.
func (s *Users) Provision(ctx context.Context, external identity.ExternalID) (identity.User, error) {
	var user identity.User
	err := s.pool.QueryRow(ctx, `INSERT INTO odysseia.users (issuer, subject) VALUES ($1, $2)
		ON CONFLICT (issuer, subject) DO NOTHING RETURNING id::text, created_at`,
		external.Issuer, external.Subject).Scan(&user.ID, &user.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// A separate statement observes a concurrent winner after ON CONFLICT waits for it.
		return s.Find(ctx, external)
	}
	if err != nil {
		return identity.User{}, fmt.Errorf("provision local user: %w", err)
	}
	return user, nil
}
