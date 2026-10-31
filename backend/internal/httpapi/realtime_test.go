package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/realtime"
)

type subscriptionBackend struct {
	ForumBackend
	actor string
	err   error
}

func (s *subscriptionBackend) GetForum(_ context.Context, actor, id string) (forum.Forum, error) {
	s.actor = actor
	return forum.Forum{ID: id}, s.err
}

func (s *subscriptionBackend) GetTopic(_ context.Context, actor, id string) (forum.Topic, error) {
	s.actor = actor
	return forum.Topic{ID: id}, s.err
}

func (s *subscriptionBackend) GetChannel(_ context.Context, actor, id string) (forum.Channel, error) {
	s.actor = actor
	return forum.Channel{ID: id}, s.err
}

func TestSubscribeProxyTrustsOnlyItsOwnCredentialAndCurrentPermissions(t *testing.T) {
	const actor = "60000000-0000-4000-8000-000000000001"
	const channel = "topic:70000000-0000-4000-8000-000000000001"
	key := strings.Repeat("p", 32)
	tests := []struct {
		name    string
		keys    []string
		body    map[string]any
		failure error
		status  int
		allowed bool
		checked bool
	}{
		{name: "external bearer cannot impersonate provider", status: 401},
		{name: "duplicate keys rejected", keys: []string{key, key}, status: 401},
		{name: "current member", keys: []string{key}, status: 200, allowed: true, checked: true},
		{name: "removed member", keys: []string{key}, failure: forum.ErrNotFound, status: 200, checked: true},
		{name: "dependency failure is closed", keys: []string{key}, failure: errors.New("synthetic dependency failure"), status: 503, checked: true},
		{name: "client data cannot select actor", keys: []string{key}, body: map[string]any{"user": actor, "channel": channel, "data": map[string]any{"user": "other", "allow": true}}, status: 200, allowed: true, checked: true},
		{name: "noncanonical channel", keys: []string{key}, body: map[string]any{"user": actor, "channel": "topic:invalid"}, status: 200},
		{name: "token bypass rejected", keys: []string{key}, body: map[string]any{"user": actor, "channel": channel, "token": "legacy"}, status: 200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &subscriptionBackend{err: tc.failure}
			signer, err := realtime.NewSigner(strings.Repeat("c", 32), realtime.TokenTTL)
			if err != nil {
				t.Fatal(err)
			}
			api := NewAPI(nil, nil)
			if err := api.RegisterRealtime(signer, store, key); err != nil {
				t.Fatal(err)
			}
			body := tc.body
			if body == nil {
				body = map[string]any{"user": actor, "channel": channel}
			}
			data, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("POST", "/_internal/realtime/subscribe", strings.NewReader(string(data)))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer untrusted-user-token")
			for _, value := range tc.keys {
				r.Header.Add("X-Odysseia-Proxy-Key", value)
			}
			w := httptest.NewRecorder()
			api.ServeHTTP(w, r)
			if w.Code != tc.status || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d want=%d with no-store", w.Code, tc.status)
			}
			if (store.actor != "") != tc.checked || (tc.checked && store.actor != actor) {
				t.Fatal("authorization used untrusted identity or ran before service authentication")
			}
			var reply map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
				t.Fatal(err)
			}
			if (reply["result"] != nil) != tc.allowed {
				t.Fatal("unexpected proxy authorization result")
			}
			if w.Code == 200 && !tc.allowed {
				var denied struct {
					Code int `json:"code"`
				}
				if err := json.Unmarshal(reply["error"], &denied); err != nil || denied.Code != 103 {
					t.Fatal("denial must use provider permission code")
				}
			}
		})
	}
}
