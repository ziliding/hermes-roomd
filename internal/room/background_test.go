package room

import (
	"context"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ziliding/hermes-roomd/internal/turns"
)

// spyDecider records the busy list of every decision, then lets the plain
// rules decide.
type spyDecider struct {
	inner turns.Decider
	mu    sync.Mutex
	busy  [][]string
}

func (d *spyDecider) Next(ctx context.Context, st turns.State, t turns.Trigger) (string, bool, error) {
	d.mu.Lock()
	d.busy = append(d.busy, append([]string(nil), st.Busy...))
	d.mu.Unlock()
	return d.inner.Next(ctx, st, t)
}

func (h *harness) snap() (posts, notices, reacts, typing []string, calls []call) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.posts), slices.Clone(h.notices), slices.Clone(h.reacts), slices.Clone(h.typing), slices.Clone(h.calls)
}

func (h *harness) waitPosts(n int) []string {
	h.t.Helper()
	waitFor(h.t, func() bool { p, _, _, _, _ := h.snap(); return len(p) >= n })
	p, _, _, _, _ := h.snap()
	return p
}

func (h *harness) waitNotices(n int) []string {
	h.t.Helper()
	waitFor(h.t, func() bool { _, ns, _, _, _ := h.snap(); return len(ns) >= n })
	_, ns, _, _, _ := h.snap()
	return ns
}

func (h *harness) waitCalls(n int) []call {
	h.t.Helper()
	waitFor(h.t, func() bool { _, _, _, _, c := h.snap(); return len(c) >= n })
	_, _, _, _, c := h.snap()
	return c
}

// waitIdle waits until the room has no busy bot.
func (h *harness) waitIdle() {
	h.t.Helper()
	w := h.o.rooms[roomID]
	waitFor(h.t, func() bool { return len(w.busyBots()) == 0 })
}

// slowAda makes Ada's first run wait for release; every other run answers
// "reply <bot>" at once (Ada's first reply is "drawn: a cat").
func slowAda(release <-chan struct{}) func(bot string, n int) string {
	var once sync.Once
	return func(bot string, n int) string {
		first := false
		if bot == "ada" {
			once.Do(func() { first = true })
		}
		if first {
			<-release
			return "drawn: a cat"
		}
		return "reply " + bot
	}
}

// startBusyAda runs the worker, asks Ada for slow work and waits until it is
// handed off to the background. It returns the request message's event id.
func startBusyAda(t *testing.T, h *harness) string {
	t.Helper()
	go h.o.Run(h.ctx)
	m := h.user("Ada, draw a cat in Canva")
	h.o.Handle(h.ctx, m)
	if ns := h.waitNotices(1); ns[0] != "ada: Ada is working on it…" {
		t.Fatalf("hand-off notice %q", ns)
	}
	if busy := h.o.rooms[roomID].busyBots(); !slices.Equal(busy, []string{"ada"}) {
		t.Fatalf("busy %v", busy)
	}
	return m.EventID
}

func TestQuickRunStaysSynchronous(t *testing.T) {
	h := newHarnessWith(t, harnessOpts{maxTurns: 1, backgroundAfter: time.Second}, func(bot string, n int) string { return "hi from " + bot })
	h.runOne("Ada, hello")
	posts, notices, _, typing, _ := h.snap()
	if !slices.Equal(posts, []string{"ada: hi from ada"}) {
		t.Fatalf("posts %v (a quick reply is a plain message, not a reply)", posts)
	}
	if len(notices) != 0 {
		t.Fatalf("notices %v", notices)
	}
	waitFor(t, func() bool { _, _, _, ty, _ := h.snap(); return len(ty) == 2 })
	if _, _, _, typing, _ = h.snap(); !slices.Equal(typing, []string{"ada: on", "ada: off"}) {
		t.Fatalf("typing %v", typing)
	}
	if busy := h.o.rooms[roomID].busyBots(); len(busy) != 0 {
		t.Fatalf("busy %v", busy)
	}
}

