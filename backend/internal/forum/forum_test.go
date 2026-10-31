package forum

import (
	"errors"
	"strings"
	"testing"

	"odysseia.local/backend/internal/message"
)

func TestReplyLayoutIsIndependentFromQuote(t *testing.T) {
	references := map[string]Entry{
		"root":  {Message: message.Message{ID: "root"}},
		"child": {Message: message.Message{ID: "child"}, RootID: "root"},
	}
	valid := ReplyDraft{Body: "quote a child without creating another level", RootID: "root", QuoteID: "child"}
	if err := valid.ValidateReferences(references); err != nil {
		t.Fatal(err)
	}
	for _, draft := range []ReplyDraft{
		{Body: "third level", RootID: "child"},
		{Body: "unknown root", RootID: "other-topic"},
		{Body: "unknown quote", QuoteID: "other-topic"},
	} {
		if err := draft.ValidateReferences(references); !errors.Is(err, ErrReference) {
			t.Fatalf("invalid references accepted: %+v", draft)
		}
	}
}

func TestTextBoundaryRejectsEmptyOversizedAndDatabaseInvalidText(t *testing.T) {
	for _, body := range []string{"  \n\t", strings.Repeat("a", MaxBodyBytes+1), "contains\x00nul", string([]byte{0xff})} {
		if err := (ReplyDraft{Body: body}).Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid message text accepted")
		}
	}
	valid := TopicDraft{Title: strings.Repeat("帖", MaxTitleRunes), Body: "普通论坛不需要作品", Mode: "threaded"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	valid.Title += "帖"
	if err := valid.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatal("oversized Unicode title accepted")
	}
}
