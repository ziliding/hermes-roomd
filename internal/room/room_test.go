package room

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ziliding/hermes-roomd/internal/hermes"
	"github.com/ziliding/hermes-roomd/internal/hermes/hermestest"
	"github.com/ziliding/hermes-roomd/internal/matrixio"
	"github.com/ziliding/hermes-roomd/internal/media"
	"github.com/ziliding/hermes-roomd/internal/roster"
	"github.com/ziliding/hermes-roomd/internal/transcript"
	"github.com/ziliding/hermes-roomd/internal/turns"
)

const roomID = "!r:example.com"

type call struct{ bot, session, system, delta, raw, run string }

// harness wires an Orchestrator to a fake Hermes Runs server (through the
// real hermes.Client) and fake posters.
type harness struct {
	t       *testing.T
	ctx     context.Context
	o       *Orchestrator
	srv     *hermestest.Server
	mu      sync.Mutex
	calls   []call
	posts   []string // "bot: text" (replies: "bot: text ↩ $event")
	notices []string // "bot: text"
	reacts  []string // "bot: $event key"
	typing  []string // "bot: on" / "bot: off"
	// reply decides each run's output; it runs in the run's own goroutine
	// and may block to make the run slow. "ERR" fails the run, "HANG" never
	// finishes it, "PARTIAL:<text>" ends it as an unfinished turn.
	reply func(bot string, n int) string
	gate  chan struct{} // if set, every run waits until it can receive
	// keepSteers: runs end with every accepted steer still in pending_steer.
	keepSteers bool
	imageFail  bool // SendImage fails
	seq        int
}

type harnessOpts struct {
	maxTurns        int
	backgroundAfter time.Duration
	runTimeout      time.Duration
	typingEvery     time.Duration
	decider         func(bots []string) turns.Decider // default: the plain rules
}

// startServer starts the fake Runs server; runs follow h.reply.
func (h *harness) startServer() {
	h.srv = hermestest.New(h.t)
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel) // runs before the server closes: background runs stop polling
	h.ctx = ctx
	h.srv.Handle = func(s *hermestest.Server, r hermestest.Run) {
		h.mu.Lock()
		h.calls = append(h.calls, call{r.Profile, r.SessionID, r.Instructions, unframe(h.t, r.Input), r.Input, r.ID})
		n, gate, reply, keep := len(h.calls), h.gate, h.reply, h.keepSteers
		h.mu.Unlock()
		if gate != nil {
			<-gate
		}
		switch text := reply(r.Profile, n); {
		case strings.HasPrefix(text, "PARTIAL:"):
			s.FinishPartial(r.ID, strings.TrimPrefix(text, "PARTIAL:"))
		case text == "HANG":
		case text == "ERR":
			s.Finish(r.ID, "failed", "boom", !keep)
		default:
			s.Finish(r.ID, "completed", text, !keep)
		}
	}
}

// runner is bot's Hermes profile on the fake server.
func (h *harness) runner(bot string) Runner {
	c := hermes.New(h.srv.BaseURL(bot), "k", "")
	return c
}

// unframe returns the attributed lines inside Frame's wrapper.
func unframe(t *testing.T, msg string) string {
	if i := strings.Index(msg, "</memory>\n\n"); i >= 0 {
		msg = msg[i+len("</memory>\n\n"):]
	}
	if strings.HasPrefix(msg, "There are no new messages") {
		return ""
	}
	start, end := strings.Index(msg, "\n\n"), strings.Index(msg, "\n</messages>\n\n")+len("\n</messages>")
	if start < 0 || end <= start {
		t.Errorf("message not framed: %q", msg)
		return msg
	}
	inner := msg[start+2 : end]
	return strings.TrimSuffix(strings.TrimPrefix(inner, "<messages>\n"), "\n</messages>")
}

type fakePoster struct {
	h   *harness
	bot string
}

func (p fakePoster) Send(_ context.Context, _ string, text string) (string, error) {
	p.h.mu.Lock()
	defer p.h.mu.Unlock()
	p.h.posts = append(p.h.posts, p.bot+": "+text)
	p.h.seq++
	return fmt.Sprintf("$bot%d", p.h.seq), nil
}

