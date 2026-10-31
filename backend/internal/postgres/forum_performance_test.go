//go:build integration && performance

package postgres

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/tokenauth"
)

type performanceResult struct {
	Name              string       `json:"name"`
	Concurrency       int          `json:"concurrency"`
	Requests          int          `json:"requests"`
	Succeeded         int64        `json:"succeeded"`
	CapacityRejected  int64        `json:"capacity_rejected"`
	Errors            int64        `json:"errors"`
	FirstError        string       `json:"first_error,omitempty"`
	Seconds           float64      `json:"seconds"`
	RequestsPerSecond float64      `json:"requests_per_second"`
	P50MS             float64      `json:"p50_ms"`
	P95MS             float64      `json:"p95_ms"`
	P99MS             float64      `json:"p99_ms"`
	SuccessfulLatency mixedLatency `json:"latency_successful"`
	RefusedLatency    mixedLatency `json:"latency_capacity_refused"`
	MeanResponseBytes float64      `json:"mean_response_bytes"`
	PoolEmptyAcquires int64        `json:"pool_empty_acquires"`
	PoolWaitMS        float64      `json:"pool_wait_ms"`
}

// This closed-loop baseline measures the real API, cached-JWKS RS256 validation
// and PostgreSQL TLS together. Client and server share this process; this is not
// an open-arrival load test or a measurement of WSS fan-out / production capacity.
// Capacity refusals remain explicit; throughput counts successful responses,
// while latency percentiles include every started attempt, including refusals.
func TestPerformanceBaseline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 170*time.Second)
	t.Cleanup(cancel)
	fixture := newPerformanceFixture(t, ctx)
	owner, pool, topics := fixture.owner, fixture.pool, fixture.topics
	body, client, token := fixture.body, fixture.client, fixture.token
	client.Timeout = 15 * time.Second
	replyBody, _ := json.Marshal(forum.ReplyDraft{Body: body})
	cursor, _ := json.Marshal(struct {
		Version  int    `json:"v"`
		Kind     string `json:"kind"`
		Scope    string `json:"scope"`
		Position int64  `json:"position,string"`
	}{1, "messages", topics[0].Topic.ID, 19000})
	deepCursor := base64.RawURLEncoding.EncodeToString(cursor)
	type scenario struct {
		name, method string
		count        int
		path         func(int) string
	}
	cases := []scenario{
		{"history_first_50", http.MethodGet, 2000, func(int) string { return "/v1/topics/" + topics[0].Topic.ID + "/messages?limit=50" }},
		{"history_after_19000", http.MethodGet, 2000, func(int) string {
			return "/v1/topics/" + topics[0].Topic.ID + "/messages?limit=50&after=" + url.QueryEscape(deepCursor)
		}},
		{"reply_32_topics", http.MethodPost, 600, func(i int) string { return "/v1/topics/" + topics[1+i%32].Topic.ID + "/messages" }},
		{"reply_one_hot_topic", http.MethodPost, 600, func(int) string { return "/v1/topics/" + topics[0].Topic.ID + "/messages" }},
	}
	var results []performanceResult
	for _, test := range cases {
		request := func(i int) (int64, error) {
			var data []byte
			if test.method == http.MethodPost {
				data = replyBody
			}
			req, err := http.NewRequestWithContext(ctx, test.method, fixture.url+test.path(i), bytes.NewReader(data))
			if err != nil {
				return 0, err
			}
			req.Header.Set("Authorization", "Bearer "+token)
			if test.method == http.MethodPost {
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Idempotency-Key", uuid.NewString())
			}
			response, err := client.Do(req)
			if err != nil {
				return 0, err
			}
			defer response.Body.Close()
			if response.TLS.Version != tls.VersionTLS13 || response.ProtoMajor != 2 {
				return 0, fmt.Errorf("expected TLS1.3 / HTTP2")
			}
			if test.method == http.MethodPost && response.StatusCode == http.StatusServiceUnavailable {
				data, err := io.ReadAll(response.Body)
				if err != nil {
					return 0, err
				}
				var problem struct {
					Code string `json:"code"`
				}
				if err := json.Unmarshal(data, &problem); err != nil {
					return 0, err
				}
				if problem.Code != "write_capacity_exceeded" || response.Header.Get("Retry-After") != "1" {
					return 0, fmt.Errorf("unexpected capacity response contract")
				}
				return int64(len(data)), forum.ErrBusy
			}
			read, err := io.Copy(io.Discard, response.Body)
			if err != nil {
				return read, err
			}
			want := http.StatusOK
			if test.method == http.MethodPost {
				want = http.StatusCreated
			}
			if response.StatusCode != want {
				return read, fmt.Errorf("unexpected response status %d", response.StatusCode)
			}
			return read, nil
		}
		for _, concurrency := range []int{1, 8, 32} {
			warm := measurePerformanceRequests(concurrency, 64, request)
			if warm.Errors != 0 {
				t.Fatalf("performance warmup failed: %s", warm.FirstError)
			}
			before := pool.Stat()
			result := measurePerformanceRequests(concurrency, test.count, request)
			after := pool.Stat()
			result.Name = test.name
			result.PoolEmptyAcquires = after.EmptyAcquireCount() - before.EmptyAcquireCount()
			result.PoolWaitMS = float64(after.EmptyAcquireWaitTime()-before.EmptyAcquireWaitTime()) / float64(time.Millisecond)
			results = append(results, result)
			t.Logf("%s c=%d n=%d succeeded=%d refused=%d errors=%d throughput=%.1f/s p50=%.2fms p95=%.2fms p99=%.2fms pool_waits=%d", result.Name, concurrency, result.Requests, result.Succeeded, result.CapacityRejected, result.Errors, result.RequestsPerSecond, result.P50MS, result.P95MS, result.P99MS, result.PoolEmptyAcquires)
		}
	}
	var pending int64
	if err := owner.QueryRow(ctx, "SELECT count(*) FROM odysseia.realtime_outbox").Scan(&pending); err != nil {
		t.Fatal(err)
	}
	plan := performanceReadPlan(t, ctx, owner, topics[0].Topic.ID)
	writeSamples := performanceWriteSamples(t, ctx, fixtureURL(t, "ODYSSEIA_TEST_DATABASE_URL"), fixture.actor, topics[0].Topic.ID, topics[0].OpeningMessage.ID)
	report := struct {
		GoVersion     string                   `json:"go_version"`
		NumCPU        int                      `json:"num_cpu"`
		GOMAXPROCS    int                      `json:"gomaxprocs"`
		PoolMax       int32                    `json:"pool_max"`
		SeedMessages  int                      `json:"seed_messages"`
		BodyBytes     int                      `json:"body_bytes"`
		OutboxPending int64                    `json:"outbox_pending"`
		Results       []performanceResult      `json:"results"`
		DeepReadPlan  json.RawMessage          `json:"deep_read_plan"`
		WriteSamples  []performanceWriteSample `json:"write_samples"`
	}{runtime.Version(), runtime.NumCPU(), runtime.GOMAXPROCS(0), pool.Config().MaxConns, 28225, 1024, pending, results, plan, writeSamples}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("ODYSSEIA_TEST_PERFORMANCE_OUTPUT"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		if result.Errors != 0 {
			t.Errorf("%s had %d request errors", result.Name, result.Errors)
		}
	}
}

