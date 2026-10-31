package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"odysseia.local/backend/internal/identity"
)

func TestCurrentUserContractAndTrustBoundary(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(openAPI)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	verified := identity.ExternalID{Issuer: "https://issuer.example", Subject: "verified-subject"}
	user := identity.User{ID: "dc9de165-b912-4dcb-8620-8dfdb30176b7", CreatedAt: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)}
	lookupCalls := 0
	lookupErr := error(nil)
	api := NewAPI(func(_ context.Context, raw string) (identity.Authentication, error) {
		if raw != "valid-access-token" {
			return identity.Authentication{}, errors.New("secret verifier internals")
		}
		return identity.Authentication{ExternalID: verified, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}, func(_ context.Context, got identity.ExternalID) (identity.User, error) {
		lookupCalls++
		if got != verified {
			t.Fatalf("untrusted identity reached lookup: %+v", got)
		}
		return user, lookupErr
	})
	tests := []struct {
		name   string
		header string
		err    error
		status int
		code   string
	}{
		{"missing bearer", "", nil, 401, "authentication_required"},
		{"wrong scheme", "Basic valid-access-token", nil, 401, "invalid_token"},
		{"invalid token", "Bearer forged-token", nil, 401, "invalid_token"},
		{"unknown local account", "Bearer valid-access-token", identity.ErrNotFound, 404, "account_not_provisioned"},
		{"database error", "Bearer valid-access-token", errors.New("secret database internals"), 503, "dependency_unavailable"},
		{"verified local user", "bearer valid-access-token", nil, 200, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lookupErr = test.err
			lookupCalls = 0
			r := httptest.NewRequest(http.MethodGet, "/v1/me?access_token=valid-access-token&subject=forged", nil)
			r.Header.Set("Cookie", "access_token=valid-access-token")
			r.Header.Set("X-User-ID", "forged")
			if test.header != "" {
				r.Header.Set("Authorization", test.header)
			}
			w := httptest.NewRecorder()
			api.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("status=%d, want %d", w.Code, test.status)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("identity response must not be cached")
			}
			if w.Code == 401 && lookupCalls != 0 {
				t.Fatal("unauthenticated request reached user storage")
			}
			if strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "verified-subject") {
				t.Fatal("private identity or error details leaked")
			}
			var body any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			schemaName := "Problem"
			if w.Code == 200 {
				schemaName = "User"
			}
			if err := doc.Components.Schemas[schemaName].Value.VisitJSON(body); err != nil {
				t.Fatalf("response violates OpenAPI schema: %v", err)
			}
			if test.code == "" {
				return
			}
			var problem Problem
			if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
				t.Fatal(err)
			}
			if problem.Code != test.code || problem.Status != w.Code {
				t.Fatalf("unexpected problem: %+v", problem)
			}
		})
	}
}

func TestOpenAPIAndProtectedRouteMethods(t *testing.T) {
	api := NewAPI(func(context.Context, string) (identity.Authentication, error) {
		t.Fatal("unprotected or wrong-method route invoked verifier")
		return identity.Authentication{}, nil
	}, func(context.Context, identity.ExternalID) (identity.User, error) {
		t.Fatal("unexpected storage call")
		return identity.User{}, nil
	})
	w := httptest.NewRecorder()
	api.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if w.Code != 200 || w.Body.String() != string(openAPI) {
		t.Fatal("published contract differs from the validated embedded contract")
	}
	w = httptest.NewRecorder()
	api.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/me", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatal("identity reads must not expose an implicit provisioning command")
	}
}
