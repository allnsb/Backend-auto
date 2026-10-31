package postgres

import (
	"context"
	"sync"
	"time"

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

// Creating topics/channels advances a forum counter; messages advance a stream counter.
// Separate fields keep those namespaces distinct even if their UUIDs match.
type writeScope struct {
	forumID  string
	streamID string
}

type writeCounter struct {
	active     *semaphore.Weighted
	references int
}

// writeAdmission bounds one Forums instance before it acquires a transaction
// connection. Database row locks remain the authority for ordering across nodes.
type writeAdmission struct {
	limit    int64
	active   *semaphore.Weighted
	pending  chan struct{}
	mu       sync.Mutex
	scopes   map[writeScope]*writeCounter
	perScope int64
}

func newWriteAdmission(writers int64, pending int) *writeAdmission {
	return &writeAdmission{
		limit: writers, active: semaphore.NewWeighted(writers), pending: make(chan struct{}, pending),
		scopes: make(map[writeScope]*writeCounter), perScope: min(writers, forumWriteScopeLimit),
	}
}

func (a *writeAdmission) acquire(ctx context.Context, key writeScope) (func(), error) {
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
func (a *writeAdmission) retain(key writeScope) *writeCounter {
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
