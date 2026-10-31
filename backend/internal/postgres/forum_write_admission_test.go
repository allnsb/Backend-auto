package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"odysseia.local/backend/internal/forum"
)

func TestWriteAdmissionBoundsAndCancellation(t *testing.T) {
	gate := newWriteAdmission(1, 1)
	key := writeScope{streamID: "hot"}
	release, err := gate.acquire(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gate.acquire(context.Background(), key); !errors.Is(err, forum.ErrBusy) {
		t.Fatalf("full admission must reject immediately: %v", err)
	}
	release()

	gate = newWriteAdmission(1, 2)
	release, err = gate.acquire(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := gate.acquire(ctx, key); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("parent cancellation was lost: %v", err)
	}
	if len(gate.pending) != 1 {
		t.Fatal("canceled waiter retained admission capacity")
	}
	release()
	if len(gate.pending) != 0 || len(gate.scopes) != 0 || !gate.active.TryAcquire(1) {
		t.Fatal("completed command retained admission or writer capacity")
	}
	gate.active.Release(1)
}

func TestWriteAdmissionQueueDeadline(t *testing.T) {
	gate := newWriteAdmission(1, 2)
	key := writeScope{streamID: "hot"}
	release, err := gate.acquire(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := gate.acquire(context.Background(), key); !errors.Is(err, forum.ErrBusy) {
		t.Fatalf("queue deadline must report temporary capacity exhaustion: %v", err)
	}
	if len(gate.pending) != 1 {
		t.Fatal("timed-out waiter retained admission capacity")
	}
}

func TestWriteAdmissionConcurrentBudget(t *testing.T) {
	gate := newWriteAdmission(2, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered := make(chan struct{}, 32)
	finish := make(chan struct{})
	results := make(chan error, 32)
	var active atomic.Int32
	var workers sync.WaitGroup
	for index := range 32 {
		workers.Go(func() {
			release, err := gate.acquire(ctx, writeScope{streamID: fmt.Sprintf("counter-%d", index)})
			if err != nil {
				results <- err
				return
			}
			defer release()
			if active.Add(1) > 2 {
				active.Add(-1)
				results <- errors.New("too many active writers")
				return
			}
			entered <- struct{}{}
			<-finish
			active.Add(-1)
			results <- nil
		})
	}
	for range 2 { // The global budget is two independent counters.
		select {
		case <-entered:
		case <-ctx.Done():
			close(finish)
			t.Fatal("writer budget did not admit available work")
		}
	}
	close(finish)
	workers.Wait()
	for range 32 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if len(gate.pending) != 0 || len(gate.scopes) != 0 || !gate.active.TryAcquire(2) {
		t.Fatal("concurrent execution exceeded or leaked the configured budget")
	}
	gate.active.Release(2)
}

func TestWriteAdmissionCounterWaitDoesNotHoldGlobalBudget(t *testing.T) {
	gate := newWriteAdmission(4, 32)
	hot := writeScope{streamID: "hot"}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var holders []func()
	releaseHolders := func() {
		for i, release := range holders {
			if release != nil {
				release()
				holders[i] = nil
			}
		}
	}
	t.Cleanup(releaseHolders)
	for range gate.perScope {
		release, err := gate.acquire(ctx, hot)
		if err != nil {
			t.Fatal(err)
		}
		holders = append(holders, release)
	}
	results := make(chan error, 8)
	for range 8 {
		go func() {
			release, err := gate.acquire(ctx, hot)
			if release != nil {
				release()
			}
			results <- err
		}()
	}
	waitWriteReferences(t, gate, hot, int(gate.perScope)+8)
	ordinaryCtx, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stop()
	// Fill every remaining global permit with independent ready counters.
	for index := range 4 - int(gate.perScope) {
		release, err := gate.acquire(ordinaryCtx, writeScope{streamID: fmt.Sprintf("ordinary-%d", index)})
		if err != nil {
			t.Fatalf("hot counter consumed unrelated global capacity: %v", err)
		}
		holders = append(holders, release)
	}
	cancel()
	for range 8 {
		if err := <-results; !errors.Is(err, context.Canceled) {
			t.Fatalf("counter waiter ignored cancellation: %v", err)
		}
	}
	releaseHolders()
	if len(gate.scopes) != 0 || len(gate.pending) != 0 || !gate.active.TryAcquire(4) {
		t.Fatal("counter cancellation leaked registry or global capacity")
	}
	gate.active.Release(4)
}

func TestWriteAdmissionGlobalCancellationAndNamespaces(t *testing.T) {
	gate := newWriteAdmission(1, 2)
	release, err := gate.acquire(context.Background(), writeScope{streamID: "first"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := gate.acquire(ctx, writeScope{streamID: "second"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("global waiter ignored cancellation: %v", err)
	}
	if len(gate.scopes) != 1 || len(gate.pending) != 1 {
		t.Fatal("global cancellation leaked a counter reference")
	}
	release()

	gate = newWriteAdmission(4, 4)
	var holders []func()
	for _, key := range []writeScope{{streamID: "same-uuid"}, {forumID: "same-uuid"}} {
		release, err := gate.acquire(context.Background(), key)
		if err != nil {
			t.Fatalf("forum and stream counter namespaces were coupled: %v", err)
		}
		holders = append(holders, release)
	}
	if len(gate.scopes) != 2 {
		t.Fatal("counter namespaces were not kept distinct")
	}
	for _, release := range holders {
		release()
	}
	if len(gate.scopes) != 0 || len(gate.pending) != 0 {
		t.Fatal("idle counter state was retained")
	}
}

func waitWriteReferences(t *testing.T, gate *writeAdmission, key writeScope, expected int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		gate.mu.Lock()
		counter := gate.scopes[key]
		ready := counter != nil && counter.references == expected
		gate.mu.Unlock()
		if ready {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("counter did not reach %d admitted callers", expected)
		}
	}
}
