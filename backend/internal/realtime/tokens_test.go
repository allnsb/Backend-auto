package realtime

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"odysseia.local/backend/internal/identity"
)

func TestConnectionTokenCannotDelegatePastOriginalExpiryOrGrantAChannel(t *testing.T) {
	key := strings.Repeat("c", 32)
	signer, err := NewSigner(key, TokenTTL)
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(10 * time.Second).Truncate(time.Second)
	actor := identity.Actor{User: identity.User{ID: "local-user"}, ExpiresAt: expiry}
	connection, err := signer.Connection(actor)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jwt.Parse(connection.Token, func(*jwt.Token) (any, error) { return []byte(key), nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil {
		t.Fatal(err)
	}
	claims := parsed.Claims.(jwt.MapClaims)
	if claims["sub"] != actor.ID || claims["channel"] != nil || claims["channels"] != nil || claims["subs"] != nil {
		t.Fatal("connection identity must not grant channel access")
	}
	if connection.ExpiresAt != expiry.Unix() {
		t.Fatal("delegation extended the original authentication lifetime")
	}
	actor.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := signer.Connection(actor); !errors.Is(err, ErrExpired) {
		t.Fatal("expired authentication minted a realtime token")
	}
	if _, err := NewSigner("short", TokenTTL); err == nil {
		t.Fatal("weak signing secret accepted")
	}
}
