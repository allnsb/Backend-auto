//go:build integration && realtime && performance

package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/telemetry"
)

type fanoutScenario struct {
	HotTopics       int `json:"hot_topics"`
	ReadersPerTopic int `json:"readers_per_topic"`
	WritesPerSecond int `json:"writes_per_second_per_topic"`
}

type fanoutAttempt struct {
	request      string
	topic, actor int
	draft        int
	started      time.Time
	acknowledged bool
	responseHash [32]byte
}

type fanoutBook struct {
	mu       sync.Mutex
	attempts []fanoutAttempt
}

type fanoutExpected struct {
	hash      [32]byte
	submitted time.Time
	eventID   string // Filled by the first checked receiver, then compared across peers.
}

type fanoutDatabase struct {
	ByKind         map[string]int              `json:"committed_by_kind"`
	Committed      int                         `json:"committed"`
	Acknowledged   int                         `json:"acknowledged"`
	Unacknowledged int                         `json:"committed_without_successful_http_response"`
	ReplyShapes    [3]int                      `json:"root_quote_root_quote_child"`
	Expected       []map[int64]*fanoutExpected `json:"-"`
}

type fanoutPoolSample struct {
	Second      float64           `json:"second"`
	Acquired    int32             `json:"acquired"`
	LockWaiters int               `json:"lock_waiters"`
	Outbox      int64             `json:"outbox_pending"`
	Oldest      float64           `json:"outbox_oldest_seconds"`
	Goroutines  int               `json:"go_process_goroutines"`
	HeapMB      float64           `json:"go_process_heap_mb"`
	API         fanoutAPISnapshot `json:"api_process"`
	WaitEvents  map[string]int    `json:"api_database_wait_events"`
	Partitions  map[int]int64     `json:"outbox_by_partition"`
}

type fanoutPhase struct {
	Name            string                      `json:"name"`
	Scenario        fanoutScenario              `json:"scenario"`
	Seconds         float64                     `json:"seconds"`
	StartedAt       time.Time                   `json:"started_at"`
	HTTPDrainMS     float64                     `json:"http_drain_ms"`
	DeliveryDrainMS float64                     `json:"delivery_drain_ms"`
	Lanes           []mixedLaneResult           `json:"lanes"`
	Database        fanoutDatabase              `json:"database"`
	Delivery        fanoutDeliveryResult        `json:"delivery"`
	OutboxPending   int64                       `json:"outbox_after_drain"`
	PoolWaitMS      float64                     `json:"pool_wait_ms"`
	Samples         []fanoutPoolSample          `json:"samples"`
	WriteTimings    map[string]mixedWriteTiming `json:"write_timings"`
	PageCache       fanoutCacheStats            `json:"page_cache"`
	APIBefore       fanoutAPISnapshot           `json:"api_before"`
	APIAfter        fanoutAPISnapshot           `json:"api_after"`
	Measurements    telemetry.Snapshot          `json:"measurements"`
}

type fanoutCacheStats struct {
	Hits   uint64 `json:"hits"`
	Misses uint64 `json:"misses"`
	Loads  uint64 `json:"database_page_loads"`
}

// This is a measured load profile, not an acceptance SLA. Capacity refusals and
// delivery remaining after the bounded drain are retained in the JSON report.
func TestPerformanceHotTopicsFanout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 390*time.Second)
	// Child cleanup must finish before its command context is cancelled.
	// A deferred cancel would kill a healthy API before testing runs Cleanups.
	t.Cleanup(cancel)
	f := newFanoutFixture(t, ctx)
	report := struct {
		GoVersion     string        `json:"go_version"`
		GOMAXPROCS    int           `json:"gomaxprocs"`
		PoolMax       int32         `json:"pool_max"`
		Subjects      int           `json:"distinct_provisioned_subjects"`
		SeedMessages  int           `json:"seed_messages"`
		BodyBytes     int           `json:"body_bytes"`
		ProcessLayout string        `json:"process_layout"`
		Storage       string        `json:"database_storage"`
		Phases        []fanoutPhase `json:"phases"`
	}{GoVersion: runtime.Version(), GOMAXPROCS: runtime.GOMAXPROCS(0), PoolMax: f.api.ready.PoolMax, Subjects: len(f.users), SeedMessages: len(f.topics) * 258, BodyBytes: fanoutBodyBytes, ProcessLayout: "generator+SDK / API child / Centrifugo / PostgreSQL container; same host", Storage: os.Getenv("ODYSSEIA_TEST_DATABASE_STORAGE")}
	for _, scenario := range []fanoutScenario{{4, 128, 25}, {4, 128, 100}, {16, 64, 25}, {16, 64, 100}} {
		peers := newFanoutPeers(t, ctx, f, scenario)
		for _, phase := range []struct {
			name     string
			duration time.Duration
		}{{"warmup", 3 * time.Second}, {"measured", 12 * time.Second}} {
			result := runFanoutPhase(t, ctx, f, peers, scenario, phase.name, phase.duration)
			report.Phases = append(report.Phases, result)
			encoded, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(os.Getenv("ODYSSEIA_TEST_PERFORMANCE_OUTPUT"), encoded, 0600); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s topics=%d readers/topic=%d offered/topic=%d/s committed=%d deliveries=%d/%d missing=%d p95=%.2fms outbox=%d", phase.name, scenario.HotTopics, scenario.ReadersPerTopic, scenario.WritesPerSecond, result.Database.Committed, result.Delivery.Matched, result.Delivery.Expected, result.Delivery.Missing, result.Delivery.SendToReceive.P95MS, result.OutboxPending)
			if result.Delivery.Invalid != 0 || result.Delivery.OutOfOrder != 0 {
				t.Fatal("fan-out payload, event identity or ordering validation failed; aggregate report retained")
			}
		}
		closeFanoutPeers(peers)
	}
}