func TestTypingIsRefreshedWhileARunGoes(t *testing.T) {
	release := make(chan struct{})
	h := newHarnessWith(t, harnessOpts{maxTurns: 1, typingEvery: 5 * time.Millisecond}, func(bot string, n int) string {
		<-release
		return "done"
	})
	done := make(chan struct{})
	go func() { h.runOne("Ada?"); close(done) }()
	waitFor(t, func() bool { _, _, _, ty, _ := h.snap(); return len(ty) >= 3 })
	close(release)
	<-done
	waitFor(t, func() bool { _, _, _, ty, _ := h.snap(); return ty[len(ty)-1] == "ada: off" })
	_, _, _, typing, _ := h.snap()
	for _, s := range typing[:len(typing)-1] {
		if s != "ada: on" {
			t.Fatalf("typing %v", typing)
		}
	}
}

// A slow run is handed off; the room goes on without Ada (the rules never
// pick her), and her result arrives as a reply to the request, starting no
// chain.
func TestSlowRunIsBackgroundedAndRepliesLater(t *testing.T) {
	release := make(chan struct{})
	spy := &spyDecider{}
	h := newHarnessWith(t, harnessOpts{maxTurns: 2, backgroundAfter: 20 * time.Millisecond, decider: func(bots []string) turns.Decider {
		spy.inner = turns.NewRules(bots, 2, rand.New(rand.NewPCG(1, 1)))
		return spy
	}}, slowAda(release))
	reqID := startBusyAda(t, h)

	// The hand-off is Ada's turn; the chain goes on without her.
	if posts := h.waitPosts(1); posts[0] != "cleo: reply cleo" {
		t.Fatalf("after the hand-off: posts %v", posts)
	}

	// The room goes on: only Cleo can speak, and then nobody (Ada is busy).
	h.o.Handle(h.ctx, h.user("anyone around?"))
	if posts := h.waitPosts(2); posts[1] != "cleo: reply cleo" {
		t.Fatalf("posts %v", posts)
	}
	waitFor(t, func() bool { spy.mu.Lock(); defer spy.mu.Unlock(); return len(spy.busy) >= 3 })
	spy.mu.Lock()
	for i, b := range spy.busy[1:] {
		if !slices.Equal(b, []string{"ada"}) {
			t.Errorf("decision %d saw busy %v", i+1, b)
		}
	}
	spy.mu.Unlock()

	close(release)
	posts := h.waitPosts(3)
	if posts[2] != "ada: drawn: a cat ↩ "+reqID {
		t.Fatalf("background result %q, want a reply to %s", posts[2], reqID)
	}
	h.waitIdle()
	time.Sleep(30 * time.Millisecond)
	if _, _, _, _, calls := h.snap(); len(calls) != 3 {
		t.Fatalf("the background result started a chain: %d calls", len(calls))
	}
	_, _, _, typing, _ := h.snap()
	if typing[len(typing)-1] != "ada: off" && !slices.Contains(typing, "ada: off") {
		t.Fatalf("typing never turned off: %v", typing)
	}

	// Ada's cursor moved to the end of her request's delta: her next turn
	// sees what happened meanwhile, not the request again.
	h.o.Handle(h.ctx, h.user("Ada, nice"))
	calls := h.waitCalls(4)
	want := "<msg from=\"Cleo\">reply cleo</msg>\n<msg from=\"Alex\">anyone around?</msg>\n<msg from=\"Cleo\">reply cleo</msg>\n<msg from=\"Alex\">Ada, nice</msg>"
	if calls[3].bot != "ada" || calls[3].delta != want {
		t.Fatalf("ada's next delta %+v", calls[3])
	}
}

