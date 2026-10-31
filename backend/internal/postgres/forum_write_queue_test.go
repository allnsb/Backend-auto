package postgres

import (
	"context"
	"errors"
	"testing"

	"odysseia.local/backend/internal/forum"
)

func TestWriteAdmissionPerCounterQueueBound(t *testing.T) {
	gate := newWriteAdmission(4, 32)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	key := writeScope{streamID: "hot"}
	var holders []func()
	releaseHolders := func() {
		for _, release := range holders {
			release()
		}
		holders = nil
	}
	t.Cleanup(releaseHolders)
	for range gate.perScope {
		release, err := gate.acquire(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		holders = append(holders, release)
	}
	waiters := forumWriteScopeQueueLimit - int(gate.perScope)
	results := make(chan error, waiters)
	for range waiters {
		go func() {
			release, err := gate.acquire(ctx, key)
			if release != nil {
				release()
			}
			results <- err
		}()
	}
	waitWriteReferences(t, gate, key, forumWriteScopeQueueLimit)
	if _, err := gate.acquire(ctx, key); !errors.Is(err, forum.ErrBusy) {
		t.Fatalf("full counter queue was not refused: %v", err)
	}
	if len(gate.pending) != forumWriteScopeQueueLimit {
		t.Fatal("counter refusal leaked global admission capacity")
	}
	release, err := gate.acquire(ctx, writeScope{streamID: "ordinary"})
	if err != nil {
		t.Fatalf("ordinary stream could not enter beside a full hot queue: %v", err)
	}
	release()
	cancel()
	for range waiters {
		if err := <-results; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	releaseHolders()
	if len(gate.pending) != 0 || len(gate.scopes) != 0 {
		t.Fatal("counter cancellation leaked queue state")
	}
}
