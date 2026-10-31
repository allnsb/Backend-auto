// Package telemetry records bounded service measurements, never resource IDs or content.
package telemetry

import (
	"context"
	"sync/atomic"
	"time"
)

type Stage uint8

const (
	TokenVerification Stage = iota
	IdentityLookup
	AccessCheck
	ScopeWait
	GlobalWait
	PoolAcquire
	TransactionBegin
	StreamLock
	IdempotencyAndReferences
	MessageWrite
	RelationAndOutbox
	TransactionCommit
	// AtomicMessageBatch covers message, optional forum relation and outbox;
	// its duration cannot be compared to the former message-only write stage.
	AtomicMessageBatch
	stageCount
)

var stageNames = [...]string{"token_verification", "identity_lookup", "access_check", "scope_wait", "global_wait", "pool_acquire", "transaction_begin", "stream_lock_observed", "idempotency_and_references", "message_write", "relation_and_outbox", "transaction_commit", "atomic_message_batch"}

type Rejection uint8

const (
	GlobalQueueFull Rejection = iota
	ScopeQueueFull
	ScopeWaitTimeout
	GlobalWaitTimeout
	RequestCanceled
	rejectionCount
)

var rejectionNames = [...]string{"global_queue_full", "scope_queue_full", "scope_wait_timeout", "global_wait_timeout", "request_canceled"}
var bounds = [...]time.Duration{100 * time.Microsecond, 500 * time.Microsecond, time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond, 25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second}

type histogram struct {
	count, nanos atomic.Uint64
	buckets      [len(bounds) + 1]atomic.Uint64
}

type Measurements struct {
	stages     [stageCount]histogram
	rejections [rejectionCount]atomic.Uint64
}

type Distribution struct {
	Count   uint64   `json:"count"`
	TotalNS uint64   `json:"total_ns"`
	Buckets []uint64 `json:"buckets"`
}

// Buckets are disjoint, ordered by BoundsNS, with one final unbounded bucket.
// Snapshots taken during writes are approximate; quiescent snapshots are exact.
type Snapshot struct {
	BoundsNS   []int64                 `json:"bounds_ns"`
	Stages     map[string]Distribution `json:"stages"`
	Rejections map[string]uint64       `json:"rejections"`
}

type contextKey struct{}

func WithMeasurements(ctx context.Context, measurements *Measurements) context.Context {
	return context.WithValue(ctx, contextKey{}, measurements)
}

// Start is a no-op outside an observed API request. The finite stage set and
// fixed buckets bound memory independently of traffic and forum cardinality.
func Start(ctx context.Context, stage Stage) func() {
	m, _ := ctx.Value(contextKey{}).(*Measurements)
	if m == nil {
		return func() {}
	}
	started := time.Now()
	return func() {
		elapsed := time.Since(started)
		h := &m.stages[stage]
		h.count.Add(1)
		h.nanos.Add(uint64(elapsed))
		bucket := len(bounds)
		for index, upper := range bounds {
			if elapsed <= upper {
				bucket = index
				break
			}
		}
		h.buckets[bucket].Add(1)
	}
}

func Reject(ctx context.Context, reason Rejection) {
	m, _ := ctx.Value(contextKey{}).(*Measurements)
	if m != nil {
		m.rejections[reason].Add(1)
	}
}

func (m *Measurements) Snapshot() Snapshot {
	result := Snapshot{BoundsNS: make([]int64, len(bounds)), Stages: make(map[string]Distribution, stageCount), Rejections: make(map[string]uint64, rejectionCount)}
	for index, bound := range bounds {
		result.BoundsNS[index] = int64(bound)
	}
	for index, name := range stageNames {
		h := &m.stages[index]
		d := Distribution{Count: h.count.Load(), TotalNS: h.nanos.Load(), Buckets: make([]uint64, len(h.buckets))}
		for bucket := range h.buckets {
			d.Buckets[bucket] = h.buckets[bucket].Load()
		}
		result.Stages[name] = d
	}
	for index, name := range rejectionNames {
		result.Rejections[name] = m.rejections[index].Load()
	}
	return result
}
