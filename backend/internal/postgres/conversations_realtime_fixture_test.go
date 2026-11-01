//go:build integration && realtime && cluster

package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	centrifuge "github.com/centrifugal/centrifuge-go"
	"github.com/google/uuid"
	"odysseia.local/backend/internal/blob"
	"odysseia.local/backend/internal/emoji"
	"odysseia.local/backend/internal/httpapi"
	"odysseia.local/backend/internal/identity"
	"odysseia.local/backend/internal/media"
	"odysseia.local/backend/internal/message"
	"odysseia.local/backend/internal/mockidp"
	"odysseia.local/backend/internal/realtime"
	"odysseia.local/backend/internal/tokenauth"
)

type conversationRealtimeFixture struct {
	base   *clusterFixture
	server *httptest.Server
	users  map[string]identity.User
	tokens map[string]string
	blobs  *blob.Memory
}

// The issuer is synthetic, but HTTPS JWKS retrieval, access-token validation,
// automatic local accounts, runtime SQL grants, connection JWT and WSS are real.
// Object storage remains a named test double, never described as a real R2 test.
func newConversationRealtimeFixture(t *testing.T, ctx context.Context) *conversationRealtimeFixture {
	t.Helper()
	base := newClusterFixture(t, ctx)
	_, err := base.owner.Exec(ctx, `GRANT INSERT ON odysseia.users TO odysseia_runtime;
		GRANT SELECT ON odysseia.identity_groups, odysseia.user_groups TO odysseia_runtime;
		GRANT SELECT, INSERT ON odysseia.conversations, odysseia.conversation_members,
			odysseia.attachments, odysseia.message_attachments, odysseia.emojis TO odysseia_runtime;
		GRANT UPDATE(owner_id,dissolved_at,dm_low,dm_high) ON odysseia.conversations TO odysseia_runtime;
		GRANT UPDATE(role,joined_position,last_read_position,joined_at,removed_at)
			ON odysseia.conversation_members TO odysseia_runtime;
		GRANT UPDATE(status,expired_at) ON odysseia.attachments TO odysseia_runtime;
		GRANT UPDATE(deleted_at) ON odysseia.emojis TO odysseia_runtime;`)
	if err != nil {
		t.Fatal(err)
	}
	issuerMux := http.NewServeMux()
	idp := newRealtimeFixtureServer(t, base.pool.Ping, issuerMux)
	issuer, err := mockidp.New(idp.URL)
	if err != nil {
		t.Fatal(err)
	}
	issuerMux.Handle("/", issuer.Handler("odysseia-p4"))
	verifier, err := tokenauth.New(ctx, tokenauth.Config{
		Issuer: issuer.Origin(), Audience: "odysseia-p4", JWKSURL: issuer.JWKSURL(),
		CAFile: filepath.Join(os.Getenv("ODYSSEIA_TEST_REALTIME_TLS_DIR"), "ca.crt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	objects := blob.NewMemory()
	if err := base.store.UseAttachments(AttachmentSettings{
		Store: objects, Limits: media.DefaultLimits(), UploadsPerMinute: 20,
		QuotaBytes: 100 << 20, PresignTTL: time.Minute, MaxPerMessage: 4,
	}); err != nil {
		t.Fatal(err)
	}
	api := httpapi.NewAPI(verifier.Authenticate, NewUsers(base.pool).Ensure)
	api.RegisterConversations(base.store)
	api.RegisterEmoji(base.store, emoji.DefaultLimits())
	if err := api.RegisterRealtime(base.signer, base.store, base.proxyKey); err != nil {
		t.Fatal(err)
	}
	f := &conversationRealtimeFixture{
		base: base, server: newRealtimeFixtureServer(t, base.pool.Ping, api),
		users: map[string]identity.User{}, tokens: map[string]string{}, blobs: objects,
	}
	for _, name := range []string{"alice", "bob", "cara", "outsider"} {
		token, err := issuer.SignAccessToken(name, "odysseia-p4", mockidp.AlgRS256, 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		f.tokens[name] = token
		var user identity.User
		f.call(t, ctx, name, "GET", "/v1/me", nil, &user, 200)
		f.users[name] = user
	}
	if _, err := NewForums(base.owner).Provision(ctx, "P4 shared forum", []string{f.users["alice"].ID, f.users["bob"].ID, f.users["cara"].ID}); err != nil {
		t.Fatal(err)
	}
	public, err := blob.NewMemoryPublic("https://emoji.invalid/p4")
	if err != nil {
		t.Fatal(err)
	}
	if err := base.store.UseEmoji(EmojiSettings{Store: public, Limits: emoji.DefaultLimits(), Owners: []string{f.users["alice"].ID}}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *conversationRealtimeFixture) call(t *testing.T, ctx context.Context, actor, method, path string, body, result any, want int) {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, f.server.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+f.tokens[actor])
	request.Header.Set("Idempotency-Key", uuid.NewString())
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := f.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want {
		t.Fatalf("%s %s as %s: status=%d want=%d body=%s", method, path, actor, response.StatusCode, want, data)
	}
	if result != nil {
		if err := json.Unmarshal(data, result); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *conversationRealtimeFixture) connectionToken(t *testing.T, ctx context.Context, actor string) string {
	t.Helper()
	var token realtime.Token
	f.call(t, ctx, actor, "POST", "/v1/realtime/connection-token", nil, &token, 200)
	var channel struct {
		Channel string `json:"channel"`
	}
	f.call(t, ctx, actor, "POST", "/v1/me/inbox/subscription", nil, &channel, 200)
	if channel.Channel != "inbox:#"+f.users[actor].ID {
		t.Fatal("API returned another user's inbox")
	}
	return token.Token
}

func readInboxEvent(t *testing.T, ctx context.Context, peer *clusterPeer, kind, scope string) (forumPublication, uint64) {
	t.Helper()
	pub := awaitRealtime(t, ctx, peer.messages, kind)
	var event forumPublication
	if err := json.Unmarshal(pub.Data, &event); err != nil {
		t.Fatal(err)
	}
	id, err := uuid.Parse(event.ID)
	if err != nil || id == uuid.Nil || id.String() != event.ID || event.Version != 1 || event.Type != kind || event.ScopeID != scope || event.Position < 0 || pub.Offset == 0 {
		t.Fatalf("invalid inbox event type=%s scope=%s position=%d", event.Type, event.ScopeID, event.Position)
	}
	return event, pub.Offset
}

func readInboxMessage(t *testing.T, ctx context.Context, peer *clusterPeer, want message.Message) (forumPublication, uint64) {
	t.Helper()
	event, offset := readInboxEvent(t, ctx, peer, eventConversationMessage, want.StreamID)
	var got message.Message
	if err := json.Unmarshal(event.Data, &got); err != nil {
		t.Fatal(err)
	}
	if event.Position != want.Sequence || !sameStoredMessage(got, want) || !reflect.DeepEqual(got.Attachments, want.Attachments) || !reflect.DeepEqual(got.Emojis, want.Emojis) {
		t.Fatal("personal WSS message differs from committed HTTPS response")
	}
	return event, offset
}

func (f *conversationRealtimeFixture) upload(t *testing.T, ctx context.Context, actor, path string, data []byte, fields map[string]string, result any) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", "synthetic.fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, "POST", f.server.URL+path, &body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Authorization", "Bearer "+f.tokens[actor])
	request.Header.Set("Idempotency-Key", uuid.NewString())
	response, err := f.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("synthetic upload status=%d", response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(result); err != nil {
		t.Fatal(err)
	}
}

func inboxHistory(t *testing.T, ctx context.Context, peer *clusterPeer) []forumPublication {
	t.Helper()
	history, err := peer.subscription.History(ctx, centrifuge.WithHistoryLimit(-1))
	if err != nil {
		t.Fatal(err)
	}
	events := make([]forumPublication, 0, len(history.Publications))
	for _, publication := range history.Publications {
		var event forumPublication
		if err := json.Unmarshal(publication.Data, &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}
