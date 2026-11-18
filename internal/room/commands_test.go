package room

import (
	"context"
	"strings"
	"testing"

	"github.com/ziliding/hermes-roomd/internal/commands"
	"github.com/ziliding/hermes-roomd/internal/matrixio"
	"github.com/ziliding/hermes-roomd/internal/turns"
)

// runCmd delivers a message and runs the command it queued synchronously.
func (h *harness) runCmd(m matrixio.Message) {
	h.t.Helper()
	ctx := context.Background()
	h.o.Handle(ctx, m)
	w := h.o.rooms[roomID]
	select {
	case q := <-w.inbox:
		if q.cmd == nil {
			h.t.Fatalf("%q was queued as conversation, not a command", m.Body)
		}
		h.o.command(ctx, w, q)
	default:
		h.t.Fatalf("%q queued nothing", m.Body)
	}
}

func (h *harness) latestSeq() int64 {
	seq, err := h.o.store.LatestSeq(context.Background(), roomID)
	if err != nil {
		h.t.Fatal(err)
	}
	return seq
}

func TestResetBumpsGenerationsAndMovesCursors(t *testing.T) {
	h := newHarness(t, 2, func(bot string, n int) string { return "reply " + bot })
	h.runOne("Cleo, hello") // cleo then ada speak in generation 1
	before := h.latestSeq()

	h.runCmd(h.user("/reset"))
	if h.latestSeq() != before {
		t.Fatal("/reset was recorded as conversation")
	}
	if want := "ada: " + commands.ResetNotice; len(h.notices) != 1 || h.notices[0] != want {
		t.Fatalf("notices %v, want [%s]", h.notices, want)
	}
	// The notice echo from the listener is dropped, not recorded.
	h.o.Handle(context.Background(), matrixio.Message{RoomID: roomID, EventID: "$notice-echo", Sender: "@ada:z", Body: commands.ResetNotice, Control: true})
	if h.latestSeq() != before {
		t.Fatal("notice echo was recorded as conversation")
	}

	h.runOne("Ada, fresh start")
	if len(h.calls) != 4 {
		t.Fatalf("calls %d, want 4", len(h.calls))
	}
	for _, c := range h.calls[2:] {
		if !strings.HasSuffix(c.session, "-"+c.bot+"-g2") {
			t.Errorf("%s still on old session %s", c.bot, c.session)
		}
	}
	if got := h.calls[2]; got.bot != "ada" || got.delta != `<msg from="Alex">Ada, fresh start</msg>` {
		t.Errorf("ada saw pre-reset messages: %+v", got)
	}
	if got := h.calls[3]; got.bot != "cleo" || got.delta != `<msg from="Alex">Ada, fresh start</msg>`+"\n"+`<msg from="Ada">reply ada</msg>` {
		t.Errorf("cleo saw pre-reset messages: %q", got.delta)
	}

	h.runCmd(h.user("!reset")) // second reset: g3, never a reused id
	h.runOne("Cleo?")
	if got := h.calls[len(h.calls)-2].session; !strings.HasSuffix(got, "-cleo-g3") {
		t.Errorf("second reset session %s, want g3", got)
	}
}

// A message sent right after /reset must still reach the bots: the cursor
// moves to the newest seq when /reset arrived, not when it ran.
func TestResetDoesNotHideALaterMessage(t *testing.T) {
	h := newHarness(t, 1, func(bot string, n int) string { return "ok" })
	h.runOne("Ada, hi")
	ctx := context.Background()
	h.o.Handle(ctx, h.user("/reset"))
	h.o.Handle(ctx, h.user("Cleo, after the reset"))
	w := h.o.rooms[roomID]
	q := <-w.inbox
	h.o.command(ctx, w, q) // runs after the later message is already recorded
	q = <-w.inbox
	if q.cmd != nil {
		t.Fatal("second item should be the message")
	}
	w.state.UserSpoke()
	h.o.chain(ctx, w, q)
	if got := h.calls[len(h.calls)-1]; got.bot != "cleo" || got.delta != `<msg from="Alex">Cleo, after the reset</msg>` {
		t.Fatalf("post-reset message lost or old context leaked: %+v", got)
	}
}

func TestResetDiscardsInFlightReply(t *testing.T) {
	h := newHarness(t, 2, func(bot string, n int) string { return "old-session reply" })
	h.gate = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.o.Run(ctx)

	h.o.Handle(ctx, h.user("Ada, long story please"))
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.calls) == 1 })
	h.o.Handle(ctx, h.user("/reset"))
	h.gate <- struct{}{}
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.notices) == 1 })
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.posts) != 0 || len(h.calls) != 1 {
		t.Fatalf("reply from before /reset posted or chain continued: posts=%v calls=%d", h.posts, len(h.calls))
	}
}

