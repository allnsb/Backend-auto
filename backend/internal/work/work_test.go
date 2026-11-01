package work

import (
	"errors"
	"strings"
	"testing"
)

func TestDraftInputBoundaries(t *testing.T) {
	for _, draft := range []Draft{
		{Title: ""}, {Title: " padded "}, {Title: strings.Repeat("字", 201)},
		{Title: "bad\x00title"}, {Title: "ok", Introduction: "bad\x00body"},
		{Title: "ok", Introduction: strings.Repeat("字", MaxIntroductionBytes/3+1)},
		{Title: string([]byte{0xff})},
	} {
		if !errors.Is(draft.Validate(), ErrInvalid) {
			t.Fatalf("accepted invalid draft: title bytes=%d intro bytes=%d", len(draft.Title), len(draft.Introduction))
		}
	}
	if err := (Draft{Title: strings.Repeat("字", 200), Introduction: strings.Repeat("a", MaxIntroductionBytes)}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Update{Draft: Draft{Title: "valid"}, Revision: 0}).Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatal("zero revision accepted")
	}
}
