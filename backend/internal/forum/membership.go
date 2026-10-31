package forum

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// MemberRemoval is an operator command, independent of HTTP and the realtime provider.
type MemberRemoval struct {
	OperationID string `json:"operation_id"`
	ForumID     string `json:"forum_id"`
	UserID      string `json:"user_id"`
	Operator    string `json:"operator"`
	Reason      string `json:"reason"`
}

func (c MemberRemoval) Validate() error {
	for _, value := range []string{c.OperationID, c.ForumID, c.UserID} {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil || id.String() != value {
			return ErrInvalid
		}
	}
	for _, value := range []string{c.Operator, c.Reason} {
		if !utf8.ValidString(value) || strings.TrimSpace(value) == "" || strings.ContainsRune(value, 0) {
			return ErrInvalid
		}
	}
	if utf8.RuneCountInString(c.Operator) > 200 || utf8.RuneCountInString(c.Reason) > 500 {
		return ErrInvalid
	}
	return nil
}

type RemovedMember struct {
	MemberRemoval
	Removed   bool      `json:"removed"`
	CreatedAt time.Time `json:"created_at"`
}
