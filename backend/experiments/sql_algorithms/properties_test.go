//go:build integration && algorithms

// Package sqlalgorithms contains isolated research probes, not business adapters.
package sqlalgorithms

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"odysseia.local/backend/internal/postgres"
)

func TestAlgorithmProperties(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	dsn := os.Getenv("ODYSSEIA_TEST_MIGRATION_DATABASE_URL")
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Hostname() != "localhost" || parsed.Path != "/odysseia_test" {
		t.Fatal("algorithm probes require the isolated loopback test database")
	}
	pool, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	schema := "algorithm_probe_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		// This generated schema was created above, in the checked synthetic DB.
		if _, err := pool.Exec(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("remove owned probe schema: %v", err)
		}
	})
	table := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
	t.Run("sequence allocation is not commit order", func(t *testing.T) {
		name := table("sequence_rows")
		if _, err := pool.Exec(ctx, "CREATE TABLE "+name+" (id bigserial PRIMARY KEY)"); err != nil {
			t.Fatal(err)
		}
		first, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer first.Rollback(ctx)
		var earlier, later int64
		if err := first.QueryRow(ctx, "INSERT INTO "+name+" DEFAULT VALUES RETURNING id").Scan(&earlier); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, "INSERT INTO "+name+" DEFAULT VALUES RETURNING id").Scan(&later); err != nil {
			t.Fatal(err)
		}
		var observed int64
		if err := pool.QueryRow(ctx, "SELECT max(id) FROM "+name).Scan(&observed); err != nil {
			t.Fatal(err)
		}
		if earlier != 1 || later != 2 || observed != 2 {
			t.Fatal("probe did not expose the later ID before the earlier transaction")
		}
		if err := first.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		var afterCursor, total int
		if err := pool.QueryRow(ctx, "SELECT count(*) FILTER(WHERE id>$1),count(*) FROM "+name, later).Scan(&afterCursor, &total); err != nil {
			t.Fatal(err)
		}
		if afterCursor != 0 || total != 2 {
			t.Fatal("unexpected sequence cursor result")
		}
		t.Log("a cursor advanced to 2 would miss the subsequently committed row 1")
	})
	t.Run("transactional counter waits and rolls back", func(t *testing.T) {
		name := table("counter")
		if _, err := pool.Exec(ctx, "CREATE TABLE "+name+" (id integer PRIMARY KEY,position bigint NOT NULL); INSERT INTO "+name+" VALUES(1,0)"); err != nil {
			t.Fatal(err)
		}
		first, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer first.Rollback(ctx)
		second, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer second.Rollback(ctx)
		query := "UPDATE " + name + " SET position=position+1 WHERE id=1 RETURNING position"
		var allocated int64
		if err := first.QueryRow(ctx, query).Scan(&allocated); err != nil || allocated != 1 {
			t.Fatal("first allocation failed")
		}
		result := queryAsync(ctx, second, query)
		waitForLock(t, ctx, pool, second.Conn().PgConn().PID())
		if err := first.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		got := <-result
		if got.err != nil || got.value != 2 {
			t.Fatal("second writer did not follow the first commit")
		}
		if err := second.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, query).Scan(&allocated); err != nil || allocated != 2 {
			t.Fatal("rolled-back counter allocation was not reclaimed")
		}
		t.Log("the second writer waited for the row lock; rolling it back preserved the next sequence")
	})
	t.Run("insert or select CTE can miss the concurrent winner", func(t *testing.T) {
		name := table("requests")
		if _, err := pool.Exec(ctx, "CREATE TABLE "+name+" (request_key text PRIMARY KEY,id bigint NOT NULL)"); err != nil {
			t.Fatal(err)
		}
		winner, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer winner.Rollback(ctx)
		if _, err := winner.Exec(ctx, "INSERT INTO "+name+" VALUES('same',1)"); err != nil {
			t.Fatal(err)
		}
		follower, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			t.Fatal(err)
		}
		defer follower.Rollback(ctx)
		query := "WITH inserted AS (INSERT INTO " + name + " VALUES('same',1) ON CONFLICT(request_key) DO NOTHING RETURNING id) SELECT id FROM inserted UNION ALL SELECT id FROM " + name + " WHERE request_key='same' LIMIT 1"
		result := queryAsync(ctx, follower, query)
		waitForLock(t, ctx, pool, follower.Conn().PgConn().PID())
		if err := winner.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		got := <-result
		if !errors.Is(got.err, pgx.ErrNoRows) {
			t.Fatal("the old statement snapshot unexpectedly returned the concurrent winner")
		}
		var fresh int64
		if err := follower.QueryRow(ctx, "SELECT id FROM "+name+" WHERE request_key='same'").Scan(&fresh); err != nil || fresh != 1 {
			t.Fatal("a new READ COMMITTED statement did not see the winner")
		}
		t.Log("conflict detection saw the winner, but the CTE's original snapshot did not; a separate read saw it")
	})
	t.Run("pipelined lock then retry read gets a fresh snapshot", func(t *testing.T) {
		name := table("pipelined_retry")
		if _, err := pool.Exec(ctx, "CREATE TABLE "+name+" (id integer PRIMARY KEY,value bigint NOT NULL); INSERT INTO "+name+" VALUES(1,0)"); err != nil {
			t.Fatal(err)
		}
		winner, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer winner.Rollback(ctx)
		if _, err := winner.Exec(ctx, "SELECT value FROM "+name+" WHERE id=1 FOR UPDATE"); err != nil {
			t.Fatal(err)
		}
		follower, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			t.Fatal(err)
		}
		defer follower.Rollback(ctx)
		result := make(chan queryResult, 1)
		go func() {
			var lock, retry int64
			batch := &pgx.Batch{}
			batch.Queue("SELECT value FROM " + name + " WHERE id=1 FOR UPDATE").QueryRow(func(row pgx.Row) error { return row.Scan(&lock) })
			batch.Queue("SELECT value FROM " + name + " WHERE id=2").QueryRow(func(row pgx.Row) error { return row.Scan(&retry) })
			err := follower.SendBatch(ctx, batch).Close()
			result <- queryResult{retry, err}
		}()
		waitForLock(t, ctx, pool, follower.Conn().PgConn().PID())
		if _, err := winner.Exec(ctx, "INSERT INTO "+name+" VALUES(2,42)"); err != nil {
			t.Fatal(err)
		}
		if err := winner.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		got := <-result
		if got.err != nil || got.value != 42 {
			t.Fatalf("pipelined retry read missed the commit after lock wait: %v", got.err)
		}
		t.Log("two statements in one pgx batch preserve the post-lock READ COMMITTED snapshot")
	})
	t.Run("driver batch retains atomic rollback", func(t *testing.T) {
		name := table("batch_rows")
		if _, err := pool.Exec(ctx, "CREATE TABLE "+name+" (id integer PRIMARY KEY,value integer CHECK(value>=0)); INSERT INTO "+name+" VALUES(1,0)"); err != nil {
			t.Fatal(err)
		}
		batch := &pgx.Batch{}
		batch.Queue("UPDATE " + name + " SET value=1 WHERE id=1")
		batch.Queue("INSERT INTO " + name + " VALUES(2,-1)")
		err := pool.SendBatch(ctx, batch).Close()
		var rejected *pgconn.PgError
		if !errors.As(err, &rejected) || rejected.Code != "23514" {
			t.Fatal("batch did not encounter the late constraint failure")
		}
		var stored int
		if err := pool.QueryRow(ctx, "SELECT value FROM "+name+" WHERE id=1").Scan(&stored); err != nil || stored != 0 {
			t.Fatal("failed batch left a partial write")
		}
		t.Log("the late constraint failure rolled back the earlier command in the implicit transaction")
	})
}

type queryResult struct {
	value int64
	err   error
}

func queryAsync(ctx context.Context, tx pgx.Tx, query string) <-chan queryResult {
	result := make(chan queryResult, 1)
	go func() {
		var value int64
		err := tx.QueryRow(ctx, query).Scan(&value)
		result <- queryResult{value, err}
	}()
	return result
}

// Observe the actual database lock wait before committing the other transaction;
// sleeps would make the snapshot probe depend on scheduler timing.
func waitForLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid uint32) {
	t.Helper()
	timeout, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := pool.QueryRow(timeout, `SELECT EXISTS(SELECT FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')`, pid).Scan(&blocked); err != nil {
			t.Fatal("cannot observe the probe's database wait")
		}
		if blocked {
			return
		}
		select {
		case <-ticker.C:
		case <-timeout.Done():
			t.Fatal("probe never reached a real lock wait")
		}
	}
}
