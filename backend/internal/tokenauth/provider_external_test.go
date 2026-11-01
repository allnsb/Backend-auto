//go:build external

package tokenauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Opt-in provider acceptance uses tokens from a dedicated test account. It
// verifies the deployed issuer; the mock issuer's passing tests are not proof
// of compatibility with a configured ZITADEL deployment.
func TestExternalIdentityProvider(t *testing.T) {
	if os.Getenv("ODYSSEIA_TEST_EXTERNAL_IDP") != "1" {
		t.Skip("real IdP acceptance requires explicit ODYSSEIA_TEST_EXTERNAL_IDP=1")
	}
	cfg := Config{
		Issuer: os.Getenv("ODYSSEIA_AUTH_ISSUER"), Audience: os.Getenv("ODYSSEIA_AUTH_AUDIENCE"),
		JWKSURL: os.Getenv("ODYSSEIA_AUTH_JWKS_URL"), CAFile: os.Getenv("ODYSSEIA_AUTH_CA_FILE"),
	}
	access, id := os.Getenv("ODYSSEIA_TEST_ACCESS_TOKEN"), os.Getenv("ODYSSEIA_TEST_ID_TOKEN")
	if cfg.Issuer == "" || cfg.Audience == "" || cfg.JWKSURL == "" || access == "" || id == "" {
		t.Fatal("real IdP acceptance requires issuer, audience, JWKS URL and test access/ID tokens")
	}
	if err := validateHTTPSURL(cfg.Issuer); err != nil {
		t.Fatal("real issuer must use HTTPS")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := newHTTPClient(cfg.CAFile)
	if err != nil {
		t.Fatal("cannot initialize verified provider TLS")
	}
	request, err := http.NewRequestWithContext(ctx, "GET", strings.TrimSuffix(cfg.Issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		t.Fatal("cannot construct provider discovery request")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("provider HTTPS discovery failed")
	}
	defer response.Body.Close()
	var discovery struct {
		Issuer string `json:"issuer"`
		JWKS   string `json:"jwks_uri"`
	}
	if response.StatusCode != 200 {
		t.Fatalf("provider discovery status=%d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&discovery); err != nil {
		t.Fatal("provider discovery is not valid JSON")
	}
	if discovery.Issuer != cfg.Issuer || discovery.JWKS != cfg.JWKSURL {
		t.Fatal("configured issuer/JWKS differs from verified discovery")
	}
	verifier, err := New(ctx, cfg)
	if err != nil {
		t.Fatal("provider JWKS initialization failed")
	}
	auth, err := verifier.Authenticate(ctx, access)
	if err != nil || auth.ExternalID.Issuer != cfg.Issuer || auth.ExternalID.Subject == "" || !auth.ExpiresAt.After(time.Now()) {
		t.Fatal("provider access token failed the configured token contract; do not weaken ID-token isolation to bypass this failure")
	}
	if _, err := verifier.Authenticate(ctx, id); err == nil {
		t.Fatal("provider ID token was accepted as an API access token")
	}
	cfg.Audience = "odysseia-external-audience-must-not-match"
	wrongAudience, err := New(ctx, cfg)
	if err != nil {
		t.Fatal("cannot initialize audience rejection probe")
	}
	if _, err := wrongAudience.Authenticate(ctx, access); err == nil {
		t.Fatal("provider access token was accepted with the wrong audience")
	}
}
