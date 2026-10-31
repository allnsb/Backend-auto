package tokenauth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"

	"odysseia.local/backend/internal/identity"
)

var ErrInvalidToken = errors.New("invalid access token")

type Config struct {
	Issuer   string
	Audience string
	JWKSURL  string
	CAFile   string
}

type Verifier struct {
	keys   keyfunc.Keyfunc
	parser *jwt.Parser
}

func New(ctx context.Context, cfg Config) (*Verifier, error) {
	if err := validateHTTPSURL(cfg.Issuer); err != nil {
		return nil, fmt.Errorf("auth issuer: %w", err)
	}
	if err := validateHTTPSURL(cfg.JWKSURL); err != nil {
		return nil, fmt.Errorf("auth JWKS URL: %w", err)
	}
	if cfg.Audience == "" {
		return nil, errors.New("auth audience is required")
	}
	client, err := newHTTPClient(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	ignoreInitialFetchError := false
	remote, err := keyfunc.NewDefaultOverrideCtx(ctx, []string{cfg.JWKSURL}, keyfunc.Override{
		Client:                    client,
		HTTPTimeout:               5 * time.Second,
		RefreshInterval:           time.Minute,
		RateLimitWaitMax:          time.Second,
		NoErrorReturnFirstHTTPReq: &ignoreInitialFetchError,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize trusted JWKS: %w", err)
	}
	keys, err := keyfunc.New(keyfunc.Options{
		Ctx:          ctx,
		Storage:      remote.Storage(),
		UseWhitelist: []jwkset.USE{jwkset.UseSig},
	})
	if err != nil {
		return nil, fmt.Errorf("configure signing keys: %w", err)
	}
	return &Verifier{
		keys: keys,
		parser: jwt.NewParser(
			jwt.WithValidMethods([]string{"RS256", "ES256", "EdDSA"}),
			jwt.WithIssuer(cfg.Issuer), jwt.WithAudience(cfg.Audience),
			jwt.WithExpirationRequired(), jwt.WithIssuedAt(),
			jwt.WithStrictDecoding(),
		),
	}, nil
}

func (v *Verifier) Verify(ctx context.Context, raw string) (identity.ExternalID, error) {
	auth, err := v.Authenticate(ctx, raw)
	return auth.ExternalID, err
}

func (v *Verifier) Authenticate(ctx context.Context, raw string) (identity.Authentication, error) {
	if raw == "" || len(raw) > 16*1024 {
		return identity.Authentication{}, ErrInvalidToken
	}
	claims := new(accessClaims)
	_, err := v.parser.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		typ, _ := token.Header["typ"].(string)
		if typ != "at+jwt" && typ != "application/at+jwt" {
			return nil, ErrInvalidToken
		}
		kid, _ := token.Header["kid"].(string)
		if kid == "" {
			return nil, ErrInvalidToken
		}
		if _, exists := token.Header["crit"]; exists {
			return nil, ErrInvalidToken
		}
		return v.keys.KeyfuncCtx(ctx)(token)
	})
	if err != nil {
		return identity.Authentication{}, ErrInvalidToken
	}
	return identity.Authentication{ExternalID: identity.ExternalID{Issuer: claims.Issuer, Subject: claims.Subject}, ExpiresAt: claims.ExpiresAt.Time}, nil
}

type accessClaims struct {
	jwt.RegisteredClaims
	ClientID string `json:"client_id"`
}

func (c accessClaims) Validate() error {
	if c.Subject == "" || c.ID == "" || c.ClientID == "" {
		return ErrInvalidToken
	}
	if c.IssuedAt == nil || c.ExpiresAt == nil {
		return ErrInvalidToken
	}
	if !c.ExpiresAt.After(c.IssuedAt.Time) {
		return ErrInvalidToken
	}
	return nil
}

func validateHTTPSURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return errors.New("an absolute HTTPS URL is required")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("URL credentials, query and fragment are not supported")
	}
	return nil
}

func newHTTPClient(caFile string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13}
	if caFile != "" {
		bundle, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read auth CA bundle: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(bundle) {
			return nil, errors.New("auth CA bundle contains no certificates")
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	return &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("JWKS redirects are not allowed")
		},
	}, nil
}
