//go:build integration && realtime && performance

package postgres

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"odysseia.local/backend/internal/httpapi"
	"odysseia.local/backend/internal/realtime"
	"odysseia.local/backend/internal/telemetry"
	"odysseia.local/backend/internal/tokenauth"
)

type fanoutAPIConfig struct {
	Auth                    tokenauth.Config
	ConnectionKey, ProxyKey string
	ProfileDir              string
	ReadConnections         int32 // Zero keeps one shared pool of sixteen.
}

type fanoutAPICommand struct {
	Operation string
	Phase     string
	Config    *fanoutAPIConfig `json:",omitempty"`
}

type fanoutAPISnapshot struct {
	PID                         int                         `json:"pid"`
	URL                         string                      `json:"-"`
	Acquired                    int32                       `json:"acquired"`
	PoolMax                     int32                       `json:"pool_max"`
	PoolWaitNS                  int64                       `json:"pool_wait_ns"`
	Goroutines                  int                         `json:"goroutines"`
	HeapBytes                   uint64                      `json:"heap_bytes"`
	Allocated                   uint64                      `json:"allocated_bytes"`
	GCCycles                    uint32                      `json:"gc_cycles"`
	GCPauseNS                   uint64                      `json:"gc_pause_ns"`
	PageCache                   fanoutCacheStats            `json:"page_cache"`
	Timings                     map[string]mixedWriteTiming `json:"write_timings,omitempty"`
	Measurements                telemetry.Snapshot          `json:"measurements"`
	PoolMode                    string                      `json:"pool_mode"`
	ReadPoolMax, WritePoolMax   int32
	ReadAcquired, WriteAcquired int32
}

// Only this tagged fixture uses the stdin/stdout control pipe. No diagnostic
// route or profiling listener is installed in the production HTTPS handler.
// The child owns its pool, page cache, GC and scheduler; SDK clients stay here.
type fanoutAPIProcess struct {
	mu        sync.Mutex
	command   *exec.Cmd
	input     io.WriteCloser
	responses chan fanoutAPISnapshot
	done      chan struct{}
	err       error
	log       *os.File
	ready     fanoutAPISnapshot
}

const fanoutProtocolPrefix = "ODYSSEIA_API_SNAPSHOT "

