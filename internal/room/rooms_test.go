package room

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/ziliding/hermes-roomd/internal/matrixio"
	"github.com/ziliding/hermes-roomd/internal/roster"
	"github.com/ziliding/hermes-roomd/internal/transcript"
	"github.com/ziliding/hermes-roomd/internal/turns"
)

const roomB = "!b:z"

var allPeople = map[string]roster.Person{
	"ada":  {Key: "ada", Display: "Ada", MXID: "@ada:z"},
	"cleo": {Key: "cleo", Display: "Cleo", MXID: "@cleo:z"},
	"bram": {Key: "bram", Display: "Bram", MXID: "@bram:z"},
}

// newRoomsHarness has two rooms with different casts and settings:
//   - roomID ("lounge"): ada + cleo, listener cleo, max 3 bot turns;
//   - roomB ("library"): ada + bram, listener bram, max 1 bot turn.
func newRoomsHarness(t *testing.T) *harness {
	t.Helper()
	store, err := transcript.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	h := &harness{t: t, reply: func(bot string, n int) string { return "reply " + bot }}
	spec := func(id, prefix string, cast []string, listener string, maxTurns int) RoomSpec {
		var people []roster.Person
		var bots []*Bot
		for _, k := range cast {
			people = append(people, allPeople[k])
			bots = append(bots, &Bot{Key: k, Poster: fakePoster{h, k}, Hermes: fakeHermes{h, k}, System: prefix + "-SYS-" + k})
		}
		return RoomSpec{
			ID: id, Roster: roster.New("@alex:z", "Alex", people), Bots: bots, Notifier: listener,
			Decider: turns.NewRules(cast, maxTurns, rand.New(rand.NewPCG(1, 1))),
		}
	}
	h.o = New(Options{
		Store: store, MaxDeltaLines: 40, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		RoomSpecs: []RoomSpec{
			spec(roomID, "LOUNGE", []string{"ada", "cleo"}, "cleo", 3),
			spec(roomB, "LIBRARY", []string{"ada", "bram"}, "bram", 1),
		},
	})
	return h
}

func (h *harness) say(room, body string) matrixio.Message {
	h.seq++
	return matrixio.Message{RoomID: room, EventID: fmt.Sprintf("$m%d", h.seq), Sender: "@alex:z", Body: body, TS: time.Now().UnixMilli()}
}

func (h *harness) runIn(room, body string) {
	ctx := context.Background()
	h.o.Handle(ctx, h.say(room, body))
	w := h.o.rooms[room]
	q := <-w.inbox
	if q.cmd != nil {
		h.o.command(ctx, w, q)
		return
	}
	w.state.UserSpoke()
	h.o.chain(ctx, w, q)
}

func TestPerRoomCastTurnsAndRules(t *testing.T) {
	h := newRoomsHarness(t)
	h.runIn(roomID, "hello everyone")
	if len(h.posts) != 3 {
		t.Fatalf("lounge posts %v, want 3 (its max_bot_turns)", h.posts)
	}
	for _, c := range h.calls {
		if c.bot == "bram" {
			t.Fatal("bram spoke in the lounge, where she is not in the cast")
		}
		if c.system != "LOUNGE-SYS-"+c.bot {
			t.Fatalf("lounge call used system %q", c.system)
		}
	}

	h.posts, h.calls = nil, nil
	h.runIn(roomB, "Cleo, are you here?") // cleo is not in the library's cast: nobody named
	if len(h.posts) != 1 {
		t.Fatalf("library posts %v, want 1 (its max_bot_turns)", h.posts)
	}
	if c := h.calls[0]; c.bot == "cleo" || c.system != "LIBRARY-SYS-"+c.bot {
		t.Fatalf("library call %+v", c)
	}
}

func TestPerRoomResetExplainAndNotices(t *testing.T) {
	h := newRoomsHarness(t)
	ctx := context.Background()
	h.runIn(roomB, "hi")
	h.runIn(roomB, "!reset")
	if len(h.notices) != 1 || !strings.HasPrefix(h.notices[0], "bram: ") {
		t.Fatalf("notices %v: the library's notices come from its listener, bram", h.notices)
	}
	for bot, want := range map[string]int{"ada": 2, "bram": 2, "cleo": 1} {
		if got, _ := h.o.store.Generation(ctx, roomB, bot); got != want {
			t.Errorf("library generation of %s = %d, want %d (reset covers the room's cast only)", bot, got, want)
		}
	}
	if got, _ := h.o.store.Generation(ctx, roomID, "ada"); got != 1 {
		t.Errorf("a reset in the library bumped ada's lounge generation to %d", got)
	}

	h.runIn(roomB, "!explain Cleo")
	if len(h.notices) != 2 || !strings.HasPrefix(h.notices[1], "bram: !explain: not a character in this room") {
		t.Fatalf("notices %v: cleo is not in the library's cast", h.notices)
	}
	h.runIn(roomID, "!explain Bram")
	if len(h.notices) != 3 || !strings.HasPrefix(h.notices[2], "cleo: ") {
		t.Fatalf("notices %v: the lounge's notices come from cleo", h.notices)
	}
}
