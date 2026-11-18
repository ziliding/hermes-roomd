package transcript

import (
	"context"
	"fmt"
	"testing"
)

func TestSteeredMarksArePrunedByTheCursor(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 1; i <= 5; i++ {
		if _, err := s.Append(ctx, Entry{RoomID: "!r:z", EventID: fmt.Sprintf("$%d", i), Sender: "@alex:z", Body: "m", TS: 1}); err != nil {
			t.Fatal(err)
		}
	}
	for _, seq := range []int64{3, 5} {
		if err := s.MarkSteered(ctx, "!r:z", "ada", seq); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkSteered(ctx, "!r:z", "ada", 3); err != nil { // idempotent
		t.Fatal(err)
	}
	got, err := s.Steered(ctx, "!r:z", "ada", 0)
	if err != nil || len(got) != 2 || !got[3] || !got[5] {
		t.Fatalf("steered %v %v", got, err)
	}
	if other, _ := s.Steered(ctx, "!r:z", "cleo", 0); len(other) != 0 {
		t.Fatalf("marks leak to another bot: %v", other)
	}
	if err := s.SetCursor(ctx, "!r:z", "ada", 4); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Steered(ctx, "!r:z", "ada", 0); len(got) != 1 || !got[5] {
		t.Fatalf("after cursor 4: %v", got)
	}
}
