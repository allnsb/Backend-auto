//go:build integration && realtime && performance

package postgres

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"odysseia.local/backend/internal/telemetry"
)

// All three product forms share the real API process and its total pool budget.
// Fixed-arrival load, refused and unissued commands are retained independently.
func TestPerformanceDiscussionModes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 390*time.Second)
	t.Cleanup(cancel)
	f := newFanoutFixtureWithModes(t, ctx, true)
	seconds := 60
	if raw := os.Getenv("ODYSSEIA_TEST_PHASE_SECONDS"); raw != "" {
		var err error
		seconds, err = strconv.Atoi(raw)
		if err != nil || seconds < 10 || seconds > 60 {
			t.Fatal("measurement seconds must be in 10..60")
		}
	}
	report := struct {
		Version       string         `json:"version"`
		GoVersion     string         `json:"go_version"`
		PoolMax       int32          `json:"pool_max"`
		Subjects      int            `json:"distinct_provisioned_subjects"`
		SeedMessages  int            `json:"seed_messages"`
		BodyBytes     int            `json:"body_bytes"`
		Storage       string         `json:"database_storage"`
		ProcessLayout string         `json:"process_layout"`
		Streams       map[string]int `json:"streams_by_kind"`
		HotStreams    map[string]int `json:"hot_streams_by_kind"`
		Phases        []fanoutPhase  `json:"phases"`
	}{Version: "0.5.1", GoVersion: runtime.Version(), PoolMax: f.api.ready.PoolMax, Subjects: len(f.users), SeedMessages: len(f.topics) * 258, BodyBytes: fanoutBodyBytes, Storage: os.Getenv("ODYSSEIA_TEST_DATABASE_STORAGE"), ProcessLayout: "generator+SDK / API child / Centrifugo / PostgreSQL container; same physical host", Streams: make(map[string]int), HotStreams: make(map[string]int)}
	for index, stream := range f.topics {
		report.Streams[stream.kind]++
		if index < 15 {
			report.HotStreams[stream.kind]++
		}
	}
	for _, rate := range []int{10, 25, 50} {
		scenario := fanoutScenario{HotTopics: 15, ReadersPerTopic: 64, WritesPerSecond: rate}
		peers := newFanoutPeers(t, ctx, f, scenario)
		for _, phase := range []struct {
			name     string
			duration time.Duration
		}{{"warmup", 3 * time.Second}, {"measured", time.Duration(seconds) * time.Second}} {
			result := runFanoutPhase(t, ctx, f, peers, scenario, phase.name, phase.duration)
			report.Phases = append(report.Phases, result)
			data, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(os.Getenv("ODYSSEIA_TEST_PERFORMANCE_OUTPUT"), data, 0600); err != nil {
				t.Fatal(err)
			}
			var refused, unsent, succeeded int
			for _, lane := range result.Lanes {
				refused += lane.Failures["http_503"]
				unsent += lane.Dropped
				succeeded += lane.Succeeded
			}
			t.Logf("%s per-stream=%d/s successes=%d refused=%d unsent=%d committed=%v deliveries=%d/%d receive_p95=%.2fms rejections=%v", phase.name, rate, succeeded, refused, unsent, result.Database.ByKind, result.Delivery.Matched, result.Delivery.Expected, result.Delivery.SendToReceive.P95MS, result.Measurements.Rejections)
			if result.Delivery.Missing != 0 || result.Delivery.Invalid != 0 || result.Delivery.OutOfOrder != 0 || result.OutboxPending != 0 {
				t.Error("committed discussion delivery did not complete correctly; full report retained")
			}
			var measuredRefusals uint64
			for reason, count := range result.Measurements.Rejections {
				if reason != "request_canceled" {
					measuredRefusals += count
				}
			}
			if measuredRefusals != uint64(refused) {
				t.Error("internal admission reasons do not match HTTP capacity refusals")
			}
		}
		closeFanoutPeers(peers)
	}
}

// Snapshot queries never reset live counters. Per-phase deltas retain the fixed
// histogram boundaries; bucket quantiles are upper bounds, not exact samples.
func differenceMeasurements(before, after telemetry.Snapshot) telemetry.Snapshot {
	result := telemetry.Snapshot{BoundsNS: after.BoundsNS, Stages: make(map[string]telemetry.Distribution), Rejections: make(map[string]uint64)}
	for name, latest := range after.Stages {
		previous := before.Stages[name]
		d := telemetry.Distribution{Count: latest.Count - previous.Count, TotalNS: latest.TotalNS - previous.TotalNS, Buckets: make([]uint64, len(latest.Buckets))}
		for index, count := range latest.Buckets {
			d.Buckets[index] = count - previous.Buckets[index]
		}
		result.Stages[name] = d
	}
	for reason, count := range after.Rejections {
		result.Rejections[reason] = count - before.Rejections[reason]
	}
	return result
}
