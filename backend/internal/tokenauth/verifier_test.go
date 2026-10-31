package tokenauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/golang-jwt/jwt/v5"
)

type tokenFixture struct {
	server   *httptest.Server
	config   Config
	key      ed25519.PrivateKey
	keys     atomic.Value
	requests atomic.Int32
}

func newTokenFixture(t *testing.T) *tokenFixture {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &tokenFixture{key: private}
	f.publish(t, "first", private.Public(), jwkset.UseSig, jwkset.AlgEdDSA)
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.TLS == nil || r.TLS.Version != tls.VersionTLS13 {
			t.Error("JWKS request did not use TLS 1.3")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(f.keys.Load().([]byte))
	}))
	f.server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	caFile := filepath.Join(t.TempDir(), "test-ca.crt")
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.Certificate().Raw})
	if err := os.WriteFile(caFile, cert, 0600); err != nil {
		t.Fatal(err)
	}
	f.config = Config{Issuer: f.server.URL, Audience: "odysseia-api", JWKSURL: f.server.URL + "/jwks", CAFile: caFile}
	return f
}

func (f *tokenFixture) publish(t *testing.T, kid string, key any, use jwkset.USE, alg jwkset.ALG) {
	t.Helper()
	jwk, err := jwkset.NewJWKFromKey(key, jwkset.JWKOptions{
		Metadata: jwkset.JWKMetadataOptions{KID: kid, ALG: alg, USE: use},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(jwkset.JWKSMarshal{Keys: []jwkset.JWKMarshal{jwk.Marshal()}})
	if err != nil {
		t.Fatal(err)
	}
	f.keys.Store(data)
}

func (f *tokenFixture) claims() jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss": f.config.Issuer, "aud": f.config.Audience, "sub": "user-one",
		"exp": now.Add(5 * time.Minute).Unix(), "iat": now.Add(-time.Second).Unix(),
		"jti": "synthetic-token-id", "client_id": "synthetic-browser-client",
	}
}

func signedToken(t *testing.T, claims jwt.MapClaims, typ, kid string, key any, method jwt.SigningMethod) string {
	t.Helper()
	token := jwt.NewWithClaims(method, claims)
	token.Header["typ"] = typ
	token.Header["kid"] = kid
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestAccessTokenValidation(t *testing.T) {
	f := newTokenFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	verifier, err := New(ctx, f.config)
	if err != nil {
		t.Fatal(err)
	}
	valid := signedToken(t, f.claims(), "at+jwt", "first", f.key, jwt.SigningMethodEdDSA)
	external, err := verifier.Verify(ctx, valid)
	if err != nil || external.Issuer != f.config.Issuer || external.Subject != "user-one" {
		t.Fatalf("valid access token: identity=%+v, err=%v", external, err)
	}
	if f.requests.Load() != 1 {
		t.Fatal("known signing key was not served from cache")
	}
	tests := []struct {
		name   string
		change func(jwt.MapClaims)
	}{
		{"wrong issuer", func(c jwt.MapClaims) { c["iss"] = "https://untrusted.example" }},
		{"wrong audience", func(c jwt.MapClaims) { c["aud"] = "another-api" }},
		{"expired", func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Minute).Unix() }},
		{"not yet valid", func(c jwt.MapClaims) { c["nbf"] = time.Now().Add(time.Hour).Unix() }},
		{"future issued", func(c jwt.MapClaims) { c["iat"] = time.Now().Add(time.Hour).Unix() }},
		{"missing expiry", func(c jwt.MapClaims) { delete(c, "exp") }},
		{"missing issued at", func(c jwt.MapClaims) { delete(c, "iat") }},
		{"missing subject", func(c jwt.MapClaims) { delete(c, "sub") }},
		{"missing token ID", func(c jwt.MapClaims) { delete(c, "jti") }},
		{"missing client ID", func(c jwt.MapClaims) { delete(c, "client_id") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims := f.claims()
			test.change(claims)
			raw := signedToken(t, claims, "at+jwt", "first", f.key, jwt.SigningMethodEdDSA)
			_, err := verifier.Verify(ctx, raw)
			if !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("want invalid token, got %v", err)
			}
		})
	}
	_, forgedKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	invalidTokens := map[string]string{
		"forged signature": signedToken(t, f.claims(), "at+jwt", "first", forgedKey, jwt.SigningMethodEdDSA),
		"ID token":         signedToken(t, f.claims(), "JWT", "first", f.key, jwt.SigningMethodEdDSA),
		"missing kid":      signedToken(t, f.claims(), "at+jwt", "", f.key, jwt.SigningMethodEdDSA),
		"HMAC confusion":   signedToken(t, f.claims(), "at+jwt", "first", []byte(f.key.Public().(ed25519.PublicKey)), jwt.SigningMethodHS256),
		"unsigned":         signedToken(t, f.claims(), "at+jwt", "first", jwt.UnsafeAllowNoneSignatureType, jwt.SigningMethodNone),
		"opaque":           "not-a-jwt",
	}
	for name, raw := range invalidTokens {
		t.Run(name, func(t *testing.T) {
			_, err := verifier.Verify(ctx, raw)
			if !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("want invalid token, got %v", err)
			}
		})
	}
}

