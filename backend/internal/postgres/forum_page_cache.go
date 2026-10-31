package postgres

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/ristretto/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"
	"odysseia.local/backend/internal/forum"
)

const forumPageCacheCost = 64 << 20
const forumPageCacheTTL = 5 * time.Minute

type cachedMessagePage struct {
	key     string
	entries []forum.Entry
}

// Only append-only content is cached. Current membership and stream position
// are read from PostgreSQL for every caller before a cache key is constructed.
type messagePageCache struct {
	data                *ristretto.Cache[string, cachedMessagePage]
	flights             singleflight.Group
	hits, misses, loads atomic.Uint64
}

// NewCachedForums opts serving processes into the bounded page cache. Operator
// commands can keep using NewForums without owning background cache workers.
// A zero write limit selects the automatic per-instance budget. An explicit
// limit cannot exceed this pool; it does not allocate or reserve connections.
func NewCachedForums(pool *pgxpool.Pool, maxWriteTransactions int64) (*Forums, error) {
	if maxWriteTransactions < 0 || maxWriteTransactions > int64(pool.Config().MaxConns) {
		return nil, fmt.Errorf("write transaction budget must be between zero (automatic) and pool maximum %d", pool.Config().MaxConns)
	}
	store := NewForums(pool)
	if maxWriteTransactions > 0 {
		store.writes = newWriteAdmission(maxWriteTransactions, forumWriteQueueLimit)
	}
	cache, err := ristretto.NewCache(&ristretto.Config[string, cachedMessagePage]{
		NumCounters: 10000, MaxCost: forumPageCacheCost, BufferItems: 64,
	})
	if err != nil {
		return nil, fmt.Errorf("create forum page cache: %w", err)
	}
	store.pages = &messagePageCache{data: cache}
	return store, nil
}

// Close follows HTTP shutdown, before the database pool is closed.
func (s *Forums) Close() {
	if s.pages != nil {
		s.pages.data.Close()
	}
}

func (p *messagePageCache) read(ctx context.Context, key string, load func(context.Context) ([]forum.Entry, error)) ([]forum.Entry, error) {
	if page, found := p.data.Get(key); found && page.key == key {
		p.hits.Add(1)
		return slices.Clone(page.entries), nil
	}
	p.misses.Add(1)
	// Waiters cancel independently. A canceled leader may fail this shared load;
	// no result, denial or permission decision is cached on failure.
	result := p.flights.DoChan(key, func() (any, error) {
		p.loads.Add(1)
		entries, err := load(ctx)
		if err != nil {
			return nil, err
		}
		cost := int64(len(key) + 64)
		for _, entry := range entries {
			cost += int64(160 + len(entry.ID) + len(entry.StreamID) + len(entry.AuthorID) + len(entry.RootID) + len(entry.QuoteID) + len(entry.Body))
		}
		// Admission may discard a Set under contention or eviction pressure.
		// The database result is still returned; this is not a persistence queue.
		p.data.SetWithTTL(key, cachedMessagePage{key, slices.Clone(entries)}, cost, forumPageCacheTTL)
		return entries, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case loaded := <-result:
		if loaded.Err != nil {
			return nil, loaded.Err
		}
		return slices.Clone(loaded.Val.([]forum.Entry)), nil
	}
}
