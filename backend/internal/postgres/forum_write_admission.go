package postgres

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/semaphore"
	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/telemetry"
)

const forumWriteQueueLimit = 256 // Admitted commands, including active writers.
const forumWriteWaitLimit = 2 * time.Second

// One local writer per counter avoids spending a second transaction connection
// waiting on its row lock. Different counters still share the global budget;
// database locks remain authoritative when another API instance writes too.
const forumWriteScopeLimit int64 = 1
const forumWriteScopeQueueLimit = 12 // Active and waiting, within the global 256.

// Topics/channels use forum counters, messages stream counters, draft creation
// an author key and draft edits a work key. The namespaces remain distinct even
// when their UUID strings happen to match.
type writeScope struct {
	forumID      string
	streamID     string
	workID       string
	workAuthorID string
}

type writeCounter struct {
	active     *semaphore.Weighted
	references int
}

// WriteBudget is shared by message and work-draft stores in one serving process
// before they acquire write connections. It does not reserve connections for
// reads, cover identity lookups or provide a distributed quota across nodes.
type WriteBudget struct {
	limit    int64
	active   *semaphore.Weighted
	pending  chan struct{}
	mu       sync.Mutex
	scopes   map[writeScope]*writeCounter
	perScope int64
}

func newWriteAdmission(writers int64, pending int) *WriteBudget {
	return &WriteBudget{
		limit: writers, active: semaphore.NewWeighted(writers), pending: make(chan struct{}, pending),
		scopes: make(map[writeScope]*writeCounter), perScope: min(writers, forumWriteScopeLimit),
	}
}

// NewWriteBudget preserves the existing automatic headroom policy (pool 16 ->
// 10 writers). Pass one result to all command stores using that serving pool.
func NewWriteBudget(pool *pgxpool.Pool, maxWriteTransactions int64) (*WriteBudget, error) {
	if pool == nil || maxWriteTransactions < 0 || maxWriteTransactions > int64(pool.Config().MaxConns) {
		return nil, fmt.Errorf("write budget requires a pool and a limit from zero (automatic) to its maximum")
	}
	if maxWriteTransactions == 0 {
		maxWriteTransactions = defaultWriteLimit(int64(pool.Config().MaxConns))
	}
	return newWriteAdmission(maxWriteTransactions, forumWriteQueueLimit), nil
}

func defaultWriteLimit(poolLimit int64) int64 {
	return max(int64(1), poolLimit-(poolLimit+2)/3)
}

func (a *WriteBudget) acquire(ctx context.Context, key writeScope) (func(), error) {
	if err := ctx.Err(); err != nil {
		telemetry.Reject(ctx, telemetry.RequestCanceled)
		return nil, err
	}
	select {
	case a.pending <- struct{}{}:
	default:
		telemetry.Reject(ctx, telemetry.GlobalQueueFull)
		return nil, forum.ErrBusy
	}
	counter := a.retain(key)
	if counter == nil {
		<-a.pending
		telemetry.Reject(ctx, telemetry.ScopeQueueFull)
		return nil, forum.ErrBusy
	}
	cleanup := func() {
		a.mu.Lock()
		counter.references--
		if counter.references == 0 {
			delete(a.scopes, key)
		}
		a.mu.Unlock()
		<-a.pending
	}
	waitCtx, cancel := context.WithTimeout(ctx, forumWriteWaitLimit)
	defer cancel()
	// Wait for the counter first: blocked hot writers must not occupy global
	// transaction permits while unrelated streams are ready to write.
	finish := telemetry.Start(ctx, telemetry.ScopeWait)
	err := counter.active.Acquire(waitCtx, 1)
	finish()
	if err != nil {
		cleanup()
		return nil, writeWaitError(ctx, telemetry.ScopeWaitTimeout)
	}
	finish = telemetry.Start(ctx, telemetry.GlobalWait)
	err = a.active.Acquire(waitCtx, 1)
	finish()
	if err != nil {
		counter.active.Release(1)
		cleanup()
		return nil, writeWaitError(ctx, telemetry.GlobalWaitTimeout)
	}
	return func() {
		a.active.Release(1)
		counter.active.Release(1)
		cleanup()
	}, nil
}

// The admitted-command bound also bounds registry size. A counter is removed
// after its final active/waiting caller exits; no TTL worker is needed.
func (a *WriteBudget) retain(key writeScope) *writeCounter {
	a.mu.Lock()
	defer a.mu.Unlock()
	counter := a.scopes[key]
	// A busy stream cannot fill the entire global waiting room by itself.
	// This is a bounded per-stream queue, not a distributed fairness guarantee.
	if counter != nil && counter.references >= forumWriteScopeQueueLimit {
		return nil
	}
	if counter == nil {
		counter = &writeCounter{active: semaphore.NewWeighted(a.perScope)}
		a.scopes[key] = counter
	}
	counter.references++
	return counter
}

func writeWaitError(ctx context.Context, reason telemetry.Rejection) error {
	if err := ctx.Err(); err != nil {
		telemetry.Reject(ctx, telemetry.RequestCanceled)
		return err
	}
	telemetry.Reject(ctx, reason)
	return forum.ErrBusy
}