// Steering a busy bot, and exactly-once delivery of the steered message.
func TestSteerBusyBot(t *testing.T) {
	steer := "<msg from=\"Alex\">Ada, make it blue</msg>"
	tests := []struct {
		name       string
		keepSteers bool // the run ends with the steer in pending_steer
		refuse     int  // the steer call answers this status
		wantAck    bool
		wantResent bool // the steered message is in Ada's next delta
	}{
		{"consumed: never resent", false, 0, true, false},
		{"returned in pending_steer: resent once", true, 0, true, true},
		{"refused with 409: resent once", false, 409, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			release := make(chan struct{})
			h := newHarnessWith(t, harnessOpts{maxTurns: 1, backgroundAfter: 20 * time.Millisecond}, slowAda(release))
			h.keepSteers = tt.keepSteers
			h.srv.SteerStatus = tt.refuse
			startBusyAda(t, h)
			runID := h.waitCalls(1)[0].run

			m := h.user("Ada, make it blue")
			h.o.Handle(h.ctx, m)
			if tt.wantAck {
				waitFor(t, func() bool { _, _, r, _, _ := h.snap(); return len(r) == 1 })
				if _, _, r, _, _ := h.snap(); r[0] != "ada: "+m.EventID+" ✓" {
					t.Fatalf("reaction %v", r)
				}
				if got := h.srv.Get(runID).Steers; !slices.Equal(got, []string{steer}) {
					t.Fatalf("steers %q", got)
				}
			} else {
				waitFor(t, func() bool { return slices.Contains(h.srv.Requests(), "POST /p/ada/v1/runs/"+runID+"/steer") })
			}
			time.Sleep(20 * time.Millisecond)
			if _, _, r, _, c := h.snap(); len(c) != 1 || (!tt.wantAck && len(r) != 0) {
				t.Fatalf("steering started a chain or acked a refused steer: calls %d reacts %v", len(c), r)
			}

			close(release)
			h.waitPosts(1)
			h.waitIdle()
			h.o.Handle(h.ctx, h.user("Ada, done?"))
			calls := h.waitCalls(2)
			if n := strings.Count(calls[1].delta, "make it blue"); (n == 1) != tt.wantResent || n > 1 {
				t.Fatalf("steered message appears %d times in the next delta:\n%s", n, calls[1].delta)
			}
			if !strings.Contains(calls[1].delta, "Ada, done?") {
				t.Fatalf("next delta %q", calls[1].delta)
			}
		})
	}
}

// A message to someone else (or nobody) while Ada is busy is not steered:
// it reaches her on her next turn.
func TestMessagesToOthersAreNotSteered(t *testing.T) {
	release := make(chan struct{})
	h := newHarnessWith(t, harnessOpts{maxTurns: 1, backgroundAfter: 20 * time.Millisecond}, slowAda(release))
	startBusyAda(t, h)
	h.o.Handle(h.ctx, h.user("Cleo, what do you think?"))
	h.waitPosts(1)
	runID := h.waitCalls(1)[0].run
	if s := h.srv.Get(runID).Steers; len(s) != 0 {
		t.Fatalf("steered %q", s)
	}
	close(release)
	h.waitPosts(2)
	h.waitIdle()
	h.o.Handle(h.ctx, h.user("Ada?"))
	if c := h.waitCalls(3); !strings.Contains(c[2].delta, "Cleo, what do you think?") {
		t.Fatalf("ada's delta %q", c[2].delta)
	}
}

func TestBackgroundFailureAndTimeout(t *testing.T) {
	tests := []struct {
		name       string
		reply      string
		runTimeout time.Duration
		notice     string
		wantStop   bool
	}{
		{"failed run", "ERR", 0, "ada: Ada couldn't finish that task.", false},
		{"timed out run is stopped", "HANG", 150 * time.Millisecond, "ada: Ada ran out of time on that task and stopped.", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			h := newHarnessWith(t, harnessOpts{maxTurns: 1, backgroundAfter: 20 * time.Millisecond, runTimeout: tt.runTimeout},
				func(bot string, n int) string {
					first := false
					once.Do(func() { first = true })
					if first {
						<-release
						return tt.reply
					}
					return "reply " + bot
				})
			startBusyAda(t, h)
			runID := h.waitCalls(1)[0].run
			close(release)
			if ns := h.waitNotices(2); ns[1] != tt.notice {
				t.Fatalf("notices %v", ns)
			}
			h.waitIdle()
			if got := h.srv.Get(runID).Stopped; got != tt.wantStop {
				t.Fatalf("stopped = %v", got)
			}
			if p, _, _, _, _ := h.snap(); len(p) != 0 {
				t.Fatalf("posts %v", p)
			}
			// The cursor stayed put: the request is offered again.
			h.o.Handle(h.ctx, h.user("Ada, try again"))
			if c := h.waitCalls(2); !strings.Contains(c[1].delta, "draw a cat") {
				t.Fatalf("failed delta not re-offered: %q", c[1].delta)
			}
		})
	}
}

