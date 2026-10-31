// Package emoji is the custom-emoji rule set. It does not talk to a database
// or an object store. Storage decides who may manage a catalog; this package
// decides names, file shape, message tokens, and how a deleted emoji reads.
package emoji

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
)

const (
	// MaxPerMessage bounds one write so a body cannot force an unbounded lookup.
	MaxPerMessage = 50
	// MaxSiteLive and MaxForumLive are catalog caps for live rows.
	MaxSiteLive  = 200
	MaxForumLive = 50
	// MaxBytesCeiling and MaxEdgeCeiling are the database ceilings. Configuration
	// may only lower them. Discord's upload cap is 256KiB and the recommended
	// edge is 128; 256 is the largest edge this slice accepts.
	MaxBytesCeiling int64 = 256 * 1024
	MaxEdgeCeiling        = 256
)

var (
	ErrInvalid     = errors.New("invalid emoji")
	ErrUnavailable = errors.New("emoji reference is not usable")
	ErrForbidden   = errors.New("emoji permission denied")
	ErrNotFound    = errors.New("emoji is not available")
	ErrNameTaken   = errors.New("emoji name is taken")
	ErrConflict    = errors.New("emoji idempotency key was reused")
	ErrLimit       = errors.New("emoji catalog is full")
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_]{2,32}$`)

// Limits is the process allowlist. Zero values are not implicit defaults.
type Limits struct {
	MaxBytes int64
	MaxEdge  int
}

func DefaultLimits() Limits {
	return Limits{MaxBytes: MaxBytesCeiling, MaxEdge: 128}
}

func (l Limits) Valid() error {
	if l.MaxBytes < 1 || l.MaxBytes > MaxBytesCeiling {
		return errors.New("emoji byte limit must be from 1 to 262144")
	}
	if l.MaxEdge < 1 || l.MaxEdge > MaxEdgeCeiling {
		return errors.New("emoji edge limit must be from 1 to 256")
	}
	return nil
}

// ValidName is the shared catalog and token name rule.
func ValidName(name string) error {
	if !namePattern.MatchString(name) {
		return ErrInvalid
	}
	return nil
}

// Image is the sanitized file that will be stored.
type Image struct {
	ContentType string
	Bytes       []byte
	Width       int
	Height      int
	Animated    bool
	Sum         [32]byte
	Key         string
}

// ObjectKey is emoji/v1/<hh>/<sha256>.<ext>. The prefix is not conversations/,
// so the attachment lifecycle rule does not cover these objects.
func ObjectKey(sum [32]byte, contentType string) string {
	ext := extension(contentType)
	if ext == "" {
		return ""
	}
	encoded := hex.EncodeToString(sum[:])
	return "emoji/v1/" + encoded[:2] + "/" + encoded + "." + ext
}

func extension(contentType string) string {
	switch contentType {
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	default:
		return ""
	}
}

// RequestHash binds an upload to its scope, name, and stored bytes.
func RequestHash(scope, forumID, name string, sum [32]byte) [32]byte {
	hash := sha256.New()
	_, _ = hash.Write([]byte(scope))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(forumID))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(name))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(sum[:])
	var out [32]byte
	copy(out[:], hash.Sum(nil))
	return out
}

// Emoji is one live catalog row plus its public URL.
type Emoji struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Animated    bool   `json:"animated"`
	Scope       string `json:"scope"`
	ForumID     string `json:"forum_id,omitempty"`
	ContentType string `json:"content_type"`
	URL         string `json:"url"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Bytes       int    `json:"bytes"`
}

// Page is a catalog list. Items is empty, not null, when nothing matches.
type Page struct {
	Items []Emoji `json:"items"`
}

// Ref is the message metadata for one still-available token.
type Ref struct {
	ID          string
	Name        string
	Animated    bool
	Scope       string
	ForumID     string
	ContentType string
	URL         string
	Width       int
	Height      int
	Bytes       int
}

// Check confirms a prepared image still matches the process limits and its hash.
func (image Image) Check(limits Limits) error {
	if err := limits.Valid(); err != nil {
		return err
	}
	if len(image.Bytes) == 0 || int64(len(image.Bytes)) > limits.MaxBytes {
		return ErrInvalid
	}
	if image.Width < 1 || image.Height < 1 || image.Width > limits.MaxEdge || image.Height > limits.MaxEdge {
		return ErrInvalid
	}
	if sha256.Sum256(image.Bytes) != image.Sum || ObjectKey(image.Sum, image.ContentType) != image.Key {
		return ErrInvalid
	}
	switch image.ContentType {
	case "image/png", "image/gif", "image/webp":
		return nil
	default:
		return ErrInvalid
	}
}

func (e Emoji) Ref() Ref {
	return Ref{
		ID: e.ID, Name: e.Name, Animated: e.Animated, Scope: e.Scope, ForumID: e.ForumID,
		ContentType: e.ContentType, URL: e.URL, Width: e.Width, Height: e.Height, Bytes: e.Bytes,
	}
}
