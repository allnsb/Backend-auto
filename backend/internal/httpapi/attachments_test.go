package httpapi

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"odysseia.local/backend/internal/identity"
	"odysseia.local/backend/internal/media"
)

func TestUploadExtendsTheReadDeadline(t *testing.T) {
	actor := identity.User{ID: "dc9de165-b912-4dcb-8620-8dfdb30176b7", CreatedAt: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)}
	api := NewAPI(func(context.Context, string) (identity.Authentication, error) {
		return identity.Authentication{ExternalID: identity.ExternalID{Issuer: "https://issuer.example", Subject: "verified-subject"}, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}, func(context.Context, identity.ExternalID) (identity.User, error) {
		return actor, nil
	})
	api.RegisterConversations(&conversationFake{id: "7c9e6679-7425-40de-944b-e07fc1f90ae7", hidden: true})
	body := multipartBody(t, "file", "image.svg", []byte("<svg/>"), "")
	writer := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	request := httptest.NewRequest(http.MethodPost, "/v1/conversations/7c9e6679-7425-40de-944b-e07fc1f90ae7/attachments", bytes.NewReader(body.body))
	request.Header.Set("Authorization", "Bearer valid-access-token")
	request.Header.Set("Content-Type", body.contentType)
	api.ServeHTTP(writer, request)
	if writer.readAt.Before(time.Now().Add(90*time.Second)) || writer.writeAt.Before(time.Now().Add(90*time.Second)) {
		t.Fatalf("upload deadlines read=%s write=%s", writer.readAt, writer.writeAt)
	}
}

type deadlineWriter struct {
	*httptest.ResponseRecorder
	readAt  time.Time
	writeAt time.Time
}

func (w *deadlineWriter) SetReadDeadline(deadline time.Time) error {
	w.readAt = deadline
	return nil
}

func (w *deadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.writeAt = deadline
	return nil
}

func TestAttachmentUploadRejectsTypeAndSizeAndHiddenDownload(t *testing.T) {
	actor := identity.User{ID: "dc9de165-b912-4dcb-8620-8dfdb30176b7", CreatedAt: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)}
	store := &conversationFake{id: "7c9e6679-7425-40de-944b-e07fc1f90ae7", hidden: true}
	api := NewAPI(func(context.Context, string) (identity.Authentication, error) {
		return identity.Authentication{ExternalID: identity.ExternalID{Issuer: "https://issuer.example", Subject: "verified-subject"}, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}, func(context.Context, identity.ExternalID) (identity.User, error) {
		return actor, nil
	})
	limits := media.DefaultLimits()
	limits.MaxBytes = 32
	if err := api.SetMediaLimits(limits); err != nil {
		t.Fatal(err)
	}
	api.RegisterConversations(store)

	svg := multipartBody(t, "file", "image.svg", []byte("<svg/>"), "")
	svgResponse := callAttachment(t, api, http.MethodPost, "/v1/conversations/"+store.id+"/attachments", svg.body, svg.contentType)
	if svgResponse.Code != http.StatusUnsupportedMediaType || !bytes.Contains(svgResponse.Body.Bytes(), []byte("unsupported_media_type")) {
		t.Fatalf("svg status=%d body=%s", svgResponse.Code, svgResponse.Body.String())
	}

	oversized := multipartBody(t, "file", "note.bin", bytes.Repeat([]byte("x"), 33), "")
	large := callAttachment(t, api, http.MethodPost, "/v1/conversations/"+store.id+"/attachments", oversized.body, oversized.contentType)
	if large.Code != http.StatusRequestEntityTooLarge || !bytes.Contains(large.Body.Bytes(), []byte("body_too_large")) {
		t.Fatalf("size status=%d body=%s", large.Code, large.Body.String())
	}

	download := callAttachment(t, api, http.MethodGet, "/v1/conversations/"+store.id+"/attachments/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", nil, "")
	if download.Code != http.StatusNotFound || download.Header().Get("Location") != "" {
		t.Fatalf("hidden download status=%d location=%s body=%s", download.Code, download.Header().Get("Location"), download.Body.String())
	}

	store.hidden = false
	store.rejectAttachment = true
	body := []byte(`{"body":"","attachment_ids":["aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"]}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/conversations/"+store.id+"/messages", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-access-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	rejected := httptest.NewRecorder()
	api.ServeHTTP(rejected, request)
	if rejected.Code != 422 || !bytes.Contains(rejected.Body.Bytes(), []byte("attachment_rejected")) {
		t.Fatalf("bind status=%d body=%s", rejected.Code, rejected.Body.String())
	}
}

type multipartFile struct {
	body        []byte
	contentType string
}

func multipartBody(t *testing.T, field, filename string, payload []byte, duration string) multipartFile {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(payload); err != nil {
		t.Fatal(err)
	}
	if duration != "" {
		if err := writer.WriteField("duration_ms", duration); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return multipartFile{body: buf.Bytes(), contentType: writer.FormDataContentType()}
}

func callAttachment(t *testing.T, api *API, method, path string, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-access-token")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	return response
}
