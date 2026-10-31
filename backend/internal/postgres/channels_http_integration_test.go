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
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/httpapi"
	"odysseia.local/backend/internal/identity"
)

func testChannelHTTP(t *testing.T, ctx context.Context, pool *pgxpool.Pool, store *Forums, alice, outsider identity.User, board forum.Forum, channel forum.Channel, topicID string) {
	t.Helper()
	api := httpapi.NewAPI(func(_ context.Context, token string) (identity.Authentication, error) {
		if token != "alice" && token != "outsider" {
			return identity.Authentication{}, errors.New("invalid fixture token")
		}
		return identity.Authentication{ExternalID: identity.ExternalID{Issuer: "https://p3-fixture.example", Subject: token}, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}, NewUsers(pool).Find)
	api.RegisterForums(store)
	api.RegisterChannels(store)
	server := httptest.NewUnstartedServer(nil)
	server.Config = httpapi.NewServer("127.0.0.1:0", pool.Ping, api)
	server.TLS = server.Config.TLSConfig.Clone()
	server.StartTLS()
	t.Cleanup(server.Close)
	type response struct {
		status int
		data   []byte
		header http.Header
	}
	call := func(actor, method, path, key string, body any) response {
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
		r.Header.Set("Authorization", "Bearer "+actor)
		r.Header.Set("X-User-ID", outsider.ID)
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		res, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, err = io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response{res.StatusCode, data, res.Header}
	}
	contract := call("alice", "GET", "/openapi.json", "", nil)
	doc, err := openapi3.NewLoader().LoadFromData(contract.data)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	check := func(got response, path, method string, status int) {
		t.Helper()
		if got.status != status {
			t.Fatalf("%s %s got %d want %d: %s", method, path, got.status, status, got.data)
		}
		operation := doc.Paths.Value(path).GetOperation(method)
		definition := operation.Responses.Value(strconv.Itoa(status)).Value
		var value any
		if err := json.Unmarshal(got.data, &value); err != nil {
			t.Fatal(err)
		}
		media := definition.Content[got.header.Get("Content-Type")]
		if media == nil {
			t.Fatal("undeclared channel response content type")
		}
		if err := media.Schema.Value.VisitJSON(value); err != nil {
			t.Fatal(err)
		}
		if got.header.Get("Cache-Control") != "no-store" {
			t.Fatal("private channel response is cacheable")
		}
	}
	collection := "/v1/forums/" + board.ID + "/channels"
	item := "/v1/channels/" + channel.ID
	check(call("alice", "GET", item, "", nil), "/v1/channels/{channel_id}", "GET", 200)
	check(call("outsider", "GET", item, "", nil), "/v1/channels/{channel_id}", "GET", 404)
	check(call("outsider", "POST", collection, uuid.NewString(), forum.ChannelDraft{Title: "forbidden"}), "/v1/forums/{forum_id}/channels", "POST", 404)
	check(call("alice", "POST", collection, "", forum.ChannelDraft{Title: "missing key"}), "/v1/forums/{forum_id}/channels", "POST", 400)
	check(call("alice", "POST", collection, uuid.NewString(), map[string]string{"title": "forged", "author_id": outsider.ID}), "/v1/forums/{forum_id}/channels", "POST", 400)
	key := uuid.NewString()
	created := call("alice", "POST", collection, key, forum.ChannelDraft{Title: "HTTPS fixed channel"})
	check(created, "/v1/forums/{forum_id}/channels", "POST", 201)
	var resource forum.Channel
	if err := json.Unmarshal(created.data, &resource); err != nil {
		t.Fatal(err)
	}
	if resource.AuthorID != alice.ID || created.header.Get("Location") != "/v1/channels/"+resource.ID {
		t.Fatal("channel used a forged actor or incorrect location")
	}
	repeated := call("alice", "POST", collection, key, forum.ChannelDraft{Title: "HTTPS fixed channel"})
	check(repeated, "/v1/forums/{forum_id}/channels", "POST", 201)
	if !bytes.Equal(created.data, repeated.data) {
		t.Fatal("channel replay changed canonical response")
	}
	check(call("alice", "POST", collection, key, forum.ChannelDraft{Title: "changed"}), "/v1/forums/{forum_id}/channels", "POST", 409)
	listed := call("alice", "GET", collection+"?limit=1", "", nil)
	check(listed, "/v1/forums/{forum_id}/channels", "GET", 200)
	var channels struct {
		Items []forum.Channel `json:"items"`
		Next  string          `json:"next"`
	}
	if err := json.Unmarshal(listed.data, &channels); err != nil || len(channels.Items) != 1 || channels.Next == "" {
		t.Fatalf("channel cursor missing: %v", err)
	}
	check(call("alice", "GET", collection+"?after="+url.QueryEscape(channels.Next), "", nil), "/v1/forums/{forum_id}/channels", "GET", 200)
	check(call("alice", "GET", "/v1/forums/"+board.ID+"/topics?after="+url.QueryEscape(channels.Next), "", nil), "/v1/forums/{forum_id}/topics", "GET", 400)
	check(call("alice", "POST", item+"/messages", uuid.NewString(), map[string]string{"body": "no layout field", "root_id": uuid.NewString()}), "/v1/channels/{channel_id}/messages", "POST", 400)
	check(call("alice", "POST", item+"/messages", uuid.NewString(), map[string]string{"body": "invalid quote", "quote_id": "invalid"}), "/v1/channels/{channel_id}/messages", "POST", 400)
	check(call("alice", "POST", item+"/messages", uuid.NewString(), forum.ChannelMessageDraft{Body: "missing quote", QuoteID: uuid.NewString()}), "/v1/channels/{channel_id}/messages", "POST", 422)
	check(call("alice", "POST", item+"/messages", uuid.NewString(), forum.ChannelMessageDraft{Body: "canonical HTTPS channel message"}), "/v1/channels/{channel_id}/messages", "POST", 201)
	history := call("alice", "GET", item+"/messages?limit=1", "", nil)
	check(history, "/v1/channels/{channel_id}/messages", "GET", 200)
	var messages struct {
		Next string `json:"next"`
	}
	if err := json.Unmarshal(history.data, &messages); err != nil || messages.Next == "" {
		t.Fatal("channel message cursor missing")
	}
	check(call("alice", "GET", "/v1/topics/"+topicID+"/messages?after="+url.QueryEscape(messages.Next), "", nil), "/v1/topics/{topic_id}/messages", "GET", 400)
	check(call("outsider", "GET", item+"/messages", "", nil), "/v1/channels/{channel_id}/messages", "GET", 404)
	check(call("alice", "POST", "/v1/forums/"+board.ID+"/topics", uuid.NewString(), forum.TopicDraft{Title: "HTTPS instant topic", Body: "same message API", Mode: "chat"}), "/v1/forums/{forum_id}/topics", "POST", 201)
	measured := api.Measurements()
	for _, stage := range []string{"identity_lookup", "access_check", "pool_acquire", "transaction_commit"} {
		if measured.Stages[stage].Count == 0 {
			t.Fatalf("unmeasured channel stage: %s", stage)
		}
	}
}
