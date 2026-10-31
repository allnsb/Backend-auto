package forum

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"odysseia.local/backend/internal/message"
)

var (
	ErrNotFound    = errors.New("forum resource is unavailable to this user")
	ErrInvalid     = errors.New("invalid forum input")
	ErrReference   = errors.New("invalid same-topic reference or root")
	ErrConflict    = errors.New("idempotency key was already used with different input")
	ErrExists      = errors.New("forum member already exists")
	ErrForbidden   = errors.New("caller lacks the required grant")
	ErrLastManager = errors.New("forum would have no member manager")
	ErrBusy        = errors.New("forum write capacity is temporarily exhausted")
)

const (
	MaxTitleRunes = 200
	MaxBodyBytes  = 32 * 1024
	MaxPageSize   = 100
)

type Forum struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type Topic struct {
	ID        string    `json:"id"`
	ForumID   string    `json:"forum_id"`
	Position  int64     `json:"position,string"`
	AuthorID  string    `json:"author_id"`
	Title     string    `json:"title"`
	Mode      string    `json:"mode"`
	CreatedAt time.Time `json:"created_at"`
}

// RootID describes forum layout; it is deliberately separate from Message.QuoteID.
type Entry struct {
	message.Message
	RootID string `json:"root_id,omitempty"`
}

type CreatedTopic struct {
	Topic          Topic `json:"topic"`
	OpeningMessage Entry `json:"opening_message"`
}

type TopicDraft struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Mode  string `json:"mode"`
}

// Mode chooses the creation-time default layout. Both layouts preserve root
// ownership separately from quotes and share one canonical message history.
func (d TopicDraft) Validate() error {
	if err := ValidateTitle(d.Title); err != nil {
		return err
	}
	if d.Mode != "threaded" && d.Mode != "chat" {
		return ErrInvalid
	}
	return validateBody(d.Body)
}

func ValidateTitle(title string) error {
	if !utf8.ValidString(title) || strings.ContainsRune(title, 0) {
		return ErrInvalid
	}
	if strings.TrimSpace(title) == "" || utf8.RuneCountInString(title) > MaxTitleRunes {
		return ErrInvalid
	}
	return nil
}

type ReplyDraft struct {
	Body    string `json:"body"`
	RootID  string `json:"root_id,omitempty"`
	QuoteID string `json:"quote_id,omitempty"`
}

func (d ReplyDraft) Validate() error {
	return validateBody(d.Body)
}

// References contains only messages already scoped to the current topic by storage.
func (d ReplyDraft) ValidateReferences(references map[string]Entry) error {
	if d.RootID != "" {
		root, found := references[d.RootID]
		if !found || root.RootID != "" {
			return ErrReference
		}
	}
	if d.QuoteID != "" {
		if _, found := references[d.QuoteID]; !found {
			return ErrReference
		}
	}
	return nil
}

func validateBody(body string) error {
	if !utf8.ValidString(body) || strings.ContainsRune(body, 0) {
		return ErrInvalid
	}
	if strings.TrimSpace(body) == "" {
		return ErrInvalid
	}
	if len(body) > MaxBodyBytes {
		return ErrInvalid
	}
	return nil
}