func TestCommandsFromOthersAreConversation(t *testing.T) {
	h := newHarness(t, 2, func(bot string, n int) string { return "x" })
	ctx := context.Background()
	for i, sender := range []string{"@ada:z", "@stranger:z"} {
		h.o.Handle(ctx, matrixio.Message{RoomID: roomID, EventID: "$o" + string(rune('a'+i)), Sender: sender, Body: "/reset", TS: 1})
	}
	if n := len(h.o.rooms[roomID].inbox); n != 0 {
		t.Fatalf("command from a non-user queued %d items", n)
	}
	if h.latestSeq() != 2 {
		t.Fatalf("non-user /reset should be recorded as ordinary text, seq=%d", h.latestSeq())
	}
	gen, _ := h.o.store.Generation(ctx, roomID, "ada")
	if gen != 1 || len(h.notices) != 0 {
		t.Fatalf("non-user /reset took effect: gen=%d notices=%v", gen, h.notices)
	}
}

func TestExplain(t *testing.T) {
	h := newHarness(t, 2, func(bot string, n int) string { return "plainly: " + bot })
	h.runCmd(h.user("!explain"))
	if len(h.calls) != 0 || len(h.notices) != 1 || !strings.Contains(h.notices[0], "no bot has spoken") {
		t.Fatalf("explain with no speaker: calls=%d notices=%v", len(h.calls), h.notices)
	}

	h.runOne("Cleo, plan?") // cleo, then ada: ada is the last speaker
	before := h.latestSeq()
	h.runCmd(h.user("!explain"))
	if h.latestSeq() != before+1 { // only the bot's reply is recorded, not the command
		t.Fatalf("seq %d → %d", before, h.latestSeq())
	}
	last := h.calls[len(h.calls)-1]
	if len(h.calls) != 3 || last.bot != "ada" || !strings.Contains(last.raw, "asked you to explain") || !strings.HasPrefix(last.raw, "There are no new messages") {
		t.Fatalf("explain call %+v (calls=%d)", last, len(h.calls))
	}
	if last.system != "SYS-ada" {
		t.Error("explain changed the system prompt")
	}
	if h.posts[len(h.posts)-1] != "ada: plainly: ada" {
		t.Fatalf("posts %v", h.posts)
	}

	h.runCmd(h.user("/explain Cleo")) // named bot; one turn only, no chain after it
	if len(h.calls) != 4 || h.calls[3].bot != "cleo" || !strings.Contains(h.calls[3].delta, `<msg from="Ada">plainly: ada</msg>`) {
		t.Fatalf("explain cleo: %+v", h.calls[3:])
	}

	h.runCmd(h.user("/explain Dex"))
	if len(h.calls) != 4 || !strings.Contains(h.notices[len(h.notices)-1], "not a character") {
		t.Fatalf("unknown bot: calls=%d notices=%v", len(h.calls), h.notices)
	}
}

// blockingDecider holds every decision until released, like a slow router.
type blockingDecider struct {
	entered chan struct{}
	release chan struct{}
}

func (d blockingDecider) Next(_ context.Context, st turns.State, t turns.Trigger) (string, bool, error) {
	d.entered <- struct{}{}
	<-d.release
	if st.BotTurns > 0 {
		return "", false, nil
	}
	return "ada", true, nil
}

// /reset arriving while the router is still deciding: the stale decision is
// dropped (no old-session call), and the next turn starts on g2.
func TestResetDuringSlowDecision(t *testing.T) {
	h := newHarness(t, 2, func(bot string, n int) string { return "reply " + bot })
	d := blockingDecider{entered: make(chan struct{}), release: make(chan struct{})}
	h.o.rooms[roomID].decider = d
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.o.Run(ctx)

	h.o.Handle(ctx, h.user("hello all"))
	<-d.entered
	h.o.Handle(ctx, h.user("/reset"))
	d.release <- struct{}{}
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.notices) == 1 })
	h.mu.Lock()
	if len(h.calls) != 0 || len(h.posts) != 0 {
		t.Fatalf("stale decision ran a turn: calls=%v posts=%v", h.calls, h.posts)
	}
	h.mu.Unlock()

	h.o.Handle(ctx, h.user("after reset"))
	<-d.entered
	d.release <- struct{}{}
	<-d.entered // decision after ada's reply
	d.release <- struct{}{}
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.posts) == 1 })
	h.mu.Lock()
	defer h.mu.Unlock()
	if c := h.calls[0]; !strings.HasSuffix(c.session, "-ada-g2") || c.delta != `<msg from="Alex">after reset</msg>` {
		t.Fatalf("post-reset call %+v", c)
	}
}
