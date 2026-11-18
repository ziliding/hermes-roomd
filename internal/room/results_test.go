package room

import (
	"context"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ziliding/hermes-roomd/internal/turns"
)

// triggerSpy records every trigger and the owed set it came with, then lets
// the plain rules decide.
type triggerSpy struct {
	inner turns.Decider
	mu    sync.Mutex
	trigs []turns.Trigger
	owed  [][]string
}

func (d *triggerSpy) Plan(ctx context.Context, st turns.State, t turns.Trigger) ([]string, error) {
	d.mu.Lock()
	d.trigs = append(d.trigs, t)
	d.owed = append(d.owed, slices.Clone(st.Owed))
	d.mu.Unlock()
	return d.inner.Plan(ctx, st, t)
}

// A result that ends while Cleo is mid-reply waits for her turn to end, then
// posts before anything else happens.
func TestResultWaitsForTheFloor(t *testing.T) {
	releaseAda, releaseCleo := make(chan struct{}), make(chan struct{})
	var adaOnce, cleoOnce sync.Once
	// Long enough that Cleo's slow reply stays in the foreground.
	h := newHarnessWith(t, harnessOpts{maxTurns: 0, backgroundAfter: 500 * time.Millisecond}, func(bot string, n int) string {
		first := false
		switch bot {
		case "ada":
			adaOnce.Do(func() { first = true })
			if first {
				<-releaseAda
				return "drawn: a cat"
			}
		case "cleo":
			cleoOnce.Do(func() { first = true })
			if first {
				<-releaseCleo
				return "reply cleo"
			}
		}
		return "reply " + bot
	})
	reqID := startBusyAda(t, h)
	w := h.o.rooms[roomID]

	h.o.Handle(h.ctx, h.user("你们觉得呢?")) // owed by Cleo only: Ada is busy
	h.waitCalls(2)
	close(releaseAda)
	waitFor(t, func() bool { return len(w.inbox) == 1 }) // Ada's result is queued
	time.Sleep(20 * time.Millisecond)
	if p, _, _, _, _ := h.snap(); len(p) != 0 {
		t.Fatalf("posted while Cleo held the floor: %v", p)
	}
	if busy := w.busyBots(); !slices.Equal(busy, []string{"ada"}) {
		t.Fatalf("busy %v: Ada must stay busy until her result is posted", busy)
	}

	close(releaseCleo)
	posts := h.waitPosts(2)
	if !slices.Equal(posts, []string{"cleo: reply cleo", "ada: drawn: a cat ↩ " + reqID}) {
		t.Fatalf("posts %v", posts)
	}
	h.waitIdle()
	time.Sleep(30 * time.Millisecond)
	if _, _, _, _, c := h.snap(); len(c) != 2 {
		t.Fatalf("%d calls: with max_bot_turns 0 nobody reacts to the result", len(c))
	}
}

// The posted result is a bot message the decider sees as the latest event,
// with nobody owed a reply: it never re-creates obligations to the user.
func TestResultIsABotEventWithoutObligations(t *testing.T) {
	release := make(chan struct{})
	spy := &triggerSpy{}
	h := newHarnessWith(t, harnessOpts{backgroundAfter: 200 * time.Millisecond, decider: func(bots []string) turns.Decider {
		spy.inner = turns.NewRules(bots, 2, rand.New(rand.NewPCG(1, 1)))
		return spy
	}}, slowAda(release))
	reqID := startBusyAda(t, h)
	h.waitPosts(1) // Cleo continues after the hand-off (one of two bot turns)

	close(release)
	posts := h.waitPosts(3)
	if !slices.Equal(posts[1:], []string{"ada: drawn: a cat ↩ " + reqID, "cleo: reply cleo"}) {
		t.Fatalf("posts %v", posts)
	}
	h.waitIdle()
	time.Sleep(30 * time.Millisecond)
	spy.mu.Lock()
	i := slices.IndexFunc(spy.trigs, func(t turns.Trigger) bool { return t.Sender == "ada" && !t.Handoff })
	if i < 0 {
		spy.mu.Unlock()
		t.Fatal("the decider never saw the result")
	}
	first, owed := spy.trigs[i], spy.owed[i]
	spy.mu.Unlock()
	if first.FromUser || first.Sender != "ada" || first.Body != "drawn: a cat" || first.Handoff {
		t.Fatalf("trigger for the result %+v", first)
	}
	if len(owed) != 0 {
		t.Fatalf("owed %v after a result", owed)
	}
	if _, _, _, _, c := h.snap(); len(c) != 3 {
		t.Fatalf("%d calls: the reaction to a result shares max_bot_turns since the user spoke", len(c))
	}
}

// A user message that arrives after the run ended but before the result is
// handled stops any reaction: the user's message is answered instead.
func TestUserMessageStopsReactionToResult(t *testing.T) {
	release := make(chan struct{})
	h := newHarnessWith(t, harnessOpts{maxTurns: 2, backgroundAfter: 200 * time.Millisecond}, slowAda(release))
	w := h.o.rooms[roomID]
	req := h.runOne("Ada, draw a cat in Canva") // hand-off, then Cleo continues
	_, _, _, _, calls := h.snap()
	n := len(calls)

	close(release)
	waitFor(t, func() bool { return len(w.inbox) == 1 })
	h.o.Handle(h.ctx, h.user("Cleo, one more thing"))
	q := <-w.inbox
	if q.result == nil {
		t.Fatal("first queued item is not the result")
	}
	h.o.result(h.ctx, w, q)
	posts, _, _, _, calls := h.snap()
	if posts[len(posts)-1] != "ada: drawn: a cat ↩ "+req.EventID {
		t.Fatalf("posts %v", posts)
	}
	if len(calls) != n {
		t.Fatalf("a bot reacted to the result although the user spoke: %d calls, want %d", len(calls), n)
	}
	if len(w.held) != 1 || w.held[0].msg.Body != "Cleo, one more thing" {
		t.Fatalf("held %+v", w.held)
	}

	go h.o.Run(h.ctx)
	if c := h.waitCalls(n + 1); c[n].bot != "cleo" {
		t.Fatalf("the user's message went to %q", c[n].bot)
	}
}
