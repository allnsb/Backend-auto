//go:build integration && performance

package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"odysseia.local/backend/internal/forum"
)

type performanceWriteSample struct {
	Name           string  `json:"name"`
	Queries        int     `json:"queries"`
	Batches        int     `json:"batches"`
	Statements     int     `json:"statements"`
	ElapsedMS      float64 `json:"elapsed_ms"`
	LockToCommitMS float64 `json:"lock_to_commit_ms"`
}

// These sequential probes use a separate one-connection pool after the API load
// measurement. Driver call counts are not packet counts; lock-to-commit includes
// client work and commit acknowledgement, not server-side lock instrumentation.
func performanceWriteSamples(t *testing.T, ctx context.Context, connectionString, actor, topicID, quoteID string) []performanceWriteSample {
	t.Helper()
	trace := &performanceWriteTracer{}
	cfg, err := parseConfig(connectionString)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store := NewForums(pool)
	var samples []performanceWriteSample
	for _, test := range []struct {
		name, quote string
	}{{"reply", ""}, {"quoted_reply", quoteID}} {
		draft := forum.ReplyDraft{Body: strings.Repeat("x", 1024), QuoteID: test.quote}
		// Warm both driver statement caches before collecting this scenario.
		if _, err := store.Reply(ctx, actor, topicID, uuid.NewString(), draft); err != nil {
			t.Fatal(err)
		}
		for range 16 {
			*trace = performanceWriteTracer{}
			start := time.Now()
			if _, err := store.Reply(ctx, actor, topicID, uuid.NewString(), draft); err != nil {
				t.Fatal(err)
			}
			if trace.locked.IsZero() || trace.committed.IsZero() {
				t.Fatal("write probe did not observe lock acquisition and commit")
			}
			samples = append(samples, performanceWriteSample{
				Name: test.name, Queries: trace.queries, Batches: trace.batches, Statements: trace.statements,
				ElapsedMS:      float64(time.Since(start)) / float64(time.Millisecond),
				LockToCommitMS: float64(trace.committed.Sub(trace.locked)) / float64(time.Millisecond),
			})
		}
	}
	return samples
}

// This tracer belongs exclusively to the sequential probe, never the measured
// API pool. It retains no SQL arguments, message bodies, identities or tokens.
type performanceWriteTracer struct {
	queries, batches, statements int
	sql                          string
	locked, committed            time.Time
}

func (p *performanceWriteTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	p.queries++
	p.statements++
	p.sql = data.SQL
	return ctx
}

func (p *performanceWriteTracer) TraceQueryEnd(_ context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if data.Err != nil {
		return
	}
	if strings.Contains(p.sql, "FOR UPDATE OF s") {
		p.locked = time.Now()
	}
	if p.sql == "commit" {
		p.committed = time.Now()
	}
}

func (p *performanceWriteTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	p.batches++
	return ctx
}

func (p *performanceWriteTracer) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	p.statements++
	if data.Err == nil && strings.Contains(data.SQL, "FOR UPDATE OF s") {
		p.locked = time.Now()
	}
}

func (p *performanceWriteTracer) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}
