//go:build integration

package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/httpapi"
	"odysseia.local/backend/internal/identity"
)

func testForumHTTP(t *testing.T, ctx context.Context, runtimePool *pgxpool.Pool, alice, bob identity.User, board, private forum.Forum, originalTopicID string) {
	t.Helper()
	api := httpapi.NewAPI(func(_ context.Context, token string) (identity.Authentication, error) {
		if token != "alice" {
			return identity.Authentication{}, errors.New("invalid synthetic bearer")
		}
		return identity.Authentication{ExternalID: identity.ExternalID{Issuer: "https://forum-fixture.example", Subject: "alice"}, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}, NewUsers(runtimePool).Find)
	api.RegisterForums(newCachedForumFixture(t, runtimePool))
	server := httptest.NewUnstartedServer(nil)
	server.Config = httpapi.NewServer("127.0.0.1:0", runtimePool.Ping, api)
	server.TLS = server.Config.TLSConfig.Clone()
	server.StartTLS()
	defer server.Close()
	type response struct {
		status int
		body   []byte
		header http.Header
	}
	request := func(method, path, key string, body any) response {
		t.Helper()
		var data []byte
		if body != nil {
			var err error
			data, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		r, err := http.NewRequestWithContext(ctx, method, server.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer alice")
		r.Header.Set("X-User-ID", bob.ID)
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		result, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer result.Body.Close()
		content, err := io.ReadAll(result.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response{result.StatusCode, content, result.Header}
	}
	contract := request("GET", "/openapi.json", "", nil)
	doc, err := openapi3.NewLoader().LoadFromData(contract.body)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	check := func(got response, path, method string, status int) {
		t.Helper()
		if got.status != status {
			t.Fatalf("%s %s status=%d want=%d body=%s", method, path, got.status, status, got.body)
		}
		operation := doc.Paths.Value(path).GetOperation(method)
		definition := operation.Responses.Value(strconv.Itoa(status)).Value
		media := definition.Content[got.header.Get("Content-Type")]
		if media == nil {
			t.Fatalf("response content type %q is absent from contract", got.header.Get("Content-Type"))
		}
		var value any
		if err := json.Unmarshal(got.body, &value); err != nil {
			t.Fatal(err)
		}
		if err := media.Schema.Value.VisitJSON(value); err != nil {
			t.Fatalf("response violates OpenAPI: %v", err)
		}
		if got.header.Get("Cache-Control") != "no-store" {
			t.Fatal("private forum response can be cached")
		}
	}
	check(request("GET", "/v1/forums/"+board.ID, "", nil), "/v1/forums/{forum_id}", "GET", 200)
	check(request("GET", "/v1/forums/"+private.ID, "", nil), "/v1/forums/{forum_id}", "GET", 404)
	check(request("POST", "/v1/forums/"+board.ID+"/topics", uuid.NewString(), map[string]string{"title": "forged", "body": "body", "mode": "threaded", "author_id": bob.ID}), "/v1/forums/{forum_id}/topics", "POST", 400)
	draft := forum.TopicDraft{Title: "HTTPS 普通论坛", Body: "opening from HTTPS", Mode: "threaded"}
	created := request("POST", "/v1/forums/"+board.ID+"/topics", uuid.NewString(), draft)
	check(created, "/v1/forums/{forum_id}/topics", "POST", 201)
	var topic forum.CreatedTopic
	if err := json.Unmarshal(created.body, &topic); err != nil {
		t.Fatal(err)
	}
	if topic.Topic.AuthorID != alice.ID || topic.OpeningMessage.AuthorID != alice.ID {
		t.Fatal("forged header selected the author")
	}
	if created.header.Get("Location") != "/v1/topics/"+topic.Topic.ID {
		t.Fatal("topic location is not its readable resource")
	}
	check(request("GET", "/v1/topics/"+topic.Topic.ID, "", nil), "/v1/topics/{topic_id}", "GET", 200)
	path := "/v1/topics/" + topic.Topic.ID + "/messages"
	key := uuid.NewString()
	reply := forum.ReplyDraft{Body: "first child", RootID: topic.OpeningMessage.ID, QuoteID: topic.OpeningMessage.ID}
	first := request("POST", path, key, reply)
	check(first, "/v1/topics/{topic_id}/messages", "POST", 201)
	duplicate := request("POST", path, key, reply)
	check(duplicate, "/v1/topics/{topic_id}/messages", "POST", 201)
	if !bytes.Equal(first.body, duplicate.body) {
		t.Fatal("HTTP retry changed the persisted reply")
	}
	var child forum.Entry
	if err := json.Unmarshal(first.body, &child); err != nil {
		t.Fatal(err)
	}
	reply.Body = "quote the previous child"
	reply.QuoteID = child.ID
	check(request("POST", path, uuid.NewString(), reply), "/v1/topics/{topic_id}/messages", "POST", 201)
	check(request("POST", path, key, reply), "/v1/topics/{topic_id}/messages", "POST", 409)
	check(request("POST", path, "", reply), "/v1/topics/{topic_id}/messages", "POST", 400)
	check(request("POST", path, uuid.NewString(), forum.ReplyDraft{Body: "third level", RootID: child.ID}), "/v1/topics/{topic_id}/messages", "POST", 422)
	check(request("POST", path, uuid.NewString(), forum.ReplyDraft{Body: "cross-topic reference", QuoteID: originalTopicID}), "/v1/topics/{topic_id}/messages", "POST", 422)
	check(request("POST", path, uuid.NewString(), forum.ReplyDraft{Body: strings.Repeat("x", 256*1024)}), "/v1/topics/{topic_id}/messages", "POST", 413)
	pageOne := request("GET", path+"?limit=2", "", nil)
	check(pageOne, "/v1/topics/{topic_id}/messages", "GET", 200)
	var page struct {
		Items []forum.Entry `json:"items"`
		Next  string        `json:"next"`
	}
	if err := json.Unmarshal(pageOne.body, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Next == "" {
		t.Fatal("first message page lacks a continuation")
	}
	ids := map[string]bool{page.Items[0].ID: true, page.Items[1].ID: true}
	check(request("POST", path, uuid.NewString(), forum.ReplyDraft{Body: "inserted between pages"}), "/v1/topics/{topic_id}/messages", "POST", 201)
	pageTwo := request("GET", path+"?limit=2&after="+url.QueryEscape(page.Next), "", nil)
	check(pageTwo, "/v1/topics/{topic_id}/messages", "GET", 200)
	check(request("GET", "/v1/topics/"+originalTopicID+"/messages?after="+url.QueryEscape(page.Next), "", nil), "/v1/topics/{topic_id}/messages", "GET", 400)
	if err := json.Unmarshal(pageTwo.body, &page); err != nil {
		t.Fatal(err)
	}
	for _, entry := range page.Items {
		if ids[entry.ID] {
			t.Fatal("page insertion repeated a previous message")
		}
		ids[entry.ID] = true
	}
	if len(ids) != 4 {
		t.Fatal("page insertion lost a committed message")
	}
	check(request("GET", path+"?limit=101", "", nil), "/v1/topics/{topic_id}/messages", "GET", 400)
	topics := request("GET", "/v1/forums/"+board.ID+"/topics?limit=1", "", nil)
	check(topics, "/v1/forums/{forum_id}/topics", "GET", 200)
	var topicPage struct {
		Items []forum.Topic `json:"items"`
		Next  string        `json:"next"`
	}
	if err := json.Unmarshal(topics.body, &topicPage); err != nil {
		t.Fatal(err)
	}
	if len(topicPage.Items) != 1 || topicPage.Next == "" {
		t.Fatal("forum topic page lacks a continuation")
	}
	check(request("GET", "/v1/forums/"+board.ID+"/topics?after="+url.QueryEscape(topicPage.Next), "", nil), "/v1/forums/{forum_id}/topics", "GET", 200)
}