func TestJWKSKeyRotationReplacesOldKeys(t *testing.T) {
	f := newTokenFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	verifier, err := New(ctx, f.config)
	if err != nil {
		t.Fatal(err)
	}
	oldToken := signedToken(t, f.claims(), "at+jwt", "first", f.key, jwt.SigningMethodEdDSA)
	_, next, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.publish(t, "second", next.Public(), jwkset.UseSig, jwkset.AlgEdDSA)
	newToken := signedToken(t, f.claims(), "at+jwt", "second", next, jwt.SigningMethodEdDSA)
	if _, err := verifier.Verify(ctx, newToken); err != nil {
		t.Fatalf("new signing key was not refreshed: %v", err)
	}
	if f.requests.Load() != 2 {
		t.Fatal("rotation should trigger exactly one additional JWKS fetch")
	}
	if _, err := verifier.Verify(ctx, oldToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("removed signing key was still trusted: %v", err)
	}
	if f.requests.Load() != 2 {
		t.Fatal("unknown key refresh was not rate limited")
	}
}

func TestES256AccessTokenAndSigningKeyUse(t *testing.T) {
	f := newTokenFixture(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.publish(t, "ecdsa", key.Public(), jwkset.UseSig, jwkset.AlgES256)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	verifier, err := New(ctx, f.config)
	if err != nil {
		t.Fatal(err)
	}
	raw := signedToken(t, f.claims(), "application/at+jwt", "ecdsa", key, jwt.SigningMethodES256)
	if _, err := verifier.Verify(ctx, raw); err != nil {
		t.Fatalf("ES256 access token: %v", err)
	}
	// An encryption-only key must not become a signing key, even with a valid signature.
	f.publish(t, "encryption-only", key.Public(), jwkset.UseEnc, jwkset.AlgES256)
	raw = signedToken(t, f.claims(), "at+jwt", "encryption-only", key, jwt.SigningMethodES256)
	if _, err := verifier.Verify(ctx, raw); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("encryption-only key was accepted for signing: %v", err)
	}
}

func TestJWKSRequiresTrustedTLSAndSuccessfulStartup(t *testing.T) {
	f := newTokenFixture(t)
	tests := []struct {
		name   string
		change func(*Config)
	}{
		{"plaintext JWKS", func(c *Config) { c.JWKSURL = "http://localhost/jwks" }},
		{"plaintext issuer", func(c *Config) { c.Issuer = "http://localhost" }},
		{"untrusted CA", func(c *Config) { c.CAFile = "" }},
		{"empty audience", func(c *Config) { c.Audience = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cfg := f.config
			test.change(&cfg)
			if _, err := New(ctx, cfg); err == nil {
				t.Fatal("unsafe or unavailable verifier initialized successfully")
			}
		})
	}
	f.keys.Store([]byte(`{"keys":`))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := New(ctx, f.config); err == nil {
		t.Fatal("invalid initial JWKS must fail startup")
	}
}
