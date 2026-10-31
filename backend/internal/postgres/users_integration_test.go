//go:build integration

package postgres

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"odysseia.local/backend/internal/httpapi"
	"odysseia.local/backend/internal/identity"
	"odysseia.local/backend/internal/tokenauth"
)

func testUsers(t *testing.T, ctx context.Context, migrationURL string, runtimePool *pgxpool.Pool) {
	t.Helper()
	ownerPool, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerPool.Close()
	_, err = ownerPool.Exec(ctx, `GRANT USAGE ON SCHEMA odysseia TO odysseia_runtime;
		GRANT SELECT, INSERT ON odysseia.users TO odysseia_runtime;`)
	if err != nil {
		t.Fatal(err)
	}
	reader := NewUsers(runtimePool)
	external := identity.ExternalID{Issuer: "https://issuer.example", Subject: "same-subject"}
	if _, err := reader.Find(ctx, external); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("unprovisioned lookup: %v", err)
	}
	var count int
	if err := runtimePool.QueryRow(ctx, "SELECT count(*) FROM odysseia.users").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("read lookup unexpectedly provisioned a user")
	}
	type result struct {
		user identity.User
		err  error
	}
	results := make(chan result, 12)
	for range 12 {
		go func() {
			user, err := reader.Ensure(ctx, external)
			results <- result{user: user, err: err}
		}()
	}
	var expected identity.User
	for range 12 {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		if expected.ID == "" {
			expected = got.user
		}
		if got.user.ID != expected.ID || !got.user.CreatedAt.Equal(expected.CreatedAt) {
			t.Fatal("concurrent provisioning changed local user identity or creation time")
		}
	}
	if err := runtimePool.QueryRow(ctx, "SELECT count(*) FROM odysseia.users").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("concurrent provisioning created %d users, want 1", count)
	}
	other := identity.ExternalID{Issuer: "https://another-issuer.example", Subject: external.Subject}
	otherUser, err := reader.Ensure(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if otherUser.ID == expected.ID {
		t.Fatal("same subject from different issuers merged into one account")
	}
	_, err = runtimePool.Exec(ctx, `DELETE FROM odysseia.users WHERE subject = $1`, external.Subject)
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != "42501" {
		t.Fatalf("runtime must not delete users: %v", err)
	}
	t.Run("HTTPS access token to real database", func(t *testing.T) {
		testIdentityAPI(t, ctx, reader, runtimePool)
	})
}

func testIdentityAPI(t *testing.T, ctx context.Context, reader *Users, runtimePool *pgxpool.Pool) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwk, err := jwkset.NewJWKFromKey(key.Public(), jwkset.JWKOptions{
		Metadata: jwkset.JWKMetadataOptions{KID: "integration", ALG: jwkset.AlgRS256, USE: jwkset.UseSig},
	})
	if err != nil {
		t.Fatal(err)
	}
	issuer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jwkset.JWKSMarshal{Keys: []jwkset.JWKMarshal{jwk.Marshal()}})
	}))
	issuer.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	issuer.StartTLS()
	defer issuer.Close()
	caFile := filepath.Join(t.TempDir(), "issuer-ca.crt")
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issuer.Certificate().Raw})
	if err := os.WriteFile(caFile, cert, 0600); err != nil {
		t.Fatal(err)
	}
	verifierCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	verifier, err := tokenauth.New(verifierCtx, tokenauth.Config{
		Issuer: issuer.URL, Audience: "integration-api", JWKSURL: issuer.URL + "/jwks", CAFile: caFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewUnstartedServer(nil)
	api.Config = httpapi.NewServer("127.0.0.1:0", runtimePool.Ping, httpapi.NewAPI(verifier.Authenticate, reader.Ensure))
	api.TLS = api.Config.TLSConfig.Clone()
	api.StartTLS()
	defer api.Close()
	external := identity.ExternalID{Issuer: issuer.URL, Subject: "api-user"}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": external.Issuer, "sub": external.Subject, "aud": "integration-api",
		"exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Add(-time.Second).Unix(),
		"jti": "integration-token", "client_id": "synthetic-browser",
	})
	token.Header["typ"] = "at+jwt"
	token.Header["kid"] = "integration"
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	getMe := func() *http.Response {
		t.Helper()
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, api.URL+"/v1/me", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+raw)
		r.Header.Set("X-User-ID", "forged-local-id")
		response, err := api.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	response := getMe()
	defer response.Body.Close()
	if response.StatusCode != 200 || response.TLS.Version != tls.VersionTLS13 {
		t.Fatalf("first API status=%d; expected TLS 1.3 success", response.StatusCode)
	}
	var actual identity.User
	if err := json.NewDecoder(response.Body).Decode(&actual); err != nil {
		t.Fatal(err)
	}
	expected, err := reader.Find(ctx, external)
	if err != nil {
		t.Fatal(err)
	}
	if actual.ID != expected.ID || actual.ID == "" {
		t.Fatal("API did not bootstrap the user from verified issuer and subject")
	}
	again := getMe()
	defer again.Body.Close()
	var repeated identity.User
	if again.StatusCode != 200 || json.NewDecoder(again.Body).Decode(&repeated) != nil || repeated.ID != expected.ID {
		t.Fatal("repeated authenticated request changed the local user")
	}
}