func startFanoutAPI(t *testing.T, ctx context.Context, config fanoutAPIConfig) *fanoutAPIProcess {
	t.Helper()
	binary := os.Getenv("ODYSSEIA_TEST_API_BINARY")
	if binary == "" {
		var err error
		binary, err = os.Executable()
		if err != nil {
			t.Fatal(err)
		}
	}
	dir, err := os.MkdirTemp(os.Getenv("ODYSSEIA_TEST_REALTIME_RUNTIME"), "api-")
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.OpenFile(filepath.Join(dir, "process.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, binary, "-test.run=^TestPerformanceAPIWorker$", "-test.v", "-test.timeout=400s")
	command.Env = append(os.Environ(), "ODYSSEIA_TEST_API_WORKER=1")
	command.Stderr = log
	input, err := command.StdinPipe()
	if err != nil {
		log.Close()
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		input.Close()
		log.Close()
		t.Fatal(err)
	}
	p := &fanoutAPIProcess{command: command, input: input, responses: make(chan fanoutAPISnapshot, 1), done: make(chan struct{}), log: log}
	if err := command.Start(); err != nil {
		input.Close()
		log.Close()
		t.Fatal(err)
	}
	// Read stdout before Wait closes its pipe. Preserve non-protocol output and
	// the actual exit status so an unexplained process exit cannot look like PASS.
	go func() {
		defer close(p.responses)
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, fanoutProtocolPrefix) {
				fmt.Fprintln(log, line)
				continue
			}
			var response struct {
				fanoutAPISnapshot
				URL string
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, fanoutProtocolPrefix)), &response); err != nil {
				fmt.Fprintln(log, "invalid worker control response")
				continue
			}
			response.fanoutAPISnapshot.URL = response.URL
			p.responses <- response.fanoutAPISnapshot
		}
		p.err = errors.Join(scanner.Err(), command.Wait())
		close(p.done)
	}()
	t.Cleanup(func() {
		if err := p.stop(); err != nil {
			t.Errorf("API child exit: %v; private log %s", err, log.Name())
		}
	})
	p.ready, err = p.call(ctx, fanoutAPICommand{Operation: "start", Config: &config})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (p *fanoutAPIProcess) call(ctx context.Context, command fanoutAPICommand) (fanoutAPISnapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := json.NewEncoder(p.input).Encode(command); err != nil {
		return fanoutAPISnapshot{}, fmt.Errorf("API control input: %w", err)
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	select {
	case snapshot, ok := <-p.responses:
		if !ok {
			<-p.done
			return snapshot, fmt.Errorf("API child exited: %v; private log %s", p.err, p.log.Name())
		}
		return snapshot, nil
	case <-ctx.Done():
		return fanoutAPISnapshot{}, ctx.Err()
	case <-deadline.C:
		return fanoutAPISnapshot{}, errors.New("API control deadline exceeded")
	}
}

func (p *fanoutAPIProcess) stop() error {
	// EOF requests graceful shutdown. Kill only our child if bounded shutdown
	// fails, and always retain its exit status rather than treating it as success.
	p.input.Close()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	select {
	case <-p.done:
	case <-deadline.C:
		_ = p.command.Process.Kill()
		<-p.done
	}
	return errors.Join(p.err, p.log.Close())
}

// Invoked only as an owned child of TestPerformanceHotTopicsFanout. Secrets
// Connection/proxy keys arrive through the local pipe; the database URL stays
// in the private fixture environment, never command arguments or reports.
func TestPerformanceAPIWorker(t *testing.T) {
	if os.Getenv("ODYSSEIA_TEST_API_WORKER") != "1" {
		t.Skip("owned API subprocess helper")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := json.NewDecoder(os.Stdin)
	var start fanoutAPICommand
	if err := input.Decode(&start); err != nil || start.Operation != "start" || start.Config == nil {
		t.Fatal("API worker requires its initial private configuration")
	}
	config := start.Config
	cfg, err := parseConfig(fanoutDatabaseURL(t, "ODYSSEIA_TEST_REALTIME_RUNTIME_URL"))
	if err != nil {
		t.Fatal(err)
	}
	timings := &mixedAPITimings{}
	timings.reset()
	cfg.ConnConfig.RuntimeParams["application_name"] = "odysseia_performance_api"
	cfg.ConnConfig.Tracer = timings
	cfg.MaxConns = 16
	if config.ReadConnections != 0 {
		t.Fatal("split pools were not adopted; comparisons require the archived candidate API binary")
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store := newCachedForumFixture(t, pool)
	writerLimit := store.writes.limit
	if raw := os.Getenv("ODYSSEIA_TEST_WRITE_LIMIT"); raw != "" && raw != "0" {
		writerLimit, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || writerLimit < 1 || writerLimit > 16 {
			t.Fatal("invalid experimental writer limit")
		}
		store.writes = newWriteAdmission(writerLimit, forumWriteQueueLimit)
	}
	verifier, err := tokenauth.New(ctx, config.Auth)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := realtime.NewSigner(config.ConnectionKey, realtime.TokenTTL)
	if err != nil {
		t.Fatal(err)
	}
	api := httpapi.NewAPI(verifier.Authenticate, NewUsers(pool).Find)
	api.RegisterForums(store)
	api.RegisterChannels(store)
	if err := api.RegisterRealtime(signer, store, config.ProxyKey); err != nil {
		t.Fatal(err)
	}
	server := newRealtimeFixtureServer(t, pool.Ping, timings.wrap(api))
	// Explicitly close the server before the deferred pool close.
	defer server.Close()
	var profile *os.File
	defer func() { stopFanoutProfile(t, &profile) }()
	for command := start; ; {
		switch command.Operation {
		case "start":
		case "reset":
			timings.reset()
			if config.ProfileDir != "" && strings.HasPrefix(command.Phase, "measured-") {
				profile, err = os.OpenFile(filepath.Join(config.ProfileDir, command.Phase+".cpu.pprof"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if err := pprof.StartCPUProfile(profile); err != nil {
					t.Fatal(err)
				}
			}
		case "take":
			stopFanoutProfile(t, &profile)
		case "stats":
		default:
			t.Fatal("unknown worker control operation")
		}
		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)
		stat := pool.Stat()
		snapshot := fanoutAPISnapshot{
			PID: os.Getpid(), URL: server.URL, Acquired: stat.AcquiredConns(), PoolMax: stat.MaxConns(),
			PoolWaitNS: stat.EmptyAcquireWaitTime().Nanoseconds(), Goroutines: runtime.NumGoroutine(),
			HeapBytes: memory.HeapAlloc, Allocated: memory.TotalAlloc, GCCycles: memory.NumGC, GCPauseNS: memory.PauseTotalNs,
			PageCache:    fanoutCacheStats{store.pages.hits.Load(), store.pages.misses.Load(), store.pages.loads.Load()},
			Measurements: api.Measurements(),
		}
		snapshot.PoolMode = fmt.Sprintf("shared16_writers%d", writerLimit)
		snapshot.ReadPoolMax, snapshot.ReadAcquired = stat.MaxConns(), stat.AcquiredConns()
		if command.Operation == "take" {
			snapshot.Timings = timings.take()
		}
		// URL is excluded from saved snapshots but is needed once at startup.
		response := struct {
			fanoutAPISnapshot
			URL string
		}{snapshot, server.URL}
		encoded, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println(fanoutProtocolPrefix + string(encoded))
		err = input.Decode(&command)
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatal("invalid API worker control input")
		}
	}
}

func stopFanoutProfile(t *testing.T, profile **os.File) {
	t.Helper()
	if *profile == nil {
		return
	}
	pprof.StopCPUProfile()
	if err := (*profile).Close(); err != nil {
		t.Fatal(err)
	}
	*profile = nil
}

func fanoutHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	ca, err := os.ReadFile(filepath.Join(os.Getenv("ODYSSEIA_TEST_REALTIME_TLS_DIR"), "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("invalid API fixture CA")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}, ForceAttemptHTTP2: true}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport}
}