func TestResetWhileBusyStillPostsResult(t *testing.T) {
	release := make(chan struct{})
	h := newHarnessWith(t, harnessOpts{maxTurns: 1, backgroundAfter: 20 * time.Millisecond}, slowAda(release))
	reqID := startBusyAda(t, h)
	h.o.Handle(h.ctx, h.user("!reset"))
	h.waitNotices(2)
	close(release)
	if p := h.waitPosts(1); p[0] != "ada: drawn: a cat ↩ "+reqID {
		t.Fatalf("result after reset %v", p)
	}
	h.waitIdle()
	h.o.Handle(h.ctx, h.user("Ada, fresh start"))
	c := h.waitCalls(2)
	if c[1].session != SessionID(roomID, "ada", 2) {
		t.Fatalf("next call session %q, want generation 2", c[1].session)
	}
	if c[1].delta != "<msg from=\"Alex\">Ada, fresh start</msg>" {
		t.Fatalf("the new session saw pre-reset messages: %q", c[1].delta)
	}
}

func TestExplainBusyBot(t *testing.T) {
	release := make(chan struct{})
	h := newHarnessWith(t, harnessOpts{maxTurns: 1, backgroundAfter: 20 * time.Millisecond}, slowAda(release))
	startBusyAda(t, h)
	h.o.Handle(h.ctx, h.user("!explain Ada"))
	if ns := h.waitNotices(2); ns[1] != "ada: Ada is busy with a task; try !explain again when it has answered." {
		t.Fatalf("notices %v", ns)
	}
	if _, _, _, _, c := h.snap(); len(c) != 1 {
		t.Fatalf("explain called a busy bot: %d calls", len(c))
	}
	close(release)
	h.waitPosts(1)
}

func TestBusyIsPerRoom(t *testing.T) {
	release := make(chan struct{})
	h := newRoomsHarness(t)
	h.reply = slowAda(release)
	for _, w := range h.o.rooms {
		w.backgroundAfter = 20 * time.Millisecond
	}
	go h.o.Run(h.ctx)
	h.o.Handle(h.ctx, h.say(roomID, "Ada, draw a cat"))
	waitFor(t, func() bool { return len(h.o.rooms[roomID].busyBots()) == 1 })
	h.o.Handle(h.ctx, h.say(roomB, "Ada, quick one"))
	waitFor(t, func() bool { p, _, _, _, _ := h.snap(); return slices.Contains(p, "ada: reply ada") })
	if b := h.o.rooms[roomB].busyBots(); len(b) != 0 {
		t.Fatalf("room B busy %v", b)
	}
	close(release)
	waitFor(t, func() bool { p, _, _, _, _ := h.snap(); return slices.ContainsFunc(p, func(s string) bool { return strings.HasPrefix(s, "ada: drawn: a cat ↩ ") }) })
}

func TestShutdownWithRunInFlightDoesNotHang(t *testing.T) {
	h := newHarnessWith(t, harnessOpts{maxTurns: 1, backgroundAfter: 20 * time.Millisecond}, func(string, int) string { return "HANG" })
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	go func() { h.o.Run(ctx); close(done) }()
	h.o.Handle(ctx, h.user("Ada, long job"))
	h.waitNotices(1)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after shutdown")
	}
	if _, _, _, ty, _ := h.snap(); ty[len(ty)-1] != "ada: off" {
		t.Fatalf("typing left on at shutdown: %v", ty)
	}
}