func (p fakePoster) SendReply(_ context.Context, _ string, text, inReplyTo string) (string, error) {
	p.h.mu.Lock()
	defer p.h.mu.Unlock()
	p.h.posts = append(p.h.posts, p.bot+": "+text+" ↩ "+inReplyTo)
	p.h.seq++
	return fmt.Sprintf("$bot%d", p.h.seq), nil
}

func (p fakePoster) SendImage(_ context.Context, _ string, img media.Image, inReplyTo string) (string, error) {
	p.h.mu.Lock()
	defer p.h.mu.Unlock()
	if p.h.imageFail {
		return "", fmt.Errorf("M_TOO_LARGE")
	}
	post := fmt.Sprintf("%s: <image %s %dB>", p.bot, img.MIME, len(img.Data))
	if inReplyTo != "" {
		post += " ↩ " + inReplyTo
	}
	p.h.posts = append(p.h.posts, post)
	p.h.seq++
	return fmt.Sprintf("$img%d", p.h.seq), nil
}

func (p fakePoster) SendNotice(_ context.Context, _ string, text string) (string, error) {
	p.h.mu.Lock()
	defer p.h.mu.Unlock()
	p.h.notices = append(p.h.notices, p.bot+": "+text)
	p.h.seq++
	return fmt.Sprintf("$notice%d", p.h.seq), nil
}

func (p fakePoster) React(_ context.Context, _ string, eventID, key string) error {
	p.h.mu.Lock()
	defer p.h.mu.Unlock()
	p.h.reacts = append(p.h.reacts, p.bot+": "+eventID+" "+key)
	return nil
}

func (p fakePoster) Typing(_ context.Context, _ string, on bool, _ time.Duration) error {
	p.h.mu.Lock()
	defer p.h.mu.Unlock()
	state := "off"
	if on {
		state = "on"
	}
	p.h.typing = append(p.h.typing, p.bot+": "+state)
	return nil
}

func newHarness(t *testing.T, maxTurns int, reply func(bot string, n int) string) *harness {
	return newHarnessWith(t, harnessOpts{maxTurns: maxTurns}, reply)
}

func newHarnessWith(t *testing.T, ho harnessOpts, reply func(bot string, n int) string) *harness {
	t.Helper()
	store, err := transcript.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	r := roster.New("@alex:z", "Alex", []roster.Person{
		{Key: "ada", Display: "Ada", MXID: "@ada:z"},
		{Key: "cleo", Display: "Cleo", MXID: "@cleo:z"},
	})
	h := &harness{t: t, reply: reply}
	h.startServer()
	var bots []*Bot
	for _, k := range []string{"ada", "cleo"} {
		bots = append(bots, &Bot{Key: k, Poster: fakePoster{h, k}, Hermes: h.runner(k), System: "SYS-" + k})
	}
	var decider turns.Decider = turns.NewRules([]string{"ada", "cleo"}, ho.maxTurns, rand.New(rand.NewPCG(1, 1)))
	if ho.decider != nil {
		decider = ho.decider([]string{"ada", "cleo"})
	}
	h.o = New(Options{
		Store: store, Roster: r, Decider: decider,
		Bots: bots, Rooms: []string{roomID}, MaxDeltaLines: 40,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		PollInterval: 2 * time.Millisecond, RunTimeout: ho.runTimeout,
		TypingEvery: or0(ho.typingEvery, time.Hour), BackgroundAfter: ho.backgroundAfter,
	})
	return h
}

var evn int

func (h *harness) user(body string) matrixio.Message {
	evn++
	return matrixio.Message{RoomID: roomID, EventID: fmt.Sprintf("$u%d", evn), Sender: "@alex:z", Body: body, TS: time.Now().UnixMilli()}
}

// runOne records a user message and runs its chain synchronously.
func (h *harness) runOne(body string) matrixio.Message {
	m := h.user(body)
	h.o.Handle(h.ctx, m)
	w := h.o.rooms[roomID]
	q := <-w.inbox
	w.state.UserSpoke()
	h.o.chain(h.ctx, w, q)
	return m
}

