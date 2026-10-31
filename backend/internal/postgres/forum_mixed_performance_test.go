//go:build integration && performance

package postgres

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"odysseia.local/backend/internal/forum"
)

type mixedLatency struct {
	P50MS float64 `json:"p50_ms"`
	P95MS float64 `json:"p95_ms"`
	P99MS float64 `json:"p99_ms"`
}

// Coprime with the 10ms / 40ms ordinary arrival periods, avoiding phase-locked
// snapshots that repeatedly miss the same short-lived database work.
const mixedSampleInterval = 97 * time.Millisecond

type mixedLaneResult struct {
	Name              string         `json:"name"`
	ArrivalRate       int            `json:"arrival_rate"`
	Workers           int            `json:"workers"`
	Scheduled         int            `json:"scheduled"`
	Started           int            `json:"started"`
	Dropped           int            `json:"dropped"`
	Succeeded         int            `json:"succeeded"`
	WindowSucceeded   int            `json:"window_succeeded"`
	WindowRPS         float64        `json:"window_rps"`
	Failures          map[string]int `json:"failures"`
	Latency           mixedLatency   `json:"latency_all_started"`
	SuccessfulLatency mixedLatency   `json:"latency_successful"`
	RefusedLatency    mixedLatency   `json:"latency_capacity_refused"`
	ArrivalLatency    mixedLatency   `json:"latency_from_scheduled_arrival"`
	DispatchLag       mixedLatency   `json:"client_dispatch_lag"`
}

type mixedPoolSample struct {
	Second      float64 `json:"second"`
	Acquired    int32   `json:"acquired"`
	LockWaiters int     `json:"lock_waiters"`
	Active      int     `json:"active"`
	IdleInTx    int     `json:"idle_in_transaction"`
}

type mixedPhaseResult struct {
	Name               string                      `json:"name"`
	Seconds            float64                     `json:"seconds"`
	DrainMS            float64                     `json:"drain_ms"`
	WarmupLanes        []mixedLaneResult           `json:"warmup_lanes"`
	Lanes              []mixedLaneResult           `json:"lanes"`
	PoolEmptyAcquires  int64                       `json:"pool_empty_acquires"`
	PoolWaitMS         float64                     `json:"pool_wait_ms"`
	PoolSamples        []mixedPoolSample           `json:"pool_samples"`
	WriteTimings       map[string]mixedWriteTiming `json:"write_timings"`
	WarmupWriteTimings map[string]mixedWriteTiming `json:"warmup_write_timings"`
}

type mixedSample struct {
	elapsed, arrival, lag time.Duration
	done                  time.Time
	failure               string
}

type mixedLane struct {
	name          string
	rate, workers int // rate=0 uses a closed-loop stressor with fixed workers.
	method        string
	path          func(int) string
}

