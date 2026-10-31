package identity

import (
	"errors"
	"time"
)

var ErrNotFound = errors.New("local user is not provisioned")

// ExternalID is established by a trusted token verifier, never by request fields.
type ExternalID struct {
	Issuer  string
	Subject string
}

type User struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

// Authentication carries the authority lifetime separately from the stable external ID.
type Authentication struct {
	ExternalID ExternalID
	ExpiresAt  time.Time
}

type Actor struct {
	User
	ExpiresAt time.Time
}
