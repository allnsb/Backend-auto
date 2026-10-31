package conversation

import (
	"testing"

	"github.com/google/uuid"
)

func TestDirectPairAndReadCursor(t *testing.T) {
	low := "00000000-0000-4000-8000-000000000010"
	high := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	gotLow, gotHigh, err := OrderPair(high, low)
	if err != nil || gotLow != low || gotHigh != high {
		t.Fatalf("pair %s %s err=%v", gotLow, gotHigh, err)
	}
	if _, _, err := OrderPair(low, low); err != ErrInvalid {
		t.Fatalf("self pair err=%v", err)
	}
	if InboxChannel(low) != "inbox:#"+low {
		t.Fatal("inbox channel")
	}
	next, advanced, err := NextRead(4, 2, 9)
	if err != nil || advanced || next != 4 {
		t.Fatalf("regression next=%d advanced=%v err=%v", next, advanced, err)
	}
	next, advanced, err = NextRead(4, 4, 9)
	if err != nil || advanced || next != 4 {
		t.Fatalf("repeat next=%d advanced=%v err=%v", next, advanced, err)
	}
	next, advanced, err = NextRead(4, 6, 9)
	if err != nil || !advanced || next != 6 {
		t.Fatalf("advance next=%d advanced=%v err=%v", next, advanced, err)
	}
	if _, _, err := NextRead(4, 10, 9); err != ErrInvalid {
		t.Fatalf("past the end err=%v", err)
	}
}

func TestGroupDraftNormalizesMembers(t *testing.T) {
	actor := uuid.MustParse("11111111-1111-4111-8111-111111111111").String()
	second := uuid.MustParse("22222222-2222-4222-8222-222222222222").String()
	third := uuid.MustParse("33333333-3333-4333-8333-333333333333").String()
	draft := GroupDraft{Title: " 小组 ", MemberIDs: []string{third, second}}
	if err := draft.Normalize(actor, MaxMembers); err != nil {
		t.Fatal(err)
	}
	if draft.MemberIDs[0] != second || draft.MemberIDs[1] != third {
		t.Fatalf("order = %v", draft.MemberIDs)
	}
	if err := (&GroupDraft{Title: "x", MemberIDs: []string{second, second}}).Normalize(actor, MaxMembers); err != ErrInvalid {
		t.Fatal("duplicate members accepted")
	}
	if err := (&GroupDraft{Title: "x", MemberIDs: []string{actor}}).Normalize(actor, MaxMembers); err != ErrInvalid {
		t.Fatal("owner listed as a member")
	}
	if err := (&GroupDraft{Title: "x", MemberIDs: []string{second, third}}).Normalize(actor, 2); err != ErrInvalid {
		t.Fatal("limit counts the owner")
	}
}
