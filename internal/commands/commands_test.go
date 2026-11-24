package commands

import (
	"context"
	"errors"
	"testing"

	"github.com/ziliding/hermes-roomd/internal/roster"
	"github.com/ziliding/hermes-roomd/internal/transcript"
)

func TestParse(t *testing.T) {
	tests := []struct {
		in   string
		want *Command
	}{
		{"/reset", &Command{Kind: Reset}},
		{"  /RESET  ", &Command{Kind: Reset}},
		{"!reset", &Command{Kind: Reset}},
		{"/reset now please", &Command{Kind: Reset}},
		{"/explain", &Command{Kind: Explain}},
		{"/explain Cleo", &Command{Kind: Explain, Arg: "Cleo"}},
		{"/Explain   @cleo:z ", &Command{Kind: Explain, Arg: "@cleo:z"}},
		{"/explain\tCleo", &Command{Kind: Explain, Arg: "Cleo"}},
		{"!continue", &Command{Kind: Continue}},
		{"/Continue 5 ", &Command{Kind: Continue, Arg: "5"}},
		{"/continuing", nil},
		{"/resetting", nil},
		{"/shrug", nil},
		{"reset", nil},
		{"please /reset", nil},
		{"/", nil},
		{"", nil},
	}
	for _, tt := range tests {
		got, ok := Parse(tt.in)
		if (tt.want != nil) != ok || (ok && got != *tt.want) {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", tt.in, got, ok, tt.want)
		}
	}
}

func TestContinueTurns(t *testing.T) {
	tests := []struct {
		arg     string
		want    int
		wantErr bool
	}{
		{"", 0, false},
		{"1", 1, false},
		{"4 more please", 4, false},
		{"20", MaxContinue, false},
		{"21", 0, true},
		{"0", 0, true},
		{"-2", 0, true},
		{"more", 0, true},
	}
	for _, tt := range tests {
		got, err := ContinueTurns(Command{Kind: Continue, Arg: tt.arg})
		if got != tt.want || (err != nil) != tt.wantErr {
			t.Errorf("ContinueTurns(%q) = %d, %v; want %d, err %v", tt.arg, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestExplainTarget(t *testing.T) {
	r := roster.New("@alex:z", "Alex", []roster.Person{
		{Key: "ada", Display: "Ada", MXID: "@ada:z"},
		{Key: "cleo", Display: "Cleo", MXID: "@cleo:z"},
		{Key: "bram", Display: "Bram", MXID: "@bram_bot:z"},
	})
	pill := `<a href="https://matrix.to/#/@bram_bot:z">Bram</a>`
	tests := []struct {
		name    string
		cmd     Command
		last    string
		want    string
		wantErr error
	}{
		{"no arg: last speaker", Command{Kind: Explain}, "@cleo:z", "cleo", nil},
		{"no arg, nobody spoke", Command{Kind: Explain}, "", "", ErrNoTarget},
		{"no arg, last speaker not a bot", Command{Kind: Explain}, "@alex:z", "", ErrNoTarget},
		{"display name", Command{Kind: Explain, Arg: "ada"}, "@cleo:z", "ada", nil},
		{"@handle", Command{Kind: Explain, Arg: "@Ada"}, "", "ada", nil},
		{"mxid", Command{Kind: Explain, Arg: "@bram_bot:z"}, "", "bram", nil},
		{"localpart", Command{Kind: Explain, Arg: "bram_bot"}, "", "bram", nil},
		{"key", Command{Kind: Explain, Arg: "bram"}, "", "bram", nil},
		{"pill", Command{Kind: Explain, Arg: "Bram", FormattedBody: "/explain " + pill}, "", "bram", nil},
		{"name in a phrase", Command{Kind: Explain, Arg: "Cleo please"}, "@ada:z", "cleo", nil},
		{"unknown", Command{Kind: Explain, Arg: "Dex"}, "@ada:z", "", ErrUnknownBot},
		{"the user is not a bot", Command{Kind: Explain, Arg: "Alex"}, "@ada:z", "", ErrUnknownBot},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExplainTarget(r, tt.cmd, tt.last)
			if got != tt.want || !errors.Is(err, tt.wantErr) {
				t.Fatalf("got %q, %v; want %q, %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestRunReset(t *testing.T) {
	ctx := context.Background()
	s, err := transcript.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const room = "!r:z"
	for i, ev := range []string{"$a", "$b", "$c"} {
		if _, err := s.Append(ctx, transcript.Entry{RoomID: room, EventID: ev, Sender: "@alex:z", Body: "m", TS: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	// ada has been called once (gen 1 row exists, cursor at 1); cleo never.
	if _, err := s.Generation(ctx, room, "ada"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCursor(ctx, room, "ada", 1); err != nil {
		t.Fatal(err)
	}
	upTo, _ := s.LatestSeq(ctx, room)
	bots := []string{"ada", "cleo"}
	for round, wantGen := range []int{2, 3} {
		if err := RunReset(ctx, s, room, bots, upTo); err != nil {
			t.Fatal(err)
		}
		for _, b := range bots {
			gen, _ := s.Generation(ctx, room, b)
			cur, _ := s.Cursor(ctx, room, b)
			if gen != wantGen || cur != upTo {
				t.Errorf("round %d %s: gen=%d cursor=%d, want gen=%d cursor=%d", round, b, gen, cur, wantGen, upTo)
			}
		}
	}
	// A cursor never moves backwards.
	if err := RunReset(ctx, s, room, bots, 1); err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.Cursor(ctx, room, "cleo"); cur != upTo {
		t.Errorf("cursor moved back to %d", cur)
	}
	// Other rooms are untouched.
	if gen, _ := s.Generation(ctx, "!other:z", "ada"); gen != 1 {
		t.Errorf("other room gen %d", gen)
	}
}
