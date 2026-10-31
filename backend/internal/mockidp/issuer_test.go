package mockidp

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"odysseia.local/backend/internal/tokenauth"
)

func TestDiscoveryJWKSAndAccessTokenRejection(t *testing.T) {
	mux := http.NewServeMux()
	issuer := httptest.NewUnstartedServer(mux)
	issuer.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	issuer.StartTLS()
	defer issuer.Close()
	idp, err := New(issuer.URL)
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle("/", idp.Handler("odysseia-api"))

	discovery := getJSON(t, issuer.Client(), issuer.URL+"/.well-known/openid-configuration")
	if discovery["issuer"] != issuer.URL {
		t.Fatalf("issuer = %v", discovery["issuer"])
	}
	if discovery["jwks_uri"] != issuer.URL+"/oauth/v2/keys" {
		t.Fatalf("jwks_uri = %v", discovery["jwks_uri"])
	}
	if discovery["token_endpoint"] != issuer.URL+"/oauth/v2/token" {
		t.Fatalf("token_endpoint = %v", discovery["token_endpoint"])
	}
	algs, _ := discovery["id_token_signing_alg_values_supported"].([]any)
	if len(algs) != 2 || algs[0] != AlgRS256 || algs[1] != AlgES256 {
		t.Fatalf("signing algs = %#v", algs)
	}
	jwks := getJSON(t, issuer.Client(), idp.JWKSURL())
	keys, _ := jwks["keys"].([]any)
	if len(keys) != 2 {
		t.Fatalf("jwks keys = %#v", keys)
	}
	for _, item := range keys {
		key := item.(map[string]any)
		if key["use"] != "sig" || (key["alg"] != AlgRS256 && key["alg"] != AlgES256) || key["kid"] == "" {
			t.Fatalf("key = %#v", key)
		}
	}
	tokenResponse, err := issuer.Client().Post(issuer.URL+"/oauth/v2/token", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer tokenResponse.Body.Close()
	if tokenResponse.StatusCode != http.StatusNotImplemented {
		t.Fatalf("production token path status = %d", tokenResponse.StatusCode)
	}

	caFile := filepath.Join(t.TempDir(), "idp-ca.crt")
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issuer.Certificate().Raw})
	if err := os.WriteFile(caFile, cert, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	verifier, err := tokenauth.New(ctx, tokenauth.Config{
		Issuer: issuer.URL, Audience: "odysseia-api", JWKSURL: idp.JWKSURL(), CAFile: caFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, alg := range []string{AlgRS256, AlgES256} {
		raw, err := idp.SignAccessToken("first-user", "odysseia-api", alg, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		external, err := verifier.Verify(ctx, raw)
		if err != nil || external.Subject != "first-user" || external.Issuer != issuer.URL {
			t.Fatalf("%s token: identity=%+v err=%v", alg, external, err)
		}
	}
	none := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"sub": "first-user"})
	none.Header["typ"] = "at+jwt"
	none.Header["kid"] = "mock-rs256"
	unsigned, err := none.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	hmac := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": issuer.URL, "aud": "odysseia-api", "sub": "first-user"})
	hmac.Header["typ"] = "at+jwt"
	hmac.Header["kid"] = "mock-rs256"
	shared, err := hmac.SignedString([]byte("not-a-public-key"))
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{"none": unsigned, "HS256": shared} {
		t.Run(name, func(t *testing.T) {
			_, err := verifier.Verify(ctx, raw)
			if !errors.Is(err, tokenauth.ErrInvalidToken) {
				t.Fatalf("want invalid token, got %v", err)
			}
		})
	}
}

func getJSON(t *testing.T, client *http.Client, url string) map[string]any {
	t.Helper()
	response, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", url, response.StatusCode, body)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	return document
}
