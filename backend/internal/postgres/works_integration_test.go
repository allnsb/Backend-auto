//go:build integration

package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"odysseia.local/backend/internal/httpapi"
	"odysseia.local/backend/internal/identity"
	"odysseia.local/backend/internal/work"
)

func testWorks(t *testing.T, ctx context.Context, migrationURL string, pool *pgxpool.Pool) {
	owner, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	_, err = owner.Exec(ctx, `GRANT SELECT,INSERT ON odysseia.works TO odysseia_runtime;
		GRANT UPDATE(title,introduction,revision,updated_at) ON odysseia.works TO odysseia_runtime;`)
	if err != nil {
		t.Fatal(err)
	}
	users := NewUsers(owner)
	alice, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://works-fixture.invalid", Subject: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := users.Provision(ctx, identity.ExternalID{Issuer: "https://works-fixture.invalid", Subject: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	budget, err := NewWriteBudget(pool, 0)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewWorks(pool, budget)
	if err != nil {
		t.Fatal(err)
	}
	otherBudget, err := NewWriteBudget(pool, 0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewWorks(pool, otherBudget)
	if err != nil {
		t.Fatal(err)
	}
	var outboxBefore int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM odysseia.realtime_outbox`).Scan(&outboxBefore); err != nil {
		t.Fatal(err)
	}
	draft := work.Draft{Title: "independent draft", Introduction: "author-private text"}
	key := uuid.NewString()
	type outcome struct {
		item  work.Work
		fresh bool
		err   error
	}
	created := make(chan outcome, 12)
	for index := range 12 {
		go func() {
			writer := store
			if index%2 == 1 {
				writer = other
			}
			item, fresh, err := writer.Create(ctx, alice.ID, key, draft)
			created <- outcome{item, fresh, err}
		}()
	}
	var original work.Work
	freshCount := 0
	for range 12 {
		got := <-created
		if got.err != nil {
			t.Fatal(got.err)
		}
		if original.ID != "" && got.item.ID != original.ID {
			t.Fatal("creation replay made another draft")
		}
		original = got.item
		if got.fresh {
			freshCount++
		}
	}
	if freshCount != 1 || original.Status != "draft" || original.Revision != 1 {
		t.Fatalf("fresh=%d draft=%+v", freshCount, original)
	}
	if _, _, err := store.Create(ctx, alice.ID, key, work.Draft{Title: "different"}); !errors.Is(err, work.ErrConflict) {
		t.Fatalf("creation conflict: %v", err)
	}
	// Even a catalog owner has no implicit access in this author-only slice.
	if _, err := owner.Exec(ctx, `INSERT INTO odysseia.user_groups(user_id,group_id) VALUES($1,$2)`, bob.ID, identity.OwnerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, bob.ID, original.ID); !errors.Is(err, work.ErrNotFound) {
		t.Fatalf("private draft leaked: %v", err)
	}
	if _, err := store.Update(ctx, bob.ID, original.ID, work.Update{Draft: draft, Revision: 1}); !errors.Is(err, work.ErrNotFound) {
		t.Fatalf("non-author edited draft: %v", err)
	}

	// Independent admission objects model separate API instances. Correctness
	// must come from SQL's atomic revision predicate, not one local scope lock.
	updates := make(chan outcome, 12)
	for index := range 12 {
		go func() {
			writer := store
			if index%2 == 1 {
				writer = other
			}
			item, err := writer.Update(ctx, alice.ID, original.ID, work.Update{Draft: work.Draft{Title: fmt.Sprintf("edit %d", index)}, Revision: 1})
			updates <- outcome{item: item, err: err}
		}()
	}
	var winner work.Work
	conflicts := 0
	for range 12 {
		got := <-updates
		if errors.Is(got.err, work.ErrRevision) {
			conflicts++
			continue
		}
		if got.err != nil {
			t.Fatal(got.err)
		}
		winner = got.item
	}
	if conflicts != 11 || winner.Revision != 2 {
		t.Fatalf("conflicts=%d winner revision=%d", conflicts, winner.Revision)
	}
	noChange, err := store.Update(ctx, alice.ID, winner.ID, work.Update{Draft: winner.Draft, Revision: winner.Revision})
	if err != nil || noChange.Revision != winner.Revision || !noChange.UpdatedAt.Equal(winner.UpdatedAt) {
		t.Fatalf("identical update changed state: %v", err)
	}
	replay, fresh, err := store.Create(ctx, alice.ID, key, draft)
	if err != nil || fresh || replay.Revision != 2 || replay.Title != winner.Title {
		t.Fatalf("creation replay after edit: %+v fresh=%v error=%v", replay, fresh, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE odysseia.works SET author_id=$1 WHERE id=$2`, bob.ID, winner.ID); err == nil {
		t.Fatal("runtime role can transfer draft ownership")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM odysseia.works WHERE id=$1`, winner.ID); err == nil {
		t.Fatal("runtime role can delete drafts")
	}
	var outboxAfter int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM odysseia.realtime_outbox`).Scan(&outboxAfter); err != nil {
		t.Fatal(err)
	}
	if outboxAfter != outboxBefore {
		t.Fatal("private drafts emitted forum or conversation events")
	}
	testWorkHTTP(t, ctx, store, alice, bob)
	testWorkSharedBudget(t, ctx, pool, alice.ID)
	source, err := migrations.ReadFile("migrations/00014_private_work_drafts.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, strings.Split(string(source), "-- +goose Down")[1]); err == nil {
		t.Fatal("migration rollback would drop existing drafts")
	}
}