// The run ends the moment it consumes the steer: the steer is recorded
// before the run's end is seen, so the message is never sent again.
func TestSteerConsumedRightAsRunEnds(t *testing.T) {
	for i := 0; i < 5; i++ {
		var h *harness
		var once sync.Once
		h = newHarnessWith(t, harnessOpts{maxTurns: 1, backgroundAfter: 20 * time.Millisecond}, func(bot string, n int) string {
			first := false
			once.Do(func() { first = true })
			if !first {
				return "reply " + bot
			}
			run := h.waitCalls(1)[0].run
			waitFor(t, func() bool { return len(h.srv.Get(run).Steers) == 1 })
			return "blue cat"
		})
		startBusyAda(t, h)
		h.o.Handle(h.ctx, h.user("Ada, make it blue"))
		h.waitPosts(1)
		h.waitIdle()
		h.o.Handle(h.ctx, h.user("Ada, thanks"))
		if c := h.waitCalls(2); strings.Contains(c[1].delta, "make it blue") {
			t.Fatalf("consumed steer resent: %q", c[1].delta)
		}
	}
}

// Polling errors (Hermes draining: 503) are retried; the run's result
// still posts, with no failure notice.
func TestTransientPollErrorsAreRetried(t *testing.T) {
	h := newHarnessWith(t, harnessOpts{maxTurns: 1}, func(bot string, n int) string { return "fine" })
	h.srv.StatusFailures = 3
	h.runOne("Ada?")
	posts, notices, _, _, _ := h.snap()
	if !slices.Equal(posts, []string{"ada: fine"}) || len(notices) != 0 {
		t.Fatalf("posts %v notices %v", posts, notices)
	}
}

// An unfinished turn (Hermes: failed, with output and no error) is still
// posted and advances the cursor, like a completed one.
func TestPartialRunIsPosted(t *testing.T) {
	tests := []struct {
		name            string
		backgroundAfter time.Duration
		want            string
	}{
		{"foreground", 0, "ada: got halfway"},
		{"background", 20 * time.Millisecond, "ada: got halfway ↩ "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			release := make(chan struct{})
			close(release)
			if tt.backgroundAfter > 0 {
				release = make(chan struct{})
			}
			h := newHarnessWith(t, harnessOpts{maxTurns: 1, backgroundAfter: tt.backgroundAfter}, func(bot string, n int) string {
				if n == 1 {
					<-release
					return "PARTIAL:got halfway"
				}
				return "ok"
			})
			go h.o.Run(h.ctx)
			m := h.user("Ada, big job")
			h.o.Handle(h.ctx, m)
			if tt.backgroundAfter > 0 {
				h.waitNotices(1)
				close(release)
			}
			want := tt.want
			if tt.backgroundAfter > 0 {
				want += m.EventID
			}
			if p := h.waitPosts(1); p[0] != want {
				t.Fatalf("posts %v, want %q", p, want)
			}
			h.waitIdle()
			h.o.Handle(h.ctx, h.user("Ada, and?"))
			if c := h.waitCalls(2); strings.Contains(c[1].delta, "big job") {
				t.Fatalf("partial run's delta re-sent: %q", c[1].delta)
			}
		})
	}
}

// While Ada works in the background, the others are told so in their run
// input (not in the transcript); once she is done the status is gone.
func TestOthersSeeBusyStatus(t *testing.T) {
	release := make(chan struct{})
	h := newHarnessWith(t, harnessOpts{maxTurns: 2, backgroundAfter: 20 * time.Millisecond}, slowAda(release))
	startBusyAda(t, h)
	status := `Ada is working on a task in the background, started by Alex's message "Ada, draw a cat in Canva". ` +
		`Ada's result will be posted when it is done; don't do that task yourself or answer for Ada.`

	calls := h.waitCalls(2)
	if calls[1].bot != "cleo" || !strings.Contains(calls[1].raw, "</messages>\n\n"+status+"\n\nReply as Cleo") {
		t.Fatalf("cleo's input lacks the busy status:\n%s", calls[1].raw)
	}
	if strings.Contains(calls[1].delta, "working on") {
		t.Fatalf("status leaked into the messages: %q", calls[1].delta)
	}

	close(release)
	h.waitPosts(2)
	h.waitIdle()
	h.o.Handle(h.ctx, h.user("Cleo, and now?"))
	if c := h.waitCalls(3)[2]; c.bot != "cleo" || strings.Contains(c.raw, "in the background") {
		t.Fatalf("status still shown after Ada finished: %+v", c)
	}
}