type fanoutLane struct {
	spec     mixedLane
	hotTopic int
}

func runFanoutPhase(t *testing.T, ctx context.Context, f *fanoutFixture, peers []*fanoutPeer, scenario fanoutScenario, name string, duration time.Duration) fanoutPhase {
	t.Helper()
	base := fanoutPositions(t, ctx, f)
	for _, peer := range peers {
		peer.reset(base[peer.topic])
	}
	phaseID := fmt.Sprintf("%s-%d-%d", name, scenario.HotTopics, scenario.WritesPerSecond)
	var profile *os.File
	profileDir := os.Getenv("ODYSSEIA_TEST_API_PROFILE_DIR")
	if name == "measured" && profileDir != "" {
		var err error
		profile, err = os.OpenFile(filepath.Join(profileDir, phaseID+".generator.cpu.pprof"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err := pprof.StartCPUProfile(profile); err != nil {
			t.Fatal(err)
		}
	}
	defer stopFanoutProfile(t, &profile)
	before, err := f.api.call(ctx, fanoutAPICommand{Operation: "reset", Phase: phaseID})
	if err != nil {
		t.Fatal(err)
	}
	book := &fanoutBook{}
	lanes := []fanoutLane{{mixedLane{name: "ordinary_read", rate: 100, workers: 32, method: http.MethodGet}, -1}, {mixedLane{name: "ordinary_write", rate: 25, workers: 8, method: http.MethodPost}, -1}}
	for topic := range scenario.HotTopics {
		lanes = append(lanes, fanoutLane{mixedLane{name: fmt.Sprintf("hot_%02d", topic), rate: scenario.WritesPerSecond, workers: 32, method: http.MethodPost}, topic})
	}
	start := time.Now().Add(100 * time.Millisecond)
	end := start.Add(duration)
	cacheBefore := before.PageCache
	results := make(chan mixedLaneResult, len(lanes))
	for _, lane := range lanes {
		go func() {
			results <- runScheduledLane(ctx, lane.spec, start, end, func(index int, due time.Time) mixedSample {
				return performFanoutRequest(ctx, f, book, lane, scenario.ReadersPerTopic, index, due)
			})
		}()
	}
	type observation struct {
		samples []fanoutPoolSample
		err     error
	}
	observed := make(chan observation, 1)
	go func() { samples, err := sampleFanout(ctx, f, start, end); observed <- observation{samples, err} }()
	result := fanoutPhase{Name: name, Scenario: scenario, Seconds: duration.Seconds(), StartedAt: start.UTC()}
	for range lanes {
		result.Lanes = append(result.Lanes, <-results)
	}
	result.HTTPDrainMS = float64(max(time.Since(end), 0)) / float64(time.Millisecond)
	after, err := f.api.call(ctx, fanoutAPICommand{Operation: "take"})
	if err != nil {
		t.Fatal(err)
	}
	result.WriteTimings, after.Timings = after.Timings, nil
	result.APIBefore, result.APIAfter = before, after
	result.Measurements = differenceMeasurements(before.Measurements, after.Measurements)
	result.PageCache = fanoutCacheStats{after.PageCache.Hits - cacheBefore.Hits, after.PageCache.Misses - cacheBefore.Misses, after.PageCache.Loads - cacheBefore.Loads}
	for _, timing := range result.WriteTimings {
		if timing.InvalidClocks != 0 {
			t.Fatal("invalid write timing intervals")
		}
	}
	result.PoolWaitMS = float64(after.PoolWaitNS-before.PoolWaitNS) / float64(time.Millisecond)
	observationResult := <-observed
	if observationResult.err != nil {
		t.Fatal(observationResult.err)
	}
	result.Samples = observationResult.samples
	result.Database = fanoutCommitted(t, ctx, f, base, book.attempts)
	drainStarted := time.Now()
	result.OutboxPending = drainFanout(t, ctx, f, peers, result.Database, scenario, 30*time.Second)
	result.DeliveryDrainMS = float64(time.Since(drainStarted)) / float64(time.Millisecond)
	result.Delivery = finishFanout(peers, result.Database, scenario, start, end)
	// Retain aggregates in the report, not the phase's per-message correlation map.
	result.Database.Expected = nil
	slices.SortFunc(result.Lanes, func(a, b mixedLaneResult) int { return strings.Compare(a.Name, b.Name) })
	return result
}

func performFanoutRequest(ctx context.Context, f *fanoutFixture, book *fanoutBook, lane fanoutLane, readers, index int, due time.Time) mixedSample {
	started := time.Now()
	sample := mixedSample{lag: started.Sub(due)}
	topic, actor := lane.hotTopic, fanoutUsers
	if topic < 0 {
		topic = fanoutHotTopics + index%fanoutOrdinaryTopics
	} else {
		actor = topic*readers + index%readers
	}
	path := f.topics[topic].path + "/messages"
	var body []byte
	attempt := fanoutAttempt{request: uuid.NewString(), topic: topic, actor: actor, draft: index % 3, started: started}
	if lane.spec.method == http.MethodPost {
		body = f.topics[topic].drafts[attempt.draft]
		defer func() { book.mu.Lock(); book.attempts = append(book.attempts, attempt); book.mu.Unlock() }()
	} else {
		path += "?limit=50"
	}
	status, data, failure := f.call(ctx, actor, lane.spec.method, path, attempt.request, body)
	sample.failure = failure
	if failure != "" {
		return finishMixedSample(sample, started, due)
	}
	want := http.StatusOK
	if lane.spec.method == http.MethodPost {
		want = http.StatusCreated
	}
	if status != want {
		sample.failure = "http_" + strconv.Itoa(status)
		return finishMixedSample(sample, started, due)
	}
	if lane.spec.method == http.MethodPost {
		var entry forum.Entry
		if err := json.Unmarshal(data, &entry); err != nil {
			sample.failure = "invalid_success_payload"
			return finishMixedSample(sample, started, due)
		}
		canonical, err := json.Marshal(entry)
		if err != nil {
			sample.failure = "invalid_success_payload"
			return finishMixedSample(sample, started, due)
		}
		attempt.acknowledged, attempt.responseHash = true, sha256.Sum256(canonical)
	}
	return finishMixedSample(sample, started, due)
}

func fanoutPositions(t *testing.T, ctx context.Context, f *fanoutFixture) []int64 {
	t.Helper()
	ids := make([]string, len(f.topics))
	for index, topic := range f.topics {
		ids[index] = topic.id
	}
	rows, err := f.owner.Query(ctx, `SELECT s.last_position FROM unnest($1::uuid[]) WITH ORDINALITY i(id,n) JOIN odysseia.message_streams s ON s.id=i.id ORDER BY i.n`, ids)
	if err != nil {
		t.Fatal(err)
	}
	positions, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil || len(positions) != len(ids) {
		t.Fatal("cannot snapshot message counters")
	}
	return positions
}

func fanoutCommitted(t *testing.T, ctx context.Context, f *fanoutFixture, base []int64, attempts []fanoutAttempt) fanoutDatabase {
	t.Helper()
	requests := make(map[string]fanoutAttempt, len(attempts))
	acknowledged := 0
	for _, attempt := range attempts {
		requests[attempt.request] = attempt
		if attempt.acknowledged {
			acknowledged++
		}
	}
	ids := make([]string, len(f.topics))
	for index, topic := range f.topics {
		ids[index] = topic.id
	}
	rows, err := f.owner.Query(ctx, `SELECT `+entryColumns+`,m.request_id::text,i.n FROM odysseia.messages m
		LEFT JOIN odysseia.forum_entries e ON e.message_id=m.id
		JOIN unnest($1::uuid[],$2::bigint[]) WITH ORDINALITY i(id,last_position,n) ON i.id=m.stream_id
		WHERE m.position>i.last_position ORDER BY i.n,m.position`, ids, base)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := fanoutDatabase{Expected: make([]map[int64]*fanoutExpected, len(ids)), ByKind: make(map[string]int)}
	counts := make([]int64, len(ids))
	for index := range ids {
		result.Expected[index] = make(map[int64]*fanoutExpected)
	}
	for rows.Next() {
		var entry forum.Entry
		var request string
		var ordinal int
		err := rows.Scan(&entry.ID, &entry.StreamID, &entry.Sequence, &entry.AuthorID, &entry.Body, &entry.QuoteID, &entry.CreatedAt, &entry.RootID, &request, &ordinal)
		if err != nil {
			t.Fatal(err)
		}
		attempt, found := requests[request]
		index := ordinal - 1
		counts[index]++
		if !found || attempt.topic != index || entry.Sequence != base[index]+counts[index] || entry.AuthorID != f.users[attempt.actor].id || entry.Body != f.body {
			t.Fatal("database commit does not match its initiating HTTPS request or stream sequence")
		}
		var draft forum.ReplyDraft
		if err := json.Unmarshal(f.topics[index].drafts[attempt.draft], &draft); err != nil {
			t.Fatal(err)
		}
		if entry.RootID != draft.RootID || entry.QuoteID != draft.QuoteID {
			t.Fatal("threaded root and quote were not preserved")
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(encoded)
		if attempt.acknowledged && hash != attempt.responseHash {
			t.Fatal("HTTPS acknowledgement differs from persisted message")
		}
		result.Committed++
		result.ByKind[f.topics[index].kind]++
		result.ReplyShapes[attempt.draft]++
		if attempt.acknowledged {
			result.Acknowledged++
		} else {
			result.Unacknowledged++
		}
		result.Expected[index][entry.Sequence] = &fanoutExpected{hash: hash, submitted: attempt.started}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if result.Acknowledged != acknowledged {
		t.Fatal("an acknowledged HTTPS write is absent from the database")
	}
	return result
}

func sampleFanout(ctx context.Context, f *fanoutFixture, start, end time.Time) ([]fanoutPoolSample, error) {
	if !waitMixedArrival(ctx, start) {
		return nil, ctx.Err()
	}
	ticker := time.NewTicker(503 * time.Millisecond)
	defer ticker.Stop()
	var samples []fanoutPoolSample
	for tick := range ticker.C {
		if !tick.Before(end) {
			return samples, nil
		}
		api, err := f.api.call(ctx, fanoutAPICommand{Operation: "stats"})
		if err != nil {
			return samples, err
		}
		sample := fanoutPoolSample{Second: tick.Sub(start).Seconds(), Acquired: api.Acquired, API: api, Goroutines: runtime.NumGoroutine()}
		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)
		sample.HeapMB = float64(memory.HeapAlloc) / (1024 * 1024)
		rows, err := f.probe.Query(ctx, `SELECT COALESCE(wait_event_type||'/'||wait_event,'CPU_or_running'),count(*)
			FROM pg_stat_activity WHERE datname=current_database() AND application_name='odysseia_performance_api' AND state='active' GROUP BY 1`)
		if err != nil {
			return samples, err
		}
		waits, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
			Name  string
			Count int
		}])
		if err != nil {
			return samples, err
		}
		sample.WaitEvents = make(map[string]int, len(waits))
		for _, wait := range waits {
			sample.WaitEvents[wait.Name] = wait.Count
			if strings.HasPrefix(wait.Name, "Lock/") {
				sample.LockWaiters += wait.Count
			}
		}
		backlog, err := ReadRealtimeBacklog(ctx, f.observer)
		if err != nil {
			return samples, err
		}
		sample.Outbox, sample.Oldest = backlog.Pending, backlog.OldestAgeSeconds
		partitionRows, err := f.owner.Query(ctx, `SELECT partition,count(*) FROM odysseia.realtime_outbox GROUP BY partition`)
		if err != nil {
			return samples, err
		}
		partitions, err := pgx.CollectRows(partitionRows, pgx.RowToStructByPos[struct {
			Partition int
			Count     int64
		}])
		if err != nil {
			return samples, err
		}
		sample.Partitions = make(map[int]int64, len(partitions))
		for _, partition := range partitions {
			sample.Partitions[partition.Partition] = partition.Count
		}
		samples = append(samples, sample)
	}
	return samples, nil
}
