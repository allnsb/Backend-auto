// Package conversation is the text direct-message and group slice.
// Message bodies stay in the shared message stream. This package does not
// grant forum roles, deliver files, or publish read receipts to other members.
package conversation

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"odysseia.local/backend/internal/message"
)

var (
	ErrNotFound    = errors.New("conversation is unavailable to this user")
	ErrInvalid     = errors.New("invalid conversation input")
	ErrReference   = errors.New("invalid same-conversation reference")
	ErrConflict    = errors.New("idempotency key was already used with different input")
	ErrForbidden   = errors.New("caller cannot manage this conversation")
	ErrLastManager = errors.New("group owner must transfer or dissolve")
	ErrBusy        = errors.New("conversation write capacity is temporarily exhausted")
)

const (
	MaxTitleRunes = 200
	MaxBodyBytes  = 32 * 1024
	MaxPageSize   = 100
	// MaxMembers is the database ceiling. Configuration may only lower it.
	MaxMembers = 50
)

// InboxChannel is the Centrifugo user-limited channel for one local user.
// The connection JWT subject is that user's canonical UUID.
func InboxChannel(userID string) string { return "inbox:#" + userID }

type Member struct {
	UserID   string    `json:"user_id"`
	Role     string    `json:"role"`
	JoinedAt time.Time `json:"joined_at"`
}

// Conversation is one caller's view. Unread is private; it is not a receipt.
type Conversation struct {
	ID               string    `json:"id"`
	Kind             string    `json:"kind"`
	Title            string    `json:"title,omitempty"`
	OwnerID          string    `json:"owner_id,omitempty"`
	CreatedBy        string    `json:"created_by"`
	CreatedAt        time.Time `json:"created_at"`
	LastPosition     int64     `json:"last_position,string"`
	LastReadPosition int64     `json:"last_read_position,string"`
	Unread           int64     `json:"unread,string"`
	ActivityAt       time.Time `json:"activity_at"`
	Members          []Member  `json:"members,omitempty"`
}

type DMDraft struct {
	UserID string `json:"user_id"`
}

func (d DMDraft) Validate(actor string) error {
	if err := validID(actor); err != nil || validID(d.UserID) != nil {
		return ErrInvalid
	}
	if actor == d.UserID {
		return ErrInvalid
	}
	return nil
}

type GroupDraft struct {
	Title     string   `json:"title"`
	MemberIDs []string `json:"member_ids"`
}

// Normalize sorts member ids so request order is not part of the idempotency hash.
func (d *GroupDraft) Normalize(actor string, limit int) error {
	if err := validTitle(d.Title); err != nil || validID(actor) != nil {
		return ErrInvalid
	}
	if limit < 1 || limit > MaxMembers {
		return ErrInvalid
	}
	if d.MemberIDs == nil {
		d.MemberIDs = []string{}
	}
	if len(d.MemberIDs)+1 > limit {
		return ErrInvalid
	}
	for index, id := range d.MemberIDs {
		if err := validID(id); err != nil || id == actor {
			return ErrInvalid
		}
		d.MemberIDs[index] = id
	}
	slices.Sort(d.MemberIDs)
	if len(slices.Compact(append([]string(nil), d.MemberIDs...))) != len(d.MemberIDs) {
		return ErrInvalid
	}
	return nil
}

type MessageDraft struct {
	Body    string `json:"body"`
	QuoteID string `json:"quote_id,omitempty"`
}

func (d MessageDraft) Validate() error {
	if d.QuoteID != "" && validID(d.QuoteID) != nil {
		return ErrInvalid
	}
	return validBody(d.Body)
}

type ReadState struct {
	Position int64 `json:"position,string"`
	Unread   int64 `json:"unread,string"`
}

type Unread struct {
	ConversationID string `json:"conversation_id"`
	Unread         int64  `json:"unread,string"`
}

type UnreadSummary struct {
	Items []Unread `json:"items"`
	Total int64    `json:"total,string"`
}

// NextRead moves a private cursor forward only. A repeated or older position
// reports the cursor unchanged and tells the caller not to emit an event.
func NextRead(current, requested, last int64) (int64, bool, error) {
	if requested < 0 || requested > last {
		return 0, false, ErrInvalid
	}
	if requested <= current {
		return current, false, nil
	}
	return requested, true, nil
}

// OrderPair returns the canonical direct-message pair. UUID order matches
// PostgreSQL uuid comparison, which is unsigned byte order.
func OrderPair(left, right string) (string, string, error) {
	if err := validID(left); err != nil || validID(right) != nil || left == right {
		return "", "", ErrInvalid
	}
	a, err := uuid.Parse(left)
	if err != nil {
		return "", "", ErrInvalid
	}
	b, err := uuid.Parse(right)
	if err != nil {
		return "", "", ErrInvalid
	}
	if bytes.Compare(a[:], b[:]) > 0 {
		a, b = b, a
	}
	return a.String(), b.String(), nil
}

func validID(value string) error {
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil || id.String() != value {
		return ErrInvalid
	}
	return nil
}

func validTitle(title string) error {
	if !utf8.ValidString(title) || strings.ContainsRune(title, 0) {
		return ErrInvalid
	}
	if strings.TrimSpace(title) == "" || utf8.RuneCountInString(title) > MaxTitleRunes {
		return ErrInvalid
	}
	return nil
}

func validBody(body string) error {
	if !utf8.ValidString(body) || strings.ContainsRune(body, 0) {
		return ErrInvalid
	}
	if strings.TrimSpace(body) == "" || len(body) > MaxBodyBytes {
		return ErrInvalid
	}
	return nil
}

// Message is the canonical text row. Conversations do not add a second body.
type Message = message.Message
