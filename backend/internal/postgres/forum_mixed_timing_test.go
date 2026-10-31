//go:build integration && performance

package postgres

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

type mixedTimingKey struct{}
type mixedCommitKey struct{}

type mixedRequestTiming struct {
	started, begun, locked, ended time.Time
}

type mixedWriteTiming struct {
	Requests      int          `json:"requests"`
	Transactions  int          `json:"transactions"`
	NoTransaction int          `json:"no_transaction"`
	ClockSamples  int          `json:"clock_samples"`
	InvalidClocks int          `json:"invalid_clocks"`
	Handler       mixedLatency `json:"handler_ms"`
	BeforeBegin   mixedLatency `json:"before_begin_ms"`
	Transaction   mixedLatency `json:"transaction_ms"`
	BeginToLock   mixedLatency `json:"begin_to_lock_observed_ms"`
	LockToCommit  mixedLatency `json:"lock_to_commit_observed_ms"`
}

// SQL tracing is confined to synthetic tagged write requests in the test API.
// No arguments, statements, identities, bodies or request IDs are retained.
type mixedAPITimings struct {
	mu      sync.Mutex
	records map[string][]mixedRequestTiming
	handler map[string][]time.Duration
}

func (p *mixedAPITimings) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lane := r.Header.Get("X-Odysseia-Performance-Lane")
		if lane != "ordinary_write" && lane != "hot_write" {
			next.ServeHTTP(w, r)
			return
		}
		state := &mixedRequestTiming{started: time.Now()}
		defer func() {
			elapsed := time.Since(state.started)
			p.mu.Lock()
			p.records[lane] = append(p.records[lane], *state)
			p.handler[lane] = append(p.handler[lane], elapsed)
			p.mu.Unlock()
		}()
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), mixedTimingKey{}, state)))
	})
}

func (p *mixedAPITimings) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.records, p.handler = make(map[string][]mixedRequestTiming), make(map[string][]time.Duration)
}

func (p *mixedAPITimings) take() map[string]mixedWriteTiming {
	p.mu.Lock()
	records, handler := p.records, p.handler
	p.records, p.handler = make(map[string][]mixedRequestTiming), make(map[string][]time.Duration)
	p.mu.Unlock()
	result := make(map[string]mixedWriteTiming, len(records))
	for lane, requests := range records {
		var before, tx, lockWait, locked []time.Duration
		transactions, invalid := 0, 0
		for i, request := range requests {
			if request.begun.IsZero() || request.ended.IsZero() {
				continue
			}
			transactions++
			wait := request.begun.Sub(request.started)
			duration := request.ended.Sub(request.begun)
			if wait < 0 || duration < 0 || wait+duration > handler[lane][i] {
				invalid++
				continue
			}
			before = append(before, wait)
			tx = append(tx, duration)
			if !request.locked.IsZero() {
				lockWait = append(lockWait, request.locked.Sub(request.begun))
				locked = append(locked, request.ended.Sub(request.locked))
			}
		}
		result[lane] = mixedWriteTiming{
			Requests: len(requests), Transactions: transactions, NoTransaction: len(requests) - transactions,
			ClockSamples: len(tx), InvalidClocks: invalid,
			Handler: mixedPercentiles(handler[lane]), BeforeBegin: mixedPercentiles(before),
			Transaction: mixedPercentiles(tx),
			BeginToLock: mixedPercentiles(lockWait), LockToCommit: mixedPercentiles(locked),
		}
	}
	return result
}

func (p *mixedAPITimings) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (p *mixedAPITimings) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if data.Err != nil || !strings.Contains(data.SQL, "FOR UPDATE OF s") {
		return
	}
	state, ok := ctx.Value(mixedTimingKey{}).(*mixedRequestTiming)
	if ok {
		state.locked = time.Now()
	}
}

func (p *mixedAPITimings) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

func (p *mixedAPITimings) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	state, ok := ctx.Value(mixedTimingKey{}).(*mixedRequestTiming)
	if !ok {
		return ctx
	}
	switch {
	case data.SQL == "begin":
		state.begun = time.Now()
	case data.SQL == "commit" || data.SQL == "rollback":
		return context.WithValue(ctx, mixedCommitKey{}, true)
	}
	return ctx
}

func (p *mixedAPITimings) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(mixedCommitKey{}) == nil {
		return
	}
	state, ok := ctx.Value(mixedTimingKey{}).(*mixedRequestTiming)
	if !ok {
		return
	}
	if state.ended.IsZero() {
		state.ended = time.Now()
	}
}
