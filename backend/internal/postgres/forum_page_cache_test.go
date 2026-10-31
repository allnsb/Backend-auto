package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dgraph-io/ristretto/v2"
	"odysseia.local/backend/internal/forum"
)

func newPageCacheTest(t *testing.T) *messagePageCache {
	t.Helper()
	cache, err := ristretto.NewCache(&ristretto.Config[string, cachedMessagePage]{NumCounters: 100, MaxCost: 1 << 20, BufferItems: 64})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.Close)
	return &messagePageCache{data: cache}
}

func TestMessagePageCacheCoalescesCanceledWaiters(t *testing.T) {
	cache := newPageCacheTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	entered, release := make(chan struct{}), make(chan struct{})
	type result struct {
		entries []forum.Entry
		err     error
	}
	done := make(chan result, 1)
	load := func(ctx context.Context) ([]forum.Entry, error) {
		close(entered)
		select {
		case <-release:
			return []forum.Entry{{Body: "content"}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	go func() {
		entries, err := cache.read(ctx, "page", load)
		done <- result{entries, err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("leader did not start")
	}
	// Each waiter registers while the leader is blocked, then observes its own
	// cancellation. Neither another content load nor cancellation of the leader
	// should result from these callers leaving.
	for range 4 {
		waitCtx, stop := context.WithCancel(ctx)
		stop()
		if _, err := cache.read(waitCtx, "page", load); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	if cache.loads.Load() != 1 {
		t.Fatal("concurrent misses were not coalesced")
	}
	close(release)
	var got result
	select {
	case got = <-done:
	case <-ctx.Done():
		t.Fatal("leader did not complete")
	}
	if got.err != nil || len(got.entries) != 1 {
		t.Fatalf("leader failed: %v", got.err)
	}
	got.entries[0].Body = "caller mutation"
	cache.data.Wait()
	entries, err := cache.read(ctx, "page", load)
	if err != nil || len(entries) != 1 || entries[0].Body != "content" || cache.loads.Load() != 1 || cache.hits.Load() != 1 {
		t.Fatal("waiter cancellation or caller mutation changed the stored content")
	}
}

func TestMessagePageCacheDoesNotCacheLoadErrors(t *testing.T) {
	cache := newPageCacheTest(t)
	failure := errors.New("content load failed")
	for range 2 {
		_, err := cache.read(context.Background(), "failed", func(context.Context) ([]forum.Entry, error) { return nil, failure })
		if !errors.Is(err, failure) {
			t.Fatal(err)
		}
	}
	if cache.loads.Load() != 2 || cache.hits.Load() != 0 {
		t.Fatal("a content load failure was reused")
	}
}
