package mockidp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	AlgRS256 = "RS256"
	AlgES256 = "ES256"
	rsaKID   = "mock-rs256"
	ecKID    = "mock-es256"
)

// Issuer is a development OpenID provider. Discovery and JWKS use the same
// paths as self-hosted ZITADEL so a deployment can swap issuer and audience.
type Issuer struct {
	origin   string
	rsaKey   *rsa.PrivateKey
	ecKey    *ecdsa.PrivateKey
	jwks     []byte
	document []byte
}

func New(origin string) (*Issuer, error) {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return nil, errors.New("mock issuer must be an absolute HTTPS origin")
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	keys, err := marshalJWKS(&rsaKey.PublicKey, &ecKey.PublicKey)
	if err != nil {
		return nil, err
	}
	document, err := json.Marshal(discoveryDocument(origin))
	if err != nil {
		return nil, err
	}
	return &Issuer{origin: origin, rsaKey: rsaKey, ecKey: ecKey, jwks: keys, document: document}, nil
}

func (i *Issuer) Origin() string { return i.origin }

func (i *Issuer) JWKSURL() string { return i.origin + "/oauth/v2/keys" }

// Handler serves ZITADEL-shaped discovery and JWKS. POST /mock/token mints a
// development access token; the production token path is advertised but not a login flow.
func (i *Issuer) Handler(audience string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(i.document)
	})
	mux.HandleFunc("GET /oauth/v2/keys", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(i.jwks)
	})
	mux.HandleFunc("POST /oauth/v2/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":             "not_implemented",
			"error_description": "Development tokens are minted at POST /mock/token. A self-hosted ZITADEL instance serves the authorization-code token endpoint at this path.",
		})
	})
	mux.HandleFunc("POST /mock/token", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Subject  string `json:"subject"`
			Audience string `json:"audience"`
			Alg      string `json:"alg"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil || body.Subject == "" || len(body.Subject) > 200 {
			http.Error(w, "invalid subject", http.StatusBadRequest)
			return
		}
		if body.Audience == "" {
			body.Audience = audience
		}
		if body.Alg == "" {
			body.Alg = AlgRS256
		}
		raw, err := i.SignAccessToken(body.Subject, body.Audience, body.Alg, 5*time.Minute)
		if err != nil {
			http.Error(w, "unsupported algorithm", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": raw,
			"token_type":   "Bearer",
			"expires_in":   300,
		})
	})
	return mux
}

// SignAccessToken builds an RFC 9068 access token from one of the published keys.
func (i *Issuer) SignAccessToken(subject, audience, alg string, ttl time.Duration) (string, error) {
	if subject == "" || audience == "" || ttl <= 0 {
		return "", errors.New("subject, audience and positive lifetime are required")
	}
	var method jwt.SigningMethod = jwt.SigningMethodRS256
	var key any = i.rsaKey
	kid := rsaKID
	if alg == AlgES256 {
		method = jwt.SigningMethodES256
		key = i.ecKey
		kid = ecKID
	} else if alg != AlgRS256 {
		return "", errors.New("mock issuer signs RS256 and ES256 access tokens")
	}
	now := time.Now()
	token := jwt.NewWithClaims(method, jwt.MapClaims{
		"iss": i.origin, "aud": audience, "sub": subject,
		"exp": now.Add(ttl).Unix(), "iat": now.Add(-time.Second).Unix(),
		"jti": uuid.NewString(), "client_id": "odysseia-mock",
	})
	token.Header["typ"] = "at+jwt"
	token.Header["kid"] = kid
	return token.SignedString(key)
}

func marshalJWKS(rsaPublic *rsa.PublicKey, ecPublic *ecdsa.PublicKey) ([]byte, error) {
	rsaJWK, err := jwkset.NewJWKFromKey(rsaPublic, jwkset.JWKOptions{
		Metadata: jwkset.JWKMetadataOptions{KID: rsaKID, ALG: jwkset.AlgRS256, USE: jwkset.UseSig},
	})
	if err != nil {
		return nil, err
	}
	ecJWK, err := jwkset.NewJWKFromKey(ecPublic, jwkset.JWKOptions{
		Metadata: jwkset.JWKMetadataOptions{KID: ecKID, ALG: jwkset.AlgES256, USE: jwkset.UseSig},
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(jwkset.JWKSMarshal{Keys: []jwkset.JWKMarshal{rsaJWK.Marshal(), ecJWK.Marshal()}})
}

func discoveryDocument(origin string) map[string]any {
	return map[string]any{
		"issuer":                                origin,
		"authorization_endpoint":                origin + "/oauth/v2/authorize",
		"token_endpoint":                        origin + "/oauth/v2/token",
		"introspection_endpoint":                origin + "/oauth/v2/introspect",
		"userinfo_endpoint":                     origin + "/oidc/v1/userinfo",
		"revocation_endpoint":                   origin + "/oauth/v2/revoke",
		"end_session_endpoint":                  origin + "/oidc/v1/end_session",
		"device_authorization_endpoint":         origin + "/oauth/v2/device_authorization",
		"jwks_uri":                              origin + "/oauth/v2/keys",
		"scopes_supported":                      []string{"openid", "profile", "email", "offline_access"},
		"response_types_supported":              []string{"code"},
		"response_modes_supported":              []string{"query"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{AlgRS256, AlgES256},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "none"},
		"claims_supported":                      []string{"sub", "aud", "exp", "iat", "iss", "client_id", "jti"},
		"code_challenge_methods_supported":      []string{"S256"},
		"request_parameter_supported":           false,
		"request_uri_parameter_supported":       false,
	}
}
