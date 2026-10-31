package telemetry

import (
	"context"
	"sync"
	"testing"
)

func TestConcurrentMeasurementsRemainBoundedAndSnapshotsAreDetached(t *testing.T) {
	var m Measurements
	ctx := WithMeasurements(context.Background(), &m)
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() {
			for range 100 {
				Start(ctx, IdentityLookup)()
				Reject(ctx, ScopeQueueFull)
			}
		})
	}
	workers.Wait()
	snapshot := m.Snapshot()
	d := snapshot.Stages["identity_lookup"]
	var count uint64
	for _, bucket := range d.Buckets {
		count += bucket
	}
	if d.Count != 3200 || count != 3200 || snapshot.Rejections["scope_queue_full"] != 3200 {
		t.Fatal("concurrent measurements were lost")
	}
	d.Buckets[0] = 999999
	snapshot.Stages["unexpected-resource-id"] = d
	again := m.Snapshot()
	if len(again.Stages) != int(stageCount) || again.Stages["identity_lookup"].Buckets[0] == 999999 {
		t.Fatal("snapshot exposed mutable recorder state")
	}
}
