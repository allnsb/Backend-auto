package forum

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Member is the role stored on one forum membership. Priority and RoleName
// come from that stored role. Catalog groups are not folded in, so a catalog
// owner can still appear here as member at priority 100.
type Member struct {
	UserID   string `json:"user_id"`
	RoleID   string `json:"role_id"`
	RoleName string `json:"role_name"`
	Priority int    `json:"priority"`
}

// MemberChange assigns a forum role. An empty RoleID selects the built-in member role.
type MemberChange struct {
	UserID string `json:"user_id"`
	RoleID string `json:"role_id"`
}

func (c MemberChange) Validate() error {
	if err := ValidID(c.UserID); err != nil {
		return err
	}
	if c.RoleID == "" {
		return nil
	}
	return ValidID(c.RoleID)
}

// ValidID accepts the canonical textual form of a non-nil UUID.
func ValidID(value string) error {
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil || id.String() != value {
		return ErrInvalid
	}
	return nil
}

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
