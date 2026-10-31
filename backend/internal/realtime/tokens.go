package realtime

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"odysseia.local/backend/internal/identity"
)

const TokenTTL = time.Minute

var ErrExpired = errors.New("authentication expired before realtime token issuance")

type Token struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

type Signer struct {
	connectionKey []byte
	ttl           time.Duration
}

func NewSigner(connectionSecret string, ttl time.Duration) (*Signer, error) {
	if len(connectionSecret) < 32 {
		return nil, errors.New("realtime signing secret must contain at least 32 bytes")
	}
	if ttl < time.Second || ttl > TokenTTL {
		return nil, errors.New("realtime token lifetime must be between one second and one minute")
	}
	return &Signer{connectionKey: []byte(connectionSecret), ttl: ttl}, nil
}

// Connection delegates identity only and never outlives the source authentication.
// Channel access is deliberately absent so a JWT cannot bypass the subscribe proxy.
func (s *Signer) Connection(actor identity.Actor) (Token, error) {
	now := time.Now()
	expires := now.Add(s.ttl)
	if actor.ExpiresAt.Before(expires) {
		expires = actor.ExpiresAt
	}
	if actor.ID == "" || expires.Unix() <= now.Unix() {
		return Token{}, ErrExpired
	}
	claims := jwt.RegisteredClaims{Subject: actor.ID, IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(expires)}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.connectionKey)
	return Token{Token: raw, ExpiresAt: expires.Unix()}, err
}