func measurePerformanceRequests(concurrency, total int, request func(int) (int64, error)) performanceResult {
	jobs := make(chan int, total)
	for i := 0; i < total; i++ {
		jobs <- i
	}
	close(jobs)
	latencies := make([]int64, total)
	outcomes := make([]uint8, total) // 0=success, 1=capacity refusal, 2=other error.
	var failures, refused, responseBytes atomic.Int64
	var firstFailure sync.Once
	var firstError string
	var workers sync.WaitGroup
	start := make(chan struct{})
	for range concurrency {
		workers.Go(func() {
			<-start
			for index := range jobs {
				tick := time.Now()
				read, err := request(index)
				latencies[index] = time.Since(tick).Nanoseconds()
				responseBytes.Add(read)
				if errors.Is(err, forum.ErrBusy) {
					outcomes[index] = 1
					refused.Add(1)
					continue
				}
				if err != nil {
					outcomes[index] = 2
					failures.Add(1)
					firstFailure.Do(func() { firstError = err.Error() })
				}
			}
		})
	}
	started := time.Now()
	close(start)
	workers.Wait()
	elapsed := time.Since(started).Seconds()
	var successful, capacity []time.Duration
	for index, elapsed := range latencies {
		switch outcomes[index] {
		case 0:
			successful = append(successful, time.Duration(elapsed))
		case 1:
			capacity = append(capacity, time.Duration(elapsed))
		}
	}
	slices.Sort(latencies)
	percentile := func(p float64) float64 {
		return float64(latencies[int(math.Ceil(p*float64(total)))-1]) / float64(time.Millisecond)
	}
	succeeded := int64(total) - failures.Load() - refused.Load()
	return performanceResult{Concurrency: concurrency, Requests: total, Succeeded: succeeded, CapacityRejected: refused.Load(), Errors: failures.Load(), FirstError: firstError, Seconds: elapsed, RequestsPerSecond: float64(succeeded) / elapsed, P50MS: percentile(.5), P95MS: percentile(.95), P99MS: percentile(.99), SuccessfulLatency: mixedPercentiles(successful), RefusedLatency: mixedPercentiles(capacity), MeanResponseBytes: float64(responseBytes.Load()) / float64(total)}
}