func TestChainNamedBotThenOtherThenStop(t *testing.T) {
	h := newHarness(t, 2, func(bot string, n int) string { return "reply from " + bot })
	h.runOne("@Cleo what's up?")
	if want := []string{"cleo: reply from cleo", "ada: reply from ada"}; strings.Join(h.posts, "|") != strings.Join(want, "|") {
		t.Fatalf("posts %v, want %v", h.posts, want)
	}
	if h.calls[0].delta != "<msg from=\"Alex\">(to Cleo) what's up?</msg>" {
		t.Errorf("cleo delta %q", h.calls[0].delta)
	}
	if h.calls[1].delta != "<msg from=\"Alex\">(to Cleo) what's up?</msg>\n<msg from=\"Cleo\">reply from cleo</msg>" {
		t.Errorf("ada delta %q", h.calls[1].delta)
	}
	if h.calls[0].session != SessionID(roomID, "cleo", 1) || h.calls[0].system != "SYS-cleo" {
		t.Errorf("cleo call %+v", h.calls[0])
	}

	// Next user message: each bot gets only what it has not seen, never its own lines.
	h.runOne("Ada, short answer please")
	if got := h.calls[2]; got.bot != "ada" || got.delta != "<msg from=\"Alex\">Ada, short answer please</msg>" {
		t.Errorf("ada second delta %+v", got)
	}
	if got := h.calls[3]; got.bot != "cleo" || got.delta != "<msg from=\"Ada\">reply from ada</msg>\n<msg from=\"Alex\">Ada, short answer please</msg>\n<msg from=\"Ada\">reply from ada</msg>" {
		t.Errorf("cleo second delta %q", got.delta)
	}
}

func TestSilentReplyIsNotATurnButAdvancesCursor(t *testing.T) {
	h := newHarness(t, 2, func(bot string, n int) string {
		if n == 1 {
			return "[SILENT]"
		}
		return "hi from " + bot
	})
	h.runOne("Ada?")
	if len(h.posts) != 0 {
		t.Fatalf("silence posted %v", h.posts)
	}
	h.runOne("Ada, now?")
	if got := h.calls[1].delta; got != "<msg from=\"Alex\">Ada, now?</msg>" {
		t.Fatalf("delta after silence re-sent old lines: %q", got)
	}
}

func TestFailedCallKeepsCursor(t *testing.T) {
	h := newHarness(t, 1, func(bot string, n int) string {
		if n == 1 {
			return "ERR"
		}
		return "ok"
	})
	h.runOne("Ada?")
	h.runOne("Ada, again")
	if got := h.calls[1].delta; got != "<msg from=\"Alex\">Ada?</msg>\n<msg from=\"Alex\">Ada, again</msg>" {
		t.Fatalf("failed delta not re-offered: %q", got)
	}
}

func TestUserMidTurnStopsChainAndReachesBot(t *testing.T) {
	h := newHarness(t, 2, func(bot string, n int) string { return "reply " + bot })
	h.gate = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.o.Run(ctx)

	h.o.Handle(ctx, h.user("Ada, tell me a story"))
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.calls) == 1 })
	h.o.Handle(ctx, h.user("actually, Cleo first")) // arrives while Ada is thinking
	h.gate <- struct{}{}                            // Ada's reply finishes and is posted
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.calls) == 2 })
	h.mu.Lock()
	second := h.calls[1]
	h.mu.Unlock()
	if second.bot != "cleo" {
		t.Fatalf("after interruption the user's new addressee should speak, got %s", second.bot)
	}
	h.gate <- struct{}{}
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.calls) == 3 })
	h.mu.Lock()
	third := h.calls[2]
	h.mu.Unlock()
	if third.bot != "ada" || !strings.Contains(third.delta, "<msg from=\"Alex\">actually, Cleo first</msg>") {
		t.Fatalf("ada must still see the message that arrived mid-turn: %+v", third)
	}
	h.gate <- struct{}{}
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.posts) == 3 })
}

func TestBotMessagesNeverTrigger(t *testing.T) {
	h := newHarness(t, 2, func(bot string, n int) string { return "x" })
	h.o.Handle(context.Background(), matrixio.Message{RoomID: roomID, EventID: "$b", Sender: "@ada:z", Body: "@Cleo hey", TS: 1})
	h.o.Handle(context.Background(), matrixio.Message{RoomID: "!other:z", EventID: "$o", Sender: "@alex:z", Body: "hi", TS: 1})
	if n := len(h.o.rooms[roomID].inbox); n != 0 {
		t.Fatalf("bot message queued a trigger")
	}
}

