//go:build integration

package postgres

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"testing"
)

func newCachedForumFixture(t *testing.T, pool *pgxpool.Pool) *Forums {
	t.Helper()
	store, err := NewCachedForums(pool, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return store
}