func seedPerformanceMessages(t *testing.T, ctx context.Context, pool *pgxpool.Pool, topic, actor, body string, count int) {
	t.Helper()
	_, err := pool.Exec(ctx, `WITH inserted AS (
		INSERT INTO odysseia.messages(id,stream_id,position,author_id,body,request_id,request_hash)
		SELECT gen_random_uuid(),$1,position+1,$2,$3,gen_random_uuid(),decode(repeat('ab',32),'hex') FROM generate_series(1,$4::integer) AS position
		RETURNING id,stream_id)
		INSERT INTO odysseia.forum_entries(stream_id,message_id) SELECT stream_id,id FROM inserted`, topic, actor, body, count)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE odysseia.message_streams SET last_position=$2 WHERE id=$1`, topic, count+1); err != nil {
		t.Fatal(err)
	}
}

func performanceIdentity(t *testing.T, ctx context.Context) (tokenauth.Config, *tokenauth.Verifier, func(string) string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwk, err := jwkset.NewJWKFromKey(key.Public(), jwkset.JWKOptions{Metadata: jwkset.JWKMetadataOptions{KID: "performance", ALG: jwkset.AlgRS256, USE: jwkset.UseSig}})
	if err != nil {
		t.Fatal(err)
	}
	issuer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jwkset.JWKSMarshal{Keys: []jwkset.JWKMarshal{jwk.Marshal()}})
	}))
	issuer.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	issuer.StartTLS()
	t.Cleanup(issuer.Close)
	caFile := filepath.Join(t.TempDir(), "issuer-ca.crt")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issuer.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	config := tokenauth.Config{Issuer: issuer.URL, Audience: "performance-api", JWKSURL: issuer.URL + "/jwks", CAFile: caFile}
	verifier, err := tokenauth.New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	// Subjects are distinct in fan-out tests; API verification remains the same
	// cached-JWKS RS256 path used by the single-actor baselines.
	sign := func(subject string) string {
		t.Helper()
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": issuer.URL, "sub": subject, "aud": "performance-api", "exp": time.Now().Add(10 * time.Minute).Unix(), "iat": time.Now().Add(-time.Second).Unix(), "jti": uuid.NewString(), "client_id": "local-baseline"})
		token.Header["typ"], token.Header["kid"] = "at+jwt", "performance"
		raw, err := token.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	return config, verifier, sign
}

func performanceReadPlan(t *testing.T, ctx context.Context, pool *pgxpool.Pool, topic string) json.RawMessage {
	t.Helper()
	var plan []byte
	// Explain the actual cache-miss content query. Current membership and the
	// append bound are obtained separately for every API read.
	err := pool.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+messagePageSQL, topic, 19000, 19051, 51).Scan(&plan)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}