// Ordinary lanes keep a fixed arrival schedule even when requests slow down.
// Worker exhaustion is reported as dropped demand, never silently queued or
// replaced with an unbounded number of goroutines. Hot writers are a separate
// closed-loop stressor; their rate is not a production traffic assumption.
func TestPerformanceMixedLoad(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 220*time.Second)
	t.Cleanup(cancel)
	fixture := newPerformanceFixture(t, ctx)
	cfg, err := parseConfig(fixtureURL(t, "ODYSSEIA_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.RuntimeParams["application_name"] = "odysseia_performance_probe"
	probe, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(probe.Close)
	requestBody, err := json.Marshal(forum.ReplyDraft{Body: fixture.body})
	if err != nil {
		t.Fatal(err)
	}
	ordinaryPath := func(i int) string { return "/v1/topics/" + fixture.topics[1+i%32].Topic.ID + "/messages" }
	ordinary := []mixedLane{
		{"ordinary_read", 100, 32, http.MethodGet, func(i int) string { return ordinaryPath(i) + "?limit=50" }},
		{"ordinary_write", 25, 8, http.MethodPost, ordinaryPath},
	}
	hot := func(workers int) mixedLane {
		return mixedLane{"hot_write", 0, workers, http.MethodPost, func(int) string { return "/v1/topics/" + fixture.topics[0].Topic.ID + "/messages" }}
	}
	var phases []mixedPhaseResult
	for _, phase := range []struct {
		name  string
		lanes []mixedLane
	}{
		{"ordinary_before", ordinary},
		{"hot_only_32", []mixedLane{hot(32)}},
		{"mixed_hot_32", append(slices.Clone(ordinary), hot(32))},
		{"mixed_hot_128", append(slices.Clone(ordinary), hot(128))},
		{"ordinary_after", ordinary},
	} {
		// Warm each combination separately; retain failures in the report instead
		// of filtering unsuccessful measured requests out of latency statistics.
		warmup := runMixedPhase(t, ctx, fixture, probe, requestBody, phase.name+"_warmup", phase.lanes, 3*time.Second)
		result := runMixedPhase(t, ctx, fixture, probe, requestBody, phase.name, phase.lanes, 20*time.Second)
		result.WarmupLanes = warmup.Lanes
		result.WarmupWriteTimings = warmup.WriteTimings
		phases = append(phases, result)
		for _, lane := range result.Lanes {
			t.Logf("%s/%s started=%d ok=%d dropped=%d failures=%v window=%.1f/s p50=%.2fms p95=%.2fms p99=%.2fms", phase.name, lane.Name, lane.Started, lane.Succeeded, lane.Dropped, lane.Failures, lane.WindowRPS, lane.Latency.P50MS, lane.Latency.P95MS, lane.Latency.P99MS)
		}
	}
	// The fixture has 28225 seeded messages/entries and no consumer. Check all
	// additional commits against distinct event identities, sequence and time.
	var messages, entries, events, matched, pending int64
	err = fixture.owner.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM odysseia.messages)-28225,
		(SELECT count(*) FROM odysseia.forum_entries)-28225,
		(SELECT count(*) FROM odysseia.realtime_outbox WHERE payload#>>'{data,type}'='forum.message.created'),
		(SELECT count(DISTINCT m.id) FROM odysseia.messages m JOIN odysseia.realtime_outbox o
			ON o.payload#>>'{data,data,id}'=m.id::text
			WHERE o.payload#>>'{data,type}'='forum.message.created'
			AND o.payload#>>'{data,data,sequence}'=m.position::text
			AND (o.payload#>>'{data,data,created_at}')::timestamptz=m.created_at),
		(SELECT count(*) FROM odysseia.realtime_outbox)`).Scan(&messages, &entries, &events, &matched, &pending)
	if err != nil || messages != entries || messages != events || messages != matched {
		t.Fatalf("mixed write consistency failed: messages=%d entries=%d events=%d matched=%d: %v", messages, entries, events, matched, err)
	}
	report := struct {
		GoVersion     string             `json:"go_version"`
		GOMAXPROCS    int                `json:"gomaxprocs"`
		PoolMax       int32              `json:"pool_max"`
		ProbePoolMax  int32              `json:"probe_pool_max"`
		SamplerMS     int64              `json:"sampler_interval_ms"`
		SeedMessages  int                `json:"seed_messages"`
		BodyBytes     int                `json:"body_bytes"`
		Writes        int64              `json:"committed_writes_including_warmup"`
		OutboxPending int64              `json:"outbox_pending"`
		Phases        []mixedPhaseResult `json:"phases"`
	}{runtime.Version(), runtime.GOMAXPROCS(0), fixture.pool.Config().MaxConns, 1, mixedSampleInterval.Milliseconds(), 28225, 1024, messages, pending, phases}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("ODYSSEIA_TEST_PERFORMANCE_OUTPUT"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
}

func runMixedPhase(t *testing.T, ctx context.Context, fixture *performanceFixture, probe *pgxpool.Pool, body []byte, name string, lanes []mixedLane, duration time.Duration) mixedPhaseResult {
	t.Helper()
	fixture.timings.reset()
	start := time.Now().Add(100 * time.Millisecond)
	end := start.Add(duration)
	before := fixture.pool.Stat()
	results := make(chan mixedLaneResult, len(lanes))
	for _, lane := range lanes {
		go func() { results <- runMixedLane(ctx, fixture, body, lane, start, end) }()
	}
	type observation struct {
		samples []mixedPoolSample
		err     error
	}
	observations := make(chan observation, 1)
	go func() {
		samples, err := sampleMixedPool(ctx, fixture.pool, probe, start, end)
		observations <- observation{samples, err}
	}()
	result := mixedPhaseResult{Name: name, Seconds: duration.Seconds()}
	for range lanes {
		result.Lanes = append(result.Lanes, <-results)
	}
	result.DrainMS = float64(max(time.Since(end), 0)) / float64(time.Millisecond)
	result.WriteTimings = fixture.timings.take()
	for lane, timing := range result.WriteTimings {
		if timing.InvalidClocks != 0 {
			t.Fatalf("%s: invalid transaction timing intervals", lane)
		}
	}
	after := fixture.pool.Stat()
	observed := <-observations
	if observed.err != nil {
		t.Fatal(observed.err)
	}
	result.PoolSamples = observed.samples
	result.PoolEmptyAcquires = after.EmptyAcquireCount() - before.EmptyAcquireCount()
	result.PoolWaitMS = float64(after.EmptyAcquireWaitTime()-before.EmptyAcquireWaitTime()) / float64(time.Millisecond)
	slices.SortFunc(result.Lanes, func(a, b mixedLaneResult) int { return strings.Compare(a.Name, b.Name) })
	return result
}

func runMixedLane(ctx context.Context, fixture *performanceFixture, body []byte, lane mixedLane, start, end time.Time) mixedLaneResult {
	return runScheduledLane(ctx, lane, start, end, func(index int, due time.Time) mixedSample {
		return makeMixedRequest(ctx, fixture, body, lane, index, due)
	})
}

// Reuse one bounded arrival scheduler for the API-only and WSS fan-out profiles.
// The request callback owns protocol checks and workload-specific correlation.
func runScheduledLane(ctx context.Context, lane mixedLane, start, end time.Time, request func(int, time.Time) mixedSample) mixedLaneResult {
	samples := make(chan mixedSample, lane.workers)
	jobs := make(chan struct {
		index int
		due   time.Time
	})
	var workers sync.WaitGroup
	var scheduled, dropped int
	if lane.rate > 0 {
		for range lane.workers {
			workers.Go(func() {
				for job := range jobs {
					samples <- request(job.index, job.due)
				}
			})
		}
	} else {
		for worker := range lane.workers {
			workers.Go(func() {
				if !waitMixedArrival(ctx, start) {
					return
				}
				for index := worker; time.Now().Before(end) && ctx.Err() == nil; index += lane.workers {
					samples <- request(index, time.Now())
				}
			})
		}
	}
	go func() {
		for index := 0; index < lane.rate*int(end.Sub(start)/time.Second); index++ {
			due := start.Add(time.Duration(index) * time.Second / time.Duration(lane.rate))
			if !waitMixedArrival(ctx, due) {
				break
			}
			scheduled++
			select {
			case jobs <- struct {
				index int
				due   time.Time
			}{index, due}:
			default:
				dropped++
			}
		}
		close(jobs)
		workers.Wait()
		close(samples)
	}()
	result := mixedLaneResult{Name: lane.name, ArrivalRate: lane.rate, Workers: lane.workers, Failures: make(map[string]int)}
	var elapsed, arrivals, lags, successful, capacity []time.Duration
	for sample := range samples {
		result.Started++
		elapsed = append(elapsed, sample.elapsed)
		arrivals = append(arrivals, sample.arrival)
		lags = append(lags, sample.lag)
		if sample.failure != "" {
			result.Failures[sample.failure]++
			if sample.failure == "http_503" {
				capacity = append(capacity, sample.elapsed)
			}
			continue
		}
		result.Succeeded++
		successful = append(successful, sample.elapsed)
		if sample.done.Before(end) {
			result.WindowSucceeded++
		}
	}
	result.Scheduled, result.Dropped = scheduled, dropped
	result.WindowRPS = float64(result.WindowSucceeded) / end.Sub(start).Seconds()
	result.Latency, result.ArrivalLatency, result.DispatchLag = mixedPercentiles(elapsed), mixedPercentiles(arrivals), mixedPercentiles(lags)
	result.SuccessfulLatency, result.RefusedLatency = mixedPercentiles(successful), mixedPercentiles(capacity)
	return result
}

func waitMixedArrival(ctx context.Context, due time.Time) bool {
	delay := time.Until(due)
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func makeMixedRequest(ctx context.Context, fixture *performanceFixture, body []byte, lane mixedLane, index int, due time.Time) mixedSample {
	started := time.Now()
	sample := mixedSample{lag: started.Sub(due)}
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var payload []byte
	if lane.method == http.MethodPost {
		payload = body
	}
	req, err := http.NewRequestWithContext(requestCtx, lane.method, fixture.url+lane.path(index), bytes.NewReader(payload))
	if err != nil {
		sample.failure = "request_construction"
		return finishMixedSample(sample, started, due)
	}
	req.Header.Set("Authorization", "Bearer "+fixture.token)
	req.Header.Set("X-Odysseia-Performance-Lane", lane.name)
	if lane.method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", uuid.NewString())
	}
	response, err := fixture.client.Do(req)
	if err != nil {
		sample.failure = "transport"
		if errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
			sample.failure = "timeout"
		}
		return finishMixedSample(sample, started, due)
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	response.Body.Close()
	want := http.StatusOK
	if lane.method == http.MethodPost {
		want = http.StatusCreated
	}
	switch {
	case readErr != nil:
		sample.failure = "body_read"
	case response.TLS == nil || response.TLS.Version != tls.VersionTLS13 || response.ProtoMajor != 2:
		sample.failure = "protocol"
	case response.StatusCode != want:
		sample.failure = "http_" + strconv.Itoa(response.StatusCode)
	}
	return finishMixedSample(sample, started, due)
}

func finishMixedSample(sample mixedSample, started, due time.Time) mixedSample {
	sample.done = time.Now()
	sample.elapsed, sample.arrival = sample.done.Sub(started), sample.done.Sub(due)
	return sample
}

func mixedPercentiles(values []time.Duration) mixedLatency {
	if len(values) == 0 {
		return mixedLatency{}
	}
	slices.Sort(values)
	percentile := func(p float64) float64 {
		return float64(values[int(math.Ceil(p*float64(len(values))))-1]) / float64(time.Millisecond)
	}
	return mixedLatency{percentile(.5), percentile(.95), percentile(.99)}
}

func sampleMixedPool(ctx context.Context, pool, probe *pgxpool.Pool, start, end time.Time) ([]mixedPoolSample, error) {
	ticker := time.NewTicker(mixedSampleInterval)
	defer ticker.Stop()
	var samples []mixedPoolSample
	for {
		select {
		case now := <-ticker.C:
			if !now.Before(end) {
				return samples, nil
			}
			if now.Before(start) {
				continue
			}
			sample := mixedPoolSample{Second: now.Sub(start).Seconds(), Acquired: pool.Stat().AcquiredConns()}
			queryCtx, cancel := context.WithTimeout(ctx, time.Second)
			err := probe.QueryRow(queryCtx, `SELECT count(*) FILTER (WHERE wait_event_type='Lock'),
				count(*) FILTER (WHERE state='active'),count(*) FILTER (WHERE state='idle in transaction')
				FROM pg_stat_activity WHERE datname=current_database() AND usename=current_user
				AND application_name='odysseia_performance_api'`).Scan(&sample.LockWaiters, &sample.Active, &sample.IdleInTx)
			cancel()
			if err != nil {
				return nil, err
			}
			samples = append(samples, sample)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