func TestSessionIDShape(t *testing.T) {
	id := SessionID("!abc:example.com", "ada", 3)
	if !strings.HasPrefix(id, "grp-") || !strings.HasSuffix(id, "-ada-g3") || len(id) != len("grp-")+12+len("-ada-g3") {
		t.Fatalf("bad session id %q", id)
	}
	if strings.ContainsAny(id, "!:/\\") {
		t.Fatalf("session id not path-safe: %q", id)
	}
	if SessionID("!abc:example.com", "ada", 3) != id {
		t.Fatal("session id not stable")
	}
}

func TestSystemPrompt(t *testing.T) {
	r := roster.New("@alex:z", "Alex", []roster.Person{
		{Key: "ada", Display: "Ada", MXID: "@ada:z"},
		{Key: "cleo", Display: "Cleo", MXID: "@cleo:z"},
	})
	got := SystemPrompt("You are {{name}}. [{{user}}] is the user.\n{{others}}", r, "ada", map[string]string{"cleo": "a singer"})
	if want := "You are Ada. [Alex] is the user.\n- Cleo: a singer"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStripSelfLabel(t *testing.T) {
	tests := []struct{ in, want string }{
		{"[Ada] hello", "hello"},
		{"ada: hello", "hello"},
		{"[Ada] [Ada] twice", "[Ada] twice"},
		{"Ada is my name", "Ada is my name"},
		{"[Cleo] not mine", "[Cleo] not mine"},
		{"[Ada]", ""},
	}
	for _, tt := range tests {
		if got := StripSelfLabel(tt.in, "Ada"); got != tt.want {
			t.Errorf("StripSelfLabel(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCleanReply(t *testing.T) {
	r := roster.New("@alex:z", "Alex", []roster.Person{
		{Key: "ada", Display: "Ada", MXID: "@ada:z"},
		{Key: "cleo", Display: "Cleo", MXID: "@cleo:z"},
	})
	alex := `<msg from="Alex">hi everyone</msg>`
	ada := `<msg from="Ada">evening.</msg>`
	delta := alex + "\n" + ada
	tests := []struct{ name, in, want string }{
		{"plain reply", "Hi Alex!", "Hi Alex!"},
		{"pure echo posts nothing", alex, ""},
		{"echo of whole delta posts nothing", delta, ""},
		{"echo wrapped in messages tags posts nothing", "<messages>\n" + delta + "\n</messages>", ""},
		{"echo then own element", alex + "\n" + `<msg from="Cleo">Hi Alex!</msg>`, "Hi Alex!"},
		{"own element unescaped", `<msg from="Cleo">Tea &amp; cake?</msg>`, "Tea & cake?"},
		{"own reply then invented message for another", "Hello!\n" + `<msg from="Ada">I agree.</msg>`, "Hello!"},
		{"invented message for the user", `<msg from="Alex">tell me more</msg>`, ""},
		{"invented message first, then own text", `<msg from="Ada">sure</msg>` + "\nMe too.", "Me too."},
		{"unclosed element for another cuts the rest", "Hello!\n" + `<msg from="Ada">I agree`, "Hello!"},
		{"own label stripped", "Cleo: Morning.", "Morning."},
		{"own bracket label stripped", "[Cleo] Morning.", "Morning."},
		{"multi-line own reply kept", "Line one.\nLine two.", "Line one.\nLine two."},
		{"angle brackets in prose kept", "I <3 this", "I <3 this"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CleanReply(tt.in, "Cleo", r, delta); got != tt.want {
				t.Fatalf("CleanReply(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFrameEndsWithInstruction(t *testing.T) {
	got := Frame(`<msg from="Alex">hi</msg>`, "Cleo")
	if !strings.Contains(got, "<messages>\n<msg from=\"Alex\">hi</msg>\n</messages>") || !strings.Contains(got, "Reply as Cleo") {
		t.Fatalf("frame = %q", got)
	}
}
