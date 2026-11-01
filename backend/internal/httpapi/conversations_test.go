package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"odysseia.local/backend/internal/conversation"
	"odysseia.local/backend/internal/identity"
	"odysseia.local/backend/internal/media"
	"odysseia.local/backend/internal/message"
)

func TestConversationRoutesHideNonMembersAndKeepTheReadCursor(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(openAPI)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if doc.Info.Version != "0.10.0" {
		t.Fatalf("version %s", doc.Info.Version)
	}
	actor := identity.User{ID: "dc9de165-b912-4dcb-8620-8dfdb30176b7", CreatedAt: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)}
	other := "6d2f0c2e-1b7a-4e4e-9c1a-2f6b0d9a7c31"
	store := &conversationFake{id: "7c9e6679-7425-40de-944b-e07fc1f90ae7", hidden: true}
	api := NewAPI(func(_ context.Context, raw string) (identity.Authentication, error) {
		if raw != "valid-access-token" {
			return identity.Authentication{}, identity.ErrNotFound
		}
		return identity.Authentication{ExternalID: identity.ExternalID{Issuer: "https://issuer.example", Subject: "verified-subject"}, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}, func(context.Context, identity.ExternalID) (identity.User, error) {
		return actor, nil
	})
	api.RegisterConversations(store)
	call := func(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer valid-access-token")
		r.Header.Set("Content-Type", "application/json")
		for key, value := range headers {
			r.Header.Set(key, value)
		}
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		return w
	}
	missing := call(http.MethodGet, "/v1/conversations/"+store.id, "", nil)
	if missing.Code != http.StatusNotFound || store.gets != 1 {
		t.Fatalf("non-member status=%d gets=%d body=%s", missing.Code, store.gets, missing.Body.String())
	}
	assertProblem(t, doc, missing, "resource_not_found")
	store.hidden = false
	store.manager = true
	held := call(http.MethodDelete, "/v1/conversations/"+store.id+"/members/"+actor.ID, "", nil)
	if held.Code != http.StatusConflict {
		t.Fatalf("last manager status=%d body=%s", held.Code, held.Body.String())
	}
	assertProblem(t, doc, held, "last_manager")
	inbox := call(http.MethodPost, "/v1/me/inbox/subscription", "", nil)
	if inbox.Code != http.StatusOK || !strings.Contains(inbox.Body.String(), conversation.InboxChannel(actor.ID)) {
		t.Fatalf("inbox status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	assertSchema(t, doc, "RealtimeSubscription", inbox.Body.Bytes())
	unread := call(http.MethodGet, "/v1/me/unread", "", nil)
	if unread.Code != http.StatusOK {
		t.Fatal(unread.Body.String())
	}
	assertSchema(t, doc, "UnreadSummary", unread.Body.Bytes())
	opened := call(http.MethodPost, "/v1/conversations/dm", `{"user_id":"`+other+`"}`, map[string]string{"Idempotency-Key": "8b0b0d4a-6e2a-4a1e-9d3c-5f7a1c2b4d60"})
	if opened.Code != http.StatusCreated {
		t.Fatalf("open status=%d body=%s", opened.Code, opened.Body.String())
	}
	assertSchema(t, doc, "Conversation", opened.Body.Bytes())
	read := call(http.MethodPut, "/v1/conversations/"+store.id+"/read", `{"position":"1"}`, nil)
	if read.Code != http.StatusOK || store.readPosition != 1 {
		t.Fatalf("read status=%d position=%d body=%s", read.Code, store.readPosition, read.Body.String())
	}
	assertSchema(t, doc, "ConversationReadState", read.Body.Bytes())
}

type conversationFake struct {
	id               string
	hidden           bool
	manager          bool
	rejectAttachment bool
	gets             int
	readPosition     int64
}

func (f *conversationFake) OpenDM(context.Context, string, string, conversation.DMDraft) (conversation.Conversation, bool, error) {
	if f.hidden {
		return conversation.Conversation{}, false, conversation.ErrNotFound
	}
	return f.view(), true, nil
}

func (f *conversationFake) CreateGroup(context.Context, string, string, conversation.GroupDraft) (conversation.Conversation, error) {
	return f.view(), nil
}

func (f *conversationFake) GetConversation(context.Context, string, string) (conversation.Conversation, error) {
	f.gets++
	if f.hidden {
		return conversation.Conversation{}, conversation.ErrNotFound
	}
	return f.view(), nil
}

func (f *conversationFake) ListConversations(context.Context, string, time.Time, string, int) ([]conversation.Conversation, error) {
	return []conversation.Conversation{f.view()}, nil
}

func (f *conversationFake) ListConversationMessages(context.Context, string, string, int64, int) ([]message.Message, error) {
	if f.hidden {
		return nil, conversation.ErrNotFound
	}
	return nil, nil
}

func (f *conversationFake) SendConversationMessage(context.Context, string, string, string, conversation.MessageDraft) (message.Message, error) {
	if f.hidden {
		return message.Message{}, conversation.ErrNotFound
	}
	if f.rejectAttachment {
		return message.Message{}, conversation.ErrAttachment
	}
	return message.Message{}, nil
}

func (f *conversationFake) AddConversationMember(context.Context, string, string, string) (conversation.Member, error) {
	return conversation.Member{}, nil
}

func (f *conversationFake) RemoveConversationMember(context.Context, string, string, string) error {
	if f.hidden {
		return conversation.ErrNotFound
	}
	if f.manager {
		return conversation.ErrLastManager
	}
	return nil
}

func (f *conversationFake) TransferConversationOwner(context.Context, string, string, string) (conversation.Conversation, error) {
	return f.view(), nil
}

func (f *conversationFake) DissolveConversation(context.Context, string, string) error {
	return nil
}

func (f *conversationFake) MarkConversationRead(_ context.Context, _, _ string, position int64) (conversation.ReadState, error) {
	if f.hidden {
		return conversation.ReadState{}, conversation.ErrNotFound
	}
	f.readPosition = position
	return conversation.ReadState{Position: position, Unread: 0}, nil
}

func (f *conversationFake) ListUnread(context.Context, string) (conversation.UnreadSummary, error) {
	return conversation.UnreadSummary{Items: []conversation.Unread{}, Total: 0}, nil
}

func (f *conversationFake) UploadConversationAttachment(context.Context, string, string, media.Prepared) (message.Attachment, error) {
	if f.hidden {
		return message.Attachment{}, conversation.ErrNotFound
	}
	return message.Attachment{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", ConversationID: f.id, Kind: "image", ContentType: "image/jpeg", ByteSize: 1, CreatedAt: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)}, nil
}

func (f *conversationFake) OpenConversationAttachment(context.Context, string, string, string) (string, error) {
	if f.hidden {
		return "", conversation.ErrNotFound
	}
	if f.rejectAttachment {
		return "", conversation.ErrAttachment
	}
	return "https://blob.invalid/get?key=conversations", nil
}

func (f *conversationFake) view() conversation.Conversation {
	return conversation.Conversation{
		ID: f.id, Kind: "dm", CreatedBy: "dc9de165-b912-4dcb-8620-8dfdb30176b7",
		CreatedAt: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), ActivityAt: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC),
		Members: []conversation.Member{{
			UserID: "dc9de165-b912-4dcb-8620-8dfdb30176b7", Role: "member",
			JoinedAt: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC),
		}},
	}
}
