// Package work owns private work drafts. It does not grant forum membership,
// publish content or depend on a database, transport or object store.
package work

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxTitleRunes        = 200
	MaxIntroductionBytes = 32 * 1024
	MaxPageSize          = 100
)

var (
	ErrInvalid  = errors.New("invalid work draft")
	ErrNotFound = errors.New("work draft is unavailable to this author")
	ErrConflict = errors.New("work creation key was used with different input")
	ErrRevision = errors.New("work draft revision has changed")
)

type Draft struct {
	Title        string `json:"title"`
	Introduction string `json:"introduction"`
}

func (d Draft) Validate() error {
	if !utf8.ValidString(d.Title) || strings.TrimSpace(d.Title) != d.Title || d.Title == "" || utf8.RuneCountInString(d.Title) > MaxTitleRunes {
		return ErrInvalid
	}
	if !utf8.ValidString(d.Introduction) || len(d.Introduction) > MaxIntroductionBytes || strings.ContainsRune(d.Title, '\x00') || strings.ContainsRune(d.Introduction, '\x00') {
		return ErrInvalid
	}
	return nil
}

type Update struct {
	Draft
	Revision int64 `json:"revision,string"`
}

func (u Update) Validate() error {
	if u.Revision < 1 {
		return ErrInvalid
	}
	return u.Draft.Validate()
}

// Revision prevents lost updates; it is not a published resource version or an
// edit-history archive. This slice exposes only the author's current draft.
type Work struct {
	ID       string `json:"id"`
	AuthorID string `json:"author_id"`
	Status   string `json:"status"`
	Draft
	Revision  int64     `json:"revision,string"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