func testWorkSharedBudget(t *testing.T, ctx context.Context, pool *pgxpool.Pool, actor string) {
	budget, err := NewWriteBudget(pool, 1)
	if err != nil {
		t.Fatal(err)
	}
	forums, err := NewCachedForumsWithBudget(pool, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer forums.Close()
	works, err := NewWorks(pool, budget)
	if err != nil {
		t.Fatal(err)
	}
	release, err := forums.writes.acquire(ctx, writeScope{streamID: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	wait, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	_, _, blocked := works.Create(wait, actor, uuid.NewString(), work.Draft{Title: "budget waits before SQL"})
	release()
	if !errors.Is(blocked, context.DeadlineExceeded) {
		t.Fatalf("work bypassed shared forum budget: %v", blocked)
	}
	if _, _, err := works.Create(ctx, actor, uuid.NewString(), work.Draft{Title: "budget released"}); err != nil {
		t.Fatal(err)
	}
}

func testWorkHTTP(t *testing.T, ctx context.Context, store *Works, alice, bob identity.User) {
	api := httpapi.NewAPI(func(_ context.Context, raw string) (identity.Authentication, error) {
		if raw != "alice" && raw != "bob" {
			return identity.Authentication{}, identity.ErrNotFound
		}
		return identity.Authentication{ExternalID: identity.ExternalID{Issuer: "https://works-fixture.invalid", Subject: raw}, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}, func(_ context.Context, external identity.ExternalID) (identity.User, error) {
		if external.Subject == "alice" {
			return alice, nil
		}
		return bob, nil
	})
	api.RegisterWorks(store)
	server := httptest.NewTLSServer(api)
	defer server.Close()
	contract, err := server.Client().Get(server.URL + "/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	defer contract.Body.Close()
	source, err := io.ReadAll(contract.Body)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := openapi3.NewLoader().LoadFromData(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	call := func(actor, method, path, key string, body any, want int, result any) {
		t.Helper()
		var raw []byte
		if body != nil {
			var err error
			raw, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		request, err := http.NewRequestWithContext(ctx, method, server.URL+path, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+actor)
		request.Header.Set("Content-Type", "application/json")
		if key != "" {
			request.Header.Set("Idempotency-Key", key)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != want || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s %s as %s status=%d want=%d: %s", method, path, actor, response.StatusCode, want, data)
		}
		mediaType := "application/json"
		if want >= 400 {
			mediaType = "application/problem+json"
		}
		if response.Header.Get("Content-Type") != mediaType {
			t.Fatalf("%s %s content type = %s", method, path, response.Header.Get("Content-Type"))
		}
		schema := "Problem"
		if want < 300 {
			schema = "Work"
			if strings.HasPrefix(path, "/v1/me/works") {
				schema = "WorkPage"
			}
		}
		var value any
		if err := json.Unmarshal(data, &value); err != nil {
			t.Fatal(err)
		}
		if err := doc.Components.Schemas[schema].Value.VisitJSON(value); err != nil {
			t.Fatalf("%s response violates contract: %v", schema, err)
		}
		if result != nil {
			if err := json.Unmarshal(data, result); err != nil {
				t.Fatal(err)
			}
		}
	}
	key := uuid.NewString()
	var found work.Work
	input := work.Draft{Title: "HTTP private draft", Introduction: "not a forum post"}
	call("alice", "POST", "/v1/works", key, input, 201, &found)
	call("alice", "POST", "/v1/works", key, input, 200, &found)
	call("alice", "GET", "/v1/works/"+found.ID, "", nil, 200, &found)
	call("bob", "GET", "/v1/works/"+found.ID, "", nil, 404, nil)
	call("bob", "GET", "/v1/works/"+uuid.NewString(), "", nil, 404, nil)
	call("alice", "POST", "/v1/works", uuid.NewString(), map[string]string{"title": "forged", "author_id": bob.ID}, 400, nil)
	call("alice", "PUT", "/v1/works/"+found.ID, "", map[string]any{"title": "numeric revision", "revision": 1}, 400, nil)
	call("alice", "PUT", "/v1/works/"+found.ID, "", work.Update{Draft: input, Revision: 0}, 422, nil)
	call("alice", "PUT", "/v1/works/"+found.ID, "", work.Update{Draft: work.Draft{Title: "HTTP update"}, Revision: 1}, 200, &found)
	call("alice", "PUT", "/v1/works/"+found.ID, "", work.Update{Draft: input, Revision: 1}, 409, nil)
	var first struct {
		Items []work.Work `json:"items"`
		Next  string      `json:"next"`
	}
	call("alice", "GET", "/v1/me/works?limit=1", "", nil, 200, &first)
	if len(first.Items) != 1 || first.Next == "" {
		t.Fatal("private seek pagination omitted cursor")
	}
	call("bob", "GET", "/v1/me/works?after="+url.QueryEscape(first.Next), "", nil, 400, nil)
	seen := map[string]bool{}
	path := "/v1/me/works?limit=1"
	for {
		var page struct {
			Items []work.Work `json:"items"`
			Next  string      `json:"next"`
		}
		call("alice", "GET", path, "", nil, 200, &page)
		for _, item := range page.Items {
			if item.AuthorID != alice.ID || seen[item.ID] {
				t.Fatal("list leaked an author or repeated a draft")
			}
			seen[item.ID] = true
		}
		if page.Next == "" {
			break
		}
		path = "/v1/me/works?limit=1&after=" + url.QueryEscape(page.Next)
	}
	var empty struct {
		Items []work.Work `json:"items"`
	}
	call("bob", "GET", "/v1/me/works", "", nil, 200, &empty)
	if len(empty.Items) != 0 {
		t.Fatal("non-author list exposed drafts")
	}
}
