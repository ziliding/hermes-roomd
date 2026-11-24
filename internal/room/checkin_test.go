package room

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ziliding/hermes-roomd/internal/checkin"
	"github.com/ziliding/hermes-roomd/internal/matrixio"
)

const checkInMark = "Nobody has written in the group chat for"

// runCheckIn queues a check-in and runs it synchronously.
func (h *harness) runCheckIn(pick uint64) {
	w := h.o.rooms[roomID]
	h.o.runCheckIn(h.ctx, w, queued{checkIn: &checkInReq{quiet: 3 * time.Hour, pick: pick}, epoch: w.userEpoch.Load()})
}

func TestCheckInOpensAndOthersReact(t *testing.T) {
	h := newHarness(t, 1, func(bot string, n int) string { return "hi from " + bot })
	h.runCheckIn(0)
	if want := "ada: hi from ada|cleo: hi from cleo"; strings.Join(h.posts, "|") != want {
		t.Fatalf("posts %v, want %s (the opener, then one reaction under max_bot_turns)", h.posts, want)
	}
	if !strings.Contains(h.calls[0].raw, checkInMark+" about 3 hours") || !strings.Contains(h.calls[0].raw, "There are no new messages") {
		t.Errorf("opener input: %q", h.calls[0].raw)
	}
	if strings.Contains(h.calls[1].raw, checkInMark) || h.calls[1].delta != `<msg from="Ada">hi from ada</msg>` {
		t.Errorf("the reacting bot gets the opener as an ordinary message: %q", h.calls[1].raw)
	}
}

func TestCheckInNotTheLastSpeaker(t *testing.T) {
	h := newHarness(t, 0, func(bot string, n int) string { return "hi from " + bot })
	h.o.Handle(h.ctx, matrixio.Message{RoomID: roomID, EventID: "$a", Sender: "@ada:z", Body: "earlier", TS: 1})
	h.runCheckIn(0) // pick 0 would be ada, but ada spoke last
	if len(h.calls) != 1 || h.calls[0].bot != "cleo" {
		t.Fatalf("calls %+v, want one call to cleo", h.calls)
	}
}

func TestCheckInSilentOpenerEndsIt(t *testing.T) {
	h := newHarness(t, 2, func(bot string, n int) string { return "[SILENT]" })
	h.runCheckIn(1)
	if len(h.calls) != 1 || h.calls[0].bot != "cleo" || len(h.posts) != 0 {
		t.Fatalf("calls %+v posts %v, want one silent call", h.calls, h.posts)
	}
}

func TestCheckInSkippedWhenUserSpoke(t *testing.T) {
	h := newHarness(t, 1, func(bot string, n int) string { return "x" })
	w := h.o.rooms[roomID]
	q := queued{checkIn: &checkInReq{quiet: time.Hour}, epoch: w.userEpoch.Load()}
	h.o.Handle(h.ctx, h.user("hello?"))
	h.o.runCheckIn(h.ctx, w, q)
	if len(h.calls) != 0 {
		t.Fatalf("a stale check-in called %+v", h.calls)
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// startWatching runs the orchestrator (with check-ins) on a fake clock
// starting at start.
func startWatching(t *testing.T, sched checkin.Schedule, maxUnanswered int, start time.Time) (*harness, *fakeClock) {
	h := newHarnessWith(t, harnessOpts{checkIn: &CheckIn{Schedule: sched, MaxUnanswered: maxUnanswered, Every: 2 * time.Millisecond}},
		func(bot string, n int) string { return "hi from " + bot })
	clk := &fakeClock{t: start}
	h.o.now = clk.Now
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go h.o.Run(ctx)
	time.Sleep(20 * time.Millisecond) // Run reads the start time before the test moves the clock
	return h, clk
}

func (h *harness) nCalls() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.calls) }
func (h *harness) nPosts() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.posts) }

// stays checks that the number of calls stays n for a while.
func (h *harness) stays(n int) {
	h.t.Helper()
	time.Sleep(60 * time.Millisecond)
	if got := h.nCalls(); got != n {
		h.t.Fatalf("calls %d, want %d", got, n)
	}
}

func TestWatcherChecksInAfterQuietSpell(t *testing.T) {
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	h, clk := startWatching(t, checkin.Schedule{After: 3 * time.Hour, Loc: time.UTC}, 2, t0)

	clk.Set(t0.Add(2 * time.Hour))
	h.stays(0) // a restart never sets the bots off: quiet counts from roomd's start

	clk.Set(t0.Add(3*time.Hour + time.Minute))
	waitFor(t, func() bool { return h.nPosts() == 1 })
	h.stays(1) // once per quiet spell
	h.mu.Lock()
	if !strings.Contains(h.calls[0].raw, checkInMark) || !strings.Contains(h.calls[0].raw, "Tuesday 13:01") {
		t.Errorf("check-in input: %q", h.calls[0].raw)
	}
	h.mu.Unlock()

	clk.Set(t0.Add(6*time.Hour + 2*time.Minute)) // a full spell after the bot's message
	waitFor(t, func() bool { return h.nPosts() == 2 })

	clk.Set(t0.Add(12 * time.Hour))
	h.stays(2) // max_unanswered: 2

	// The user is back: the count starts over, and quiet counts from their message.
	h.o.Handle(context.Background(), matrixio.Message{RoomID: roomID, EventID: "$u-back", Sender: "@alex:z", Body: "ok", TS: t0.Add(12 * time.Hour).UnixMilli()})
	waitFor(t, func() bool { return h.nPosts() == 3 }) // a bot answers the user
	clk.Set(t0.Add(14 * time.Hour))
	h.stays(3)
	clk.Set(t0.Add(15*time.Hour + time.Minute))
	waitFor(t, func() bool { return h.nPosts() == 4 })
}

func TestWatcherWaitsOutQuietHours(t *testing.T) {
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	quiet, _ := checkin.ParseWindow("12:00-14:00")
	h, clk := startWatching(t, checkin.Schedule{After: 3 * time.Hour, Quiet: quiet, Loc: time.UTC}, 0, t0)

	clk.Set(t0.Add(3*time.Hour + 30*time.Minute)) // 13:30: due, but inside quiet hours
	h.stays(0)
	clk.Set(t0.Add(4*time.Hour + time.Minute)) // 14:01
	waitFor(t, func() bool { return h.nPosts() == 1 })
}
