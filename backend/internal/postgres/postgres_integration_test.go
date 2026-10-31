//go:build integration

package postgres

import (
	"context"
	"crypto/x509"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func fixtureURL(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is required for integration tests", name)
	}
	u, err := url.Parse(value)
	if err != nil || u.Path != "/odysseia_test" {
		t.Fatal("integration tests require the isolated odysseia_test database")
	}
	return value
}

func TestPostgresVerifiedTLSAndRejections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	connectionString := fixtureURL(t, "ODYSSEIA_TEST_DATABASE_URL")
	pool, err := Open(ctx, connectionString)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var protocol string
	err = pool.QueryRow(ctx, "SELECT version FROM pg_stat_ssl WHERE pid = pg_backend_pid()").Scan(&protocol)
	if err != nil {
		t.Fatal(err)
	}
	if protocol != "TLSv1.3" {
		t.Fatalf("database protocol = %q, want TLSv1.3", protocol)
	}

	t.Run("wrong hostname", func(t *testing.T) {
		u, _ := url.Parse(connectionString)
		u.Host = "127.0.0.1:" + u.Port()
		connection, err := Open(ctx, u.String())
		if connection != nil {
			connection.Close()
		}
		var hostnameError x509.HostnameError
		if !errors.As(err, &hostnameError) {
			t.Fatalf("want hostname verification failure, got %v", err)
		}
	})

	t.Run("untrusted CA", func(t *testing.T) {
		u, _ := url.Parse(connectionString)
		query := u.Query()
		query.Set("sslrootcert", os.Getenv("ODYSSEIA_TEST_UNTRUSTED_CA"))
		u.RawQuery = query.Encode()
		connection, err := Open(ctx, u.String())
		if connection != nil {
			connection.Close()
		}
		var authorityError x509.UnknownAuthorityError
		if !errors.As(err, &authorityError) {
			t.Fatalf("want CA verification failure, got %v", err)
		}
	})
	t.Run("server rejects plaintext", func(t *testing.T) {
		u, _ := url.Parse(connectionString)
		query := u.Query()
		query.Set("sslmode", "disable")
		u.RawQuery = query.Encode()
		connection, err := pgx.Connect(ctx, u.String())
		if connection != nil {
			connection.Close(ctx)
		}
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != "28000" {
			t.Fatalf("want server-side plaintext rejection, got %v", err)
		}
	})
}

func TestMigrationsAreExplicitAndRepeatable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	migrationURL := fixtureURL(t, "ODYSSEIA_TEST_MIGRATION_DATABASE_URL")
	appURL := fixtureURL(t, "ODYSSEIA_TEST_DATABASE_URL")
	pool, err := Open(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var clean bool
	err = pool.QueryRow(ctx, "SELECT to_regnamespace('odysseia') IS NULL AND to_regclass('public.goose_db_version') IS NULL").Scan(&clean)
	if err != nil {
		t.Fatal(err)
	}
	if !clean {
		t.Fatal("fixture must be fresh; opening the runtime pool must not create schema")
	}

	type migrationResult struct {
		applied int
		err     error
	}
	results := make(chan migrationResult, 2)
	for range 2 {
		go func() {
			count, err := MigrateUp(ctx, migrationURL)
			results <- migrationResult{applied: count, err: err}
		}()
	}
	applied := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		applied += result.applied
	}
	if applied != 11 {
		t.Fatalf("concurrent migrations applied = %d, want 11", applied)
	}
	applied, err = MigrateUp(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	if applied != 0 {
		t.Fatalf("second migration applied = %d, want 0", applied)
	}
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regnamespace('odysseia') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("explicit migration did not create the application schema")
	}
	_, err = pool.Exec(ctx, "CREATE SCHEMA runtime_must_not_create")
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != "42501" {
		t.Fatalf("runtime identity must not create schema, got %v", err)
	}
	t.Run("local identity mapping", func(t *testing.T) {
		testUsers(t, ctx, migrationURL, pool)
	})
	t.Run("independent forum", func(t *testing.T) {
		testForums(t, ctx, migrationURL, pool)
	})
	t.Run("collaboration surface", func(t *testing.T) {
		testCollaboration(t, ctx, migrationURL, pool)
	})
	t.Run("forum role ceiling", func(t *testing.T) {
		testRoleCeiling(t, ctx, migrationURL, pool)
	})
	t.Run("text conversations", func(t *testing.T) {
		testConversations(t, ctx, migrationURL, pool)
	})
	t.Run("conversation attachments", func(t *testing.T) {
		testAttachments(t, ctx, migrationURL, pool)
	})
	t.Run("discussion modes and fixed channels", func(t *testing.T) {
		owner, err := Open(ctx, migrationURL)
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Close()
		testDiscussionModesAndChannels(t, ctx, owner, pool)
	})
}
