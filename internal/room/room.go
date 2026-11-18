// Package room is the orchestrator: it records every room message, and for
// each message from the user runs one serialized chain of bot turns
// (decide → build delta → start a Hermes run → post as the bot → decide
// again).
//
// One worker goroutine per room means one turn in flight per room. A user
// message that arrives mid-turn lets the in-flight reply finish and post,
// then stops the chain; the worker then handles that newest user message.
//
// A run that is still going after the room's background_after hands off to
// the background: the listener posts one notice, the bot is marked busy in
// that room (never picked for a turn), and the room goes on. When the run
// ends, its result is posted as a reply to the message that asked for it,
// without starting a chain. A user message that names a busy bot is steered
// into its run instead of starting a chain.
//
// The user's commands (/reset, /explain) are never recorded as conversation.
// They go through the same worker queue, so they never race a turn, and they
// are never coalesced away by a later message.
package room

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ziliding/hermes-roomd/internal/commands"
	"github.com/ziliding/hermes-roomd/internal/contextbuild"
	"github.com/ziliding/hermes-roomd/internal/hermes"
	"github.com/ziliding/hermes-roomd/internal/matrixio"
	"github.com/ziliding/hermes-roomd/internal/media"
	"github.com/ziliding/hermes-roomd/internal/roster"
	"github.com/ziliding/hermes-roomd/internal/transcript"
	"github.com/ziliding/hermes-roomd/internal/turns"
)

// Poster posts into a room as one bot. SendNotice posts roomd's own status
// text, tagged so the listener never records its echo as conversation (see
// matrixio.SendNotice).
type Poster interface {
	Send(ctx context.Context, roomID, text string) (string, error)
	SendReply(ctx context.Context, roomID, text, inReplyTo string) (string, error)
	// SendImage uploads an image as the bot and posts it as an m.image
	// (a reply to inReplyTo when that is set).
	SendImage(ctx context.Context, roomID string, img media.Image, inReplyTo string) (string, error)
	SendNotice(ctx context.Context, roomID, text string) (string, error)
	React(ctx context.Context, roomID, eventID, key string) error
	Typing(ctx context.Context, roomID string, typing bool, timeout time.Duration) error
	// Download fetches an mxc:// URI as the bot, refusing more than maxBytes.
	Download(ctx context.Context, mxc string, maxBytes int64) ([]byte, error)
}

// Runner drives runs on one bot's Hermes profile (see hermes.Client).
type Runner interface {
	Start(ctx context.Context, sessionID, instructions, input string, images ...hermes.Image) (string, error)
	Status(ctx context.Context, runID string) (hermes.Run, error)
	Steer(ctx context.Context, runID, text string) error
	Stop(ctx context.Context, runID string) error
	ResolveUnknown(ctx context.Context, run hermes.Run) error
}

// Memory supplies the recollections block for one bot turn ("" for none)
// and the ids of the memories in it. It must never fail a turn: errors are
// its own to log. Delivered is called only after the run succeeded (Hermes
// then holds the block), so a failed run offers them again. Delivered may be
// called from a background goroutine.
type Memory interface {
	Block(ctx context.Context, roomID, sessionID string) (string, []string)
	Delivered(sessionID string, ids []string)
}

// Bot is everything the orchestrator needs to make one character speak.
type Bot struct {
	Key    string
	Poster Poster
	Hermes Runner
	System string // byte-identical on every call (keeps Hermes' prompt cache warm)
}

// Defaults for Options left zero.
const (
	DefaultRunTimeout    = 5 * time.Minute
	DefaultPollInterval  = time.Second
	DefaultTypingTimeout = 30 * time.Second
	DefaultTypingEvery   = 25 * time.Second
	// SteerAck is the reaction a bot puts on a message steered into its run.
	SteerAck = "✓"
)

// Orchestrator owns all configured rooms.
type Orchestrator struct {
	store    *transcript.Store
	maxDelta int
	log      *slog.Logger
	now      func() time.Time

	runTimeout    time.Duration
	pollInterval  time.Duration
	typingTimeout time.Duration
	typingEvery   time.Duration
	files         *media.Files // nil: image files named in replies are never read

	rooms map[string]*worker
	wg    sync.WaitGroup // room workers and their runs
}

// worker is one room: its cast and settings, plus its serialized turn queue.
type worker struct {
	id              string
	roster          *roster.Roster // the user plus this room's cast only
	builder         *contextbuild.Builder
	decider         turns.Decider
	memory          Memory // nil: no group memory in this room
	bots            map[string]*Bot
	botKeys         []string      // this room's cast, stable order
	notifier        string        // bot key that posts roomd's notices in this room
	backgroundAfter time.Duration // 0: never hand a run off to the background

	inbox     chan queued
	userEpoch atomic.Int64 // bumped by every user message and command as it arrives
	resets    atomic.Int64 // bumped by every /reset as it arrives
	state     turns.State  // touched only by the worker goroutine

	mu   sync.Mutex
	busy map[string]*job // bot key -> its background run
}

// RoomSpec is one room's cast and settings. Each Bot's System is that
// room's rendered rules for that bot (byte-identical on every call in the
// room).
type RoomSpec struct {
	ID      string
	Roster  *roster.Roster // the user plus this room's cast
	Decider turns.Decider  // this room's turn engine (cast, max_bot_turns)
	Bots    []*Bot         // this room's cast
	// Notifier is the bot key whose Poster posts roomd's notices in this
	// room (its listener). Default: the first bot.
	Notifier string
	Memory   Memory // nil: no group memory in this room
	// BackgroundAfter hands a run that is still going after this long to the
	// background. 0 disables it: the room waits for every run.
	BackgroundAfter time.Duration
}

type queued struct {
	msg   matrixio.Message
	seq   int64 // the message's transcript seq (0 for commands)
	epoch int64
	cmd   *commands.Command // non-nil for a command
	upTo  int64             // newest seq when the command arrived (for /reset)
}

// Options configures New. Each room comes from RoomSpecs; the rooms listed
// in Rooms (if any) all share Roster, Decider, Bots, Notifier, Memory and
// BackgroundAfter.
type Options struct {
	Store         *transcript.Store
	RoomSpecs     []RoomSpec
	MaxDeltaLines int
	Log           *slog.Logger

	// RunTimeout is the most a run may take (hermes_timeout); it is then
	// stopped. PollInterval is how often a run's status is read. The typing
	// indicator is sent with TypingTimeout and refreshed every TypingEvery.
	RunTimeout    time.Duration
	PollInterval  time.Duration
	TypingTimeout time.Duration
	TypingEvery   time.Duration
	// Files reads image files a reply names (MEDIA: tags, bare paths), only
	// from the allowed directories. nil: never.
	Files *media.Files

	Rooms           []string
	Roster          *roster.Roster
	Decider         turns.Decider
	Bots            []*Bot
	Notifier        string
	Memory          Memory
	BackgroundAfter time.Duration
}

// New builds an orchestrator; call Run to start the room workers.
func New(o Options) *Orchestrator {
	or := &Orchestrator{
		store: o.Store, maxDelta: o.MaxDeltaLines,
		log: o.Log, now: time.Now, rooms: map[string]*worker{},
		runTimeout: or0(o.RunTimeout, DefaultRunTimeout), pollInterval: or0(o.PollInterval, DefaultPollInterval),
		typingTimeout: or0(o.TypingTimeout, DefaultTypingTimeout), typingEvery: or0(o.TypingEvery, DefaultTypingEvery),
		files: o.Files,
	}
	specs := append([]RoomSpec{}, o.RoomSpecs...)
	for _, r := range o.Rooms {
		specs = append(specs, RoomSpec{ID: r, Roster: o.Roster, Decider: o.Decider, Bots: o.Bots, Notifier: o.Notifier,
			Memory: o.Memory, BackgroundAfter: o.BackgroundAfter})
	}
	for _, sp := range specs {
		w := &worker{
			id: sp.ID, roster: sp.Roster, builder: contextbuild.New(sp.Roster), decider: sp.Decider,
			memory: sp.Memory, bots: map[string]*Bot{}, notifier: sp.Notifier, inbox: make(chan queued, 64),
			backgroundAfter: sp.BackgroundAfter, busy: map[string]*job{},
		}
		for _, b := range sp.Bots {
			w.bots[b.Key] = b
			w.botKeys = append(w.botKeys, b.Key)
		}
		if w.notifier == "" && len(w.botKeys) > 0 {
			w.notifier = w.botKeys[0]
		}
		or.rooms[sp.ID] = w
	}
	return or
}

func or0(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Run starts one worker per room and blocks until ctx is done and they, and
// every run they are following, exit. A run still going at shutdown is
// logged and left to Hermes; its result is not posted.
func (o *Orchestrator) Run(ctx context.Context) {
	for _, w := range o.rooms {
		o.wg.Add(1)
		go func() {
			defer o.wg.Done()
			o.work(ctx, w)
		}()
	}
	<-ctx.Done()
	o.wg.Wait()
}

// Handle records a message from the listener and, if it is the user's,
// queues it as a trigger. Bot messages (including roomd's own echoes) are
// recorded but never trigger turns.
func (o *Orchestrator) Handle(ctx context.Context, m matrixio.Message) {
	w, ok := o.rooms[m.RoomID]
	if !ok {
		return
	}
	if m.Control {
		o.log.Debug("dropped roomd's own notice", "room", m.RoomID, "event", m.EventID)
		return
	}
	if m.Sender == w.roster.User().MXID && m.Image == nil {
		if cmd, ok := commands.Parse(m.Body); ok {
			o.enqueueCommand(ctx, w, m, cmd)
			return
		}
	}
	seq, err := o.store.Append(ctx, entryOf(m))
	if err != nil {
		o.log.Error("recording message", "room", m.RoomID, "event", m.EventID, "err", err)
		return
	}
	if m.Sender != w.roster.User().MXID {
		o.log.Debug("recorded; not a trigger (sender is not the configured user)", "room", m.RoomID, "sender", m.Sender)
		return
	}
	epoch := w.userEpoch.Add(1)
	select {
	case w.inbox <- queued{msg: m, seq: seq, epoch: epoch}:
	default:
		// Inbox full: the message is recorded and will reach bots as context;
		// the newest queued user message still triggers.
		o.log.Warn("room inbox full; message recorded without its own trigger", "room", m.RoomID)
	}
}

// enqueueCommand queues a user command without recording it. Like any user
// message it stops a running chain; a /reset also discards a reply that is
// still being generated in the foreground, since it belongs to the old
// sessions.
func (o *Orchestrator) enqueueCommand(ctx context.Context, w *worker, m matrixio.Message, cmd commands.Command) {
	cmd.FormattedBody = m.FormattedBody
	upTo, err := o.store.LatestSeq(ctx, m.RoomID)
	if err != nil {
		o.log.Error("reading latest seq for command", "room", m.RoomID, "cmd", cmd.Kind, "err", err)
		return
	}
	if cmd.Kind == commands.Reset {
		w.resets.Add(1)
	}
	epoch := w.userEpoch.Add(1)
	o.log.Info("command received", "room", m.RoomID, "cmd", cmd.Kind, "arg", cmd.Arg)
	select {
	case w.inbox <- queued{msg: m, epoch: epoch, cmd: &cmd, upTo: upTo}:
	default:
		o.log.Warn("room inbox full; command dropped", "room", m.RoomID, "cmd", cmd.Kind)
	}
}

func (o *Orchestrator) work(ctx context.Context, w *worker) {
	for {
		select {
		case <-ctx.Done():
			return
		case q := <-w.inbox:
			if q.cmd != nil {
				o.command(ctx, w, q)
				continue
			}
			// Before coalescing: a message to a busy bot is steered even when
			// a newer one is already queued.
			if o.steer(ctx, w, q) {
				w.state.UserSpoke()
				continue
			}
			if q.epoch != w.userEpoch.Load() {
				continue // a newer user message is queued; it carries the context
			}
			w.state.UserSpoke()
			o.chain(ctx, w, q)
		}
	}
}

// command runs one user command on the room's worker.
func (o *Orchestrator) command(ctx context.Context, w *worker, q queued) {
	log := o.log.With("room", w.id, "cmd", q.cmd.Kind)
	w.state.UserSpoke()
	switch q.cmd.Kind {
	case commands.Reset:
		if err := commands.RunReset(ctx, o.store, w.id, w.botKeys, q.upTo); err != nil {
			log.Error("reset failed", "err", err)
			o.notice(ctx, w, "Reset failed; nothing was changed. Check the roomd log.")
			return
		}
		// The conversation so far is one memory segment; what follows is the next.
		if err := o.store.AddSegmentBreak(ctx, w.id, q.upTo); err != nil {
			log.Error("recording the memory segment break", "err", err)
		}
		log.Info("reset: every bot starts a new session generation", "cursor", q.upTo)
		o.notice(ctx, w, commands.ResetNotice)
	case commands.Explain:
		var mxids []string
		for _, p := range w.roster.Bots() {
			mxids = append(mxids, p.MXID)
		}
		last, err := o.store.LastFrom(ctx, w.id, mxids)
		if err != nil {
			log.Error("reading last speaker", "err", err)
			return
		}
		bot, err := commands.ExplainTarget(w.roster, *q.cmd, last)
		if err != nil {
			log.Info("explain: no target", "err", err)
			o.notice(ctx, w, "!explain: "+err.Error()+". Name a character, e.g. !explain "+w.roster.Bots()[0].Display+".")
			return
		}
		if w.jobFor(bot) != nil {
			log.Info("explain: the bot is busy", "bot", bot)
			o.notice(ctx, w, display(w, bot)+" is busy with a task; try !explain again when it has answered.")
			return
		}
		log.Info("explain", "bot", bot)
		// One called turn: no router, no chain after it.
		if _, out := o.speak(ctx, w, bot, commands.ExplainInstruction(w.roster.User().Display), q.msg.EventID); out == spoke {
			w.state.BotSpoke(bot)
		}
	}
}

// notice posts roomd's own status text as the room's notifier (its listener).
func (o *Orchestrator) notice(ctx context.Context, w *worker, text string) {
	roomID := w.id
	bot, ok := w.bots[w.notifier]
	if !ok {
		return
	}
	if _, err := bot.Poster.SendNotice(ctx, roomID, text); err != nil {
		o.log.Error("posting notice", "room", roomID, "err", err)
	}
}

func display(w *worker, botKey string) string {
	if p, ok := w.roster.ByKey(botKey); ok && p != nil {
		return p.Display
	}
	return botKey
}

func (w *worker) jobFor(bot string) *job {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.busy[bot]
}

func (w *worker) busyBots() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, k := range w.botKeys {
		if w.busy[k] != nil {
			out = append(out, k)
		}
	}
	return out
}

func (o *Orchestrator) chain(ctx context.Context, w *worker, q queued) {
	trig := turns.Trigger{
		RoomID:    w.id,
		FromUser:  true,
		Addressee: w.roster.Addressee(q.msg.Body, q.msg.FormattedBody, q.msg.Mentions, ""),
		Body:      q.msg.Body,
	}
	for ctx.Err() == nil {
		if w.userEpoch.Load() != q.epoch {
			o.log.Info("user spoke mid-chain; stopping", "room", w.id)
			return
		}
		st := w.state
		st.Busy = w.busyBots()
		next, ok, err := w.decider.Next(ctx, st, trig)
		if err != nil {
			o.log.Error("turn decision failed", "room", w.id, "err", err)
			return
		}
		if !ok {
			o.log.Debug("turn decision: nobody speaks", "room", w.id, "bot_turns", w.state.BotTurns, "busy", st.Busy)
			return
		}
		if w.userEpoch.Load() != q.epoch {
			// The user (or a command) arrived while the decider was thinking
			// (the LLM router can take seconds): that decision is stale.
			o.log.Info("user spoke while deciding; stopping", "room", w.id)
			return
		}
		o.log.Debug("turn decision", "room", w.id, "next", next, "addressee", trig.Addressee, "bot_turns", w.state.BotTurns)
		text, out := o.speak(ctx, w, next, "", q.msg.EventID)
		if out != spoke {
			// Silence or failure ends the chain; it is not a turn. A run handed
			// to the background ends it too: its result starts no chain.
			return
		}
		w.state.BotSpoke(next)
		trig = turns.Trigger{
			RoomID:    w.id,
			Sender:    next,
			Addressee: w.roster.Addressee(text, "", nil, next),
			Body:      text,
		}
	}
}

// steer handles a user message that names a busy bot: it is steered into
// that bot's run (acknowledged with a ✓ reaction from the bot) and starts
// no chain. If Hermes refuses the steer, or the run ends without consuming
// it, the message simply reaches the bot in its next delta. It reports
// whether the message was addressed to a busy bot.
func (o *Orchestrator) steer(ctx context.Context, w *worker, q queued) bool {
	addr := w.roster.Addressee(q.msg.Body, q.msg.FormattedBody, q.msg.Mentions, "")
	if addr == "" {
		return false
	}
	j := w.jobFor(addr)
	if j == nil {
		return false
	}
	log := o.log.With("room", w.id, "bot", addr, "run", j.runID, "event", q.msg.EventID)
	if q.msg.Image != nil {
		// Steer input is text only: the image waits for the bot's next turn.
		log.Info("image for a busy bot; it waits for the bot's next turn")
		return true
	}
	e := entryOf(q.msg)
	e.Seq = q.seq
	text := w.builder.Build(addr, []transcript.Entry{e})
	if text == "" {
		return true
	}
	req := steerReq{seq: q.seq, text: text, reply: make(chan error, 1)}
	var err error
	select {
	case j.steers <- req:
		err = <-req.reply
	case <-j.polling:
		err = fmt.Errorf("%w: the run already ended", hermes.ErrSteerRefused)
	case <-ctx.Done():
		return true
	}
	if err != nil {
		log.Info("steer not delivered; the message waits for the bot's next turn", "err", err)
		return true
	}
	log.Info("steered a message into the bot's background run")
	if err := w.bots[addr].Poster.React(ctx, w.id, q.msg.EventID, SteerAck); err != nil {
		log.Warn("reacting to a steered message", "err", err)
	}
	return true
}

// outcome is what one call to speak led to.
type outcome int

const (
	silent       outcome = iota // nothing posted (silence, failure, nothing new)
	spoke                       // a reply was posted
	backgrounded                // the run continues in the background
)

// job is one bot's run. Its goroutine (follow) alone polls the run, keeps
// the typing indicator up and sends steers, so a steer can never race the
// run's end.
type job struct {
	bot     string
	runID   string
	sid     string
	upTo    int64    // the delta's last seq: the cursor once the run succeeds
	delta   string   // for CleanReply
	memIDs  []string // memories in the input, delivered on success
	replyTo string   // the message that started the chain (background replies)
	resets  int64    // w.resets when the run started
	start   time.Time

	steers    chan steerReq
	polling   chan struct{} // closed once the run has ended: no more steers
	done      chan struct{} // closed when the result is ready for the worker
	announced chan struct{} // closed once the hand-off notice is posted

	// Set by follow before done is closed (or before it finishes the job itself).
	run     hermes.Run
	err     error
	steered []steerReq // accepted by Hermes

	backgrounded bool // guarded by worker.mu
}

type steerReq struct {
	seq   int64
	text  string
	reply chan error
}

// speak makes one bot take one turn. It reports the posted text and the
// outcome. A non-empty instruction (used by /explain) is added to the
// framed delta, and the bot is called even when it has nothing new.
// replyTo is the message a background result replies to.
func (o *Orchestrator) speak(ctx context.Context, w *worker, botKey, instruction, replyTo string) (string, outcome) {
	roomID := w.id
	resets := w.resets.Load()
	bot, ok := w.bots[botKey]
	person, _ := w.roster.ByKey(botKey)
	if !ok || person == nil {
		o.log.Error("decider chose unknown bot", "bot", botKey)
		return "", silent
	}
	log := o.log.With("room", roomID, "bot", botKey)
	if w.jobFor(botKey) != nil {
		log.Info("bot is busy with a background task; not calling it")
		return "", silent
	}

	cursor, err := o.store.Cursor(ctx, roomID, botKey)
	if err != nil {
		log.Error("reading cursor", "err", err)
		return "", silent
	}
	entries, err := o.store.Since(ctx, roomID, cursor, o.maxDelta)
	if err != nil {
		log.Error("reading transcript", "err", err)
		return "", silent
	}
	if len(entries) == 0 && instruction == "" {
		log.Debug("nothing new for this bot", "cursor", cursor)
		return "", silent
	}
	upTo := cursor
	if len(entries) > 0 {
		upTo = entries[len(entries)-1].Seq
	}
	steered, err := o.store.Steered(ctx, roomID, botKey, cursor)
	if err != nil {
		log.Error("reading steered messages", "err", err)
		return "", silent
	}
	var fresh []transcript.Entry // what the bot hasn't seen: not already steered into a run
	for _, e := range entries {
		if !steered[e.Seq] {
			fresh = append(fresh, e)
		}
	}
	delta := w.builder.Build(botKey, fresh)
	if delta == "" && instruction == "" {
		log.Debug("delta empty after filtering (own lines / silence / steered)")
		_ = o.store.SetCursor(ctx, roomID, botKey, upTo)
		return "", silent
	}
	gen, err := o.store.Generation(ctx, roomID, botKey)
	if err != nil {
		log.Error("reading session generation", "err", err)
		return "", silent
	}
	sid := SessionID(roomID, botKey, gen)

	log.Info("calling hermes", "session", sid, "delta_lines", strings.Count(delta, "\n")+1)
	log.Debug("delta", "text", delta)
	var memory string
	var memIDs []string
	if w.memory != nil {
		memory, memIDs = w.memory.Block(ctx, roomID, sid)
	}
	start := o.now()
	images, notes := o.images(ctx, w, bot.Poster, person.MXID, fresh, log)
	input := FrameMemory(delta, person.Display, instruction, memory)
	if len(images) > 0 || len(notes) > 0 {
		input += "\n\n" + imagesNote(len(images), notes)
	}
	runID, err := bot.Hermes.Start(ctx, sid, bot.System, input, images...)
	if err != nil {
		// Cursor stays put: the same delta is offered next time.
		log.Error("starting hermes run", "session", sid, "err", err)
		return "", silent
	}
	j := &job{
		bot: botKey, runID: runID, sid: sid, upTo: upTo, delta: delta, memIDs: memIDs,
		replyTo: replyTo, resets: resets, start: start,
		steers: make(chan steerReq), polling: make(chan struct{}), done: make(chan struct{}), announced: make(chan struct{}),
	}
	o.wg.Add(1)
	go o.follow(ctx, w, j)

	var handoff <-chan time.Time
	if w.backgroundAfter > 0 {
		t := time.NewTimer(w.backgroundAfter)
		defer t.Stop()
		handoff = t.C
	}
	select {
	case <-j.done:
		return o.finish(ctx, w, j, false)
	case <-ctx.Done():
		return "", silent // follow logs the run left in flight
	case <-handoff:
	}
	w.mu.Lock()
	select {
	case <-j.done: // finished just as the timer fired
		w.mu.Unlock()
		return o.finish(ctx, w, j, false)
	default:
	}
	j.backgrounded = true
	w.busy[botKey] = j
	w.mu.Unlock()
	log.Info("run handed off to the background", "session", sid, "run", runID, "after", w.backgroundAfter)
	o.notice(ctx, w, person.Display+" is working on it…")
	close(j.announced)
	return "", backgrounded
}

// follow polls one run until it ends, keeping the bot shown as typing and
// passing steers to Hermes. On hermes_timeout it stops the run. A foreground
// run's result goes back to the worker; a background run is finished here.
func (o *Orchestrator) follow(ctx context.Context, w *worker, j *job) {
	defer o.wg.Done()
	bot := w.bots[j.bot]
	log := o.log.With("room", w.id, "bot", j.bot, "run", j.runID)
	// Calls that must still go out when ctx is cancelled (typing off, /stop).
	bg := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	}
	typing := func(c context.Context, on bool) {
		if err := bot.Poster.Typing(c, w.id, on, o.typingTimeout); err != nil {
			log.Debug("typing indicator", "on", on, "err", err)
		}
	}
	typing(ctx, true)
	typingTick := time.NewTicker(o.typingEvery)
	defer typingTick.Stop()
	poll := time.NewTicker(o.pollInterval)
	defer poll.Stop()
	deadline := time.NewTimer(o.runTimeout - o.now().Sub(j.start))
	defer deadline.Stop()

	var run hermes.Run
	var runErr error
loop:
	for {
		select {
		case <-ctx.Done():
			close(j.polling)
			c, cancel := bg()
			typing(c, false)
			cancel()
			log.Warn("roomd is stopping while a run is in flight; it continues in Hermes, but its result will not be posted",
				"session", j.sid, "running_for", o.now().Sub(j.start).Round(time.Second))
			return
		case <-typingTick.C:
			typing(ctx, true)
		case req := <-j.steers:
			err := bot.Hermes.Steer(ctx, j.runID, req.text)
			if err == nil {
				j.steered = append(j.steered, req)
			}
			req.reply <- err
		case <-deadline.C:
			c, cancel := bg()
			if err := bot.Hermes.Stop(c, j.runID); err != nil {
				log.Warn("stopping a timed-out run", "err", err)
			}
			cancel()
			run, runErr = hermes.Run{ID: j.runID, Status: statusTimeout}, fmt.Errorf("%w: run took longer than %v; stopped", hermes.ErrNoReply, o.runTimeout)
			break loop
		case <-poll.C:
			r, err := bot.Hermes.Status(ctx, j.runID)
			if err != nil {
				if errors.Is(err, hermes.ErrRunNotFound) { // Hermes no longer knows the run
					run, runErr = hermes.Run{ID: j.runID, Status: hermes.StatusFailed}, err
					break loop
				}
				log.Debug("polling run status (will retry)", "err", err)
				continue
			}
			if r.Terminal() {
				run = r
				break loop
			}
		}
	}
	close(j.polling)
	c, cancel := bg()
	typing(c, false)
	if run.Status == hermes.StatusUnknown {
		// The run's owner restarted mid-turn. Acknowledge it, or Hermes holds
		// this session's later turns behind it.
		if err := bot.Hermes.ResolveUnknown(c, run); err != nil {
			log.Warn("resolving a run with an unknown outcome", "err", err)
		}
	}
	cancel()
	j.run, j.err = run, runErr

	w.mu.Lock()
	background := j.backgrounded
	if !background {
		close(j.done)
	}
	w.mu.Unlock()
	if background {
		o.finish(ctx, w, j, true)
	}
}

// statusTimeout marks a run roomd stopped at hermes_timeout.
const statusTimeout = "timeout"

// finish turns an ended run into the bot's turn: the cursor, memory, steer
// bookkeeping, cleanup and posting. A foreground result is posted as a
// plain message; a background one as a reply to the message that asked for
// it, after which the bot is free again.
func (o *Orchestrator) finish(ctx context.Context, w *worker, j *job, background bool) (string, outcome) {
	roomID, botKey := w.id, j.bot
	person, _ := w.roster.ByKey(botKey)
	log := o.log.With("room", roomID, "bot", botKey, "session", j.sid)
	if background {
		defer func() {
			w.mu.Lock()
			delete(w.busy, botKey)
			w.mu.Unlock()
		}()
		select { // the result never lands before the hand-off notice
		case <-j.announced:
		case <-ctx.Done():
		}
	}

	reply, err := j.run.Reply()
	if j.err != nil {
		err = j.err
	}
	if err != nil {
		// Cursor stays put: the same delta (and any steered message) is
		// offered next time.
		log.Error("hermes run produced no reply", "run", j.runID, "status", j.run.Status, "err", err)
		if background {
			o.notice(ctx, w, failureNotice(person.Display, j.run.Status))
		}
		return "", silent
	}
	// Hermes now holds the delta (and any memory block) as a user turn, so
	// never resend it. After a /reset the cursor already sits at the reset
	// point, and steers went to the old session, so neither is touched.
	if w.resets.Load() == j.resets {
		if err := o.store.SetCursor(ctx, roomID, botKey, j.upTo); err != nil {
			log.Error("advancing cursor", "err", err)
		}
		for _, s := range j.steered {
			if strings.Contains(j.run.PendingSteer, s.text) {
				log.Info("a steered message was never consumed; it stays in the next delta", "seq", s.seq)
				continue
			}
			if err := o.store.MarkSteered(ctx, roomID, botKey, s.seq); err != nil {
				log.Error("recording a consumed steer", "seq", s.seq, "err", err)
			}
		}
	}
	if w.memory != nil {
		w.memory.Delivered(j.sid, j.memIDs)
	}
	if reply.Silent {
		log.Info("bot stayed silent")
		return "", silent
	}
	log.Debug("raw reply", "text", reply.Text)
	reply.Text = CleanReply(reply.Text, person.Display, w.roster, j.delta)
	if reply.Text == "" {
		log.Warn("reply only echoed the context or spoke for others; posting nothing")
		return "", silent
	}
	if reply.Partial {
		log.Warn("posting a partial reply (the turn ended unfinished)", "reason", j.run.TurnExitReason)
	}
	replyTo := ""
	if background {
		// Real work the user asked for: posted even after a /reset.
		replyTo = j.replyTo
	} else if w.resets.Load() != j.resets {
		log.Info("discarding a reply from before /reset")
		return "", silent
	}
	posted, ok := o.post(ctx, w, botKey, reply.Text, replyTo, log)
	if !ok {
		return "", silent
	}
	reply.Text = posted
	log.Info("bot spoke", "took", o.now().Sub(j.start).Round(time.Millisecond), "chars", len(reply.Text), "background", background)
	return reply.Text, spoke
}

// post sends a cleaned reply as the bot: its text and images (inline data
// URLs, and files it names inside media_dirs) in their original order, each image as an m.image and text longer than one
// event allows split into several messages. Only the first event carries
// the reply relation. Each event is recorded in the transcript, images as
// "[image]", never their data. An image that fails to upload is replaced by
// a short note, so the reply is never lost. It returns the reply as the
// transcript recorded it, and whether anything was posted.
func (o *Orchestrator) post(ctx context.Context, w *worker, botKey, text, replyTo string, log *slog.Logger) (string, bool) {
	bot := w.bots[botKey]
	person, _ := w.roster.ByKey(botKey)
	parts, dropped := media.Split(text, o.files)
	for _, d := range dropped {
		log.Warn("left out of the posted reply", "what", d)
	}
	var recorded []string
	record := func(eventID, body string) {
		replyTo = "" // the relation goes on the first event only
		recorded = append(recorded, body)
		if _, err := o.store.Append(ctx, transcript.Entry{
			RoomID: w.id, EventID: eventID, Sender: person.MXID, Body: body, TS: o.now().UnixMilli(),
		}); err != nil {
			log.Error("recording own reply", "err", err)
		}
	}
	sendText := func(t string) {
		var id string
		var err error
		if replyTo != "" {
			id, err = bot.Poster.SendReply(ctx, w.id, t, replyTo)
		} else {
			id, err = bot.Poster.Send(ctx, w.id, t)
		}
		if err != nil {
			log.Error("posting reply", "err", err, "chars", len(t))
			return
		}
		record(id, t)
	}
	for _, p := range parts {
		if p.Image == nil {
			for _, chunk := range media.Chunks(p.Text, media.MaxText) {
				sendText(chunk)
			}
			continue
		}
		id, err := bot.Poster.SendImage(ctx, w.id, *p.Image, replyTo)
		if err != nil {
			log.Error("posting an image; sending a note instead", "err", err, "mime", p.Image.MIME, "bytes", len(p.Image.Data))
			sendText(media.Unsent)
			continue
		}
		record(id, media.Placeholder)
	}
	return strings.Join(recorded, "\n"), len(recorded) > 0
}

// failureNotice is the listener's short notice for a background run that
// ended without a reply.
func failureNotice(name, status string) string {
	switch status {
	case statusTimeout:
		return name + " ran out of time on that task and stopped."
	case hermes.StatusCancelled:
		return name + "'s task was cancelled."
	case hermes.StatusInterrupted, hermes.StatusUnknown:
		return name + "'s task was interrupted (Hermes restarted)."
	}
	return name + " couldn't finish that task."
}

// SessionID is the API-owned Hermes session id for one bot in one room:
// grp-<first 12 hex of sha256(room id)>-<bot>-g<generation>. It only uses
// path-safe characters; it never changes except when /reset bumps the
// generation, because Hermes retires deleted ids permanently.
func SessionID(roomID, botKey string, gen int) string {
	sum := sha256.Sum256([]byte(roomID))
	return fmt.Sprintf("grp-%s-%s-g%d", hex.EncodeToString(sum[:])[:12], botKey, gen)
}

// SystemPrompt renders the group rules for one bot. Placeholders: {{name}}
// (the bot's display name), {{user}} (the user's display name) and
// {{others}} (one "- Name: blurb" line per other bot). Built once at startup
// so it is byte-identical on every call.
func SystemPrompt(rules string, r *roster.Roster, botKey string, blurbs map[string]string) string {
	self, _ := r.ByKey(botKey)
	var others []string
	for _, p := range r.Bots() {
		if p.Key == botKey {
			continue
		}
		line := "- " + p.Display
		if b := strings.TrimSpace(blurbs[p.Key]); b != "" {
			line += ": " + b
		}
		others = append(others, line)
	}
	return strings.NewReplacer(
		"{{name}}", self.Display,
		"{{user}}", r.User().Display,
		"{{others}}", strings.Join(others, "\n"),
	).Replace(rules)
}

// Frame wraps the attributed delta in the user message sent to Hermes. The
// trailing instruction stops models from continuing the transcript (echoing
// it, or writing other characters' messages) instead of replying.
func Frame(delta, self string) string { return FrameWith(delta, self, "") }

// FrameWith is Frame plus an optional extra instruction (for /explain). An
// empty delta is stated as "no new messages".
func FrameWith(delta, self, instruction string) string {
	return FrameMemory(delta, self, instruction, "")
}

// FrameMemory is FrameWith plus an optional memory block (recollections of
// past group chats), placed before the new messages and clearly separated
// from them. The system message is never changed.
func FrameMemory(delta, self, instruction, memory string) string {
	var b strings.Builder
	if memory != "" {
		b.WriteString(memory + "\n\n")
	}
	if delta == "" {
		b.WriteString("There are no new messages in the group chat since your last reply.\n\n")
	} else {
		b.WriteString("New messages in the group chat since your last reply:\n\n<messages>\n" + delta + "\n</messages>\n\n")
	}
	if instruction != "" {
		b.WriteString(instruction + "\n\n")
	}
	b.WriteString("Reply as " + self + " with " + self + "'s next chat message only, as plain text: no <msg> tags and no name label. " +
		"Don't repeat or quote the messages above, and don't write messages for anyone else.")
	return b.String()
}

var (
	msgRe      = regexp.MustCompile(`(?s)<msg\s+from\s*=\s*"([^"]*)"[^>]*>(.*?)(?:</msg>|$)`)
	strayTagRe = regexp.MustCompile(`</?messages>|</?msg\b[^>]*>`)
	blankRunRe = regexp.MustCompile(`\n{3,}`)
)

// CleanReply turns a model reply into what the bot posts:
//   - message elements copied verbatim from the delta are dropped;
//   - <msg from="Self">…</msg> is unwrapped to its text;
//   - a message written for anyone else is dropped if it comes first, and
//     cuts the reply there if the bot had already said something;
//   - stray tags and the bot's own "[Name]"/"Name:" label are removed.
//
// An empty result means there is nothing to post.
func CleanReply(text, self string, r *roster.Roster, delta string) string {
	for _, line := range strings.Split(delta, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "<msg") {
			text = strings.ReplaceAll(text, line, "")
		}
	}
	var out strings.Builder
	pos := 0
	for _, m := range msgRe.FindAllStringSubmatchIndex(text, -1) {
		out.WriteString(text[pos:m[0]])
		pos = m[1]
		from := contextbuild.Unescape.Replace(text[m[2]:m[3]])
		if strings.EqualFold(strings.TrimSpace(from), self) {
			out.WriteString(contextbuild.Unescape.Replace(text[m[4]:m[5]]))
			continue
		}
		if strings.TrimSpace(strayTagRe.ReplaceAllString(out.String(), "")) != "" {
			pos = len(text) // the bot started writing for someone else: stop here
			break
		}
	}
	out.WriteString(text[pos:])
	cleaned := strayTagRe.ReplaceAllString(out.String(), "")
	cleaned = blankRunRe.ReplaceAllString(strings.TrimSpace(cleaned), "\n\n")
	return StripSelfLabel(cleaned, self)
}

// StripSelfLabel removes one leading "[Name]" or "Name:" that a model echoes
// from the attributed context, so it neither shows in the room nor stacks
// up as "[Ada] [Ada] …" in other bots' deltas.
func StripSelfLabel(text, display string) string {
	t := strings.TrimSpace(text)
	for _, prefix := range []string{"[" + display + "]", display + ":"} {
		if len(t) >= len(prefix) && strings.EqualFold(t[:len(prefix)], prefix) {
			return strings.TrimSpace(t[len(prefix):])
		}
	}
	return t
}

func entryOf(m matrixio.Message) transcript.Entry {
	e := transcript.Entry{
		RoomID: m.RoomID, EventID: m.EventID, Sender: m.Sender, Body: m.Body,
		FormattedBody: m.FormattedBody, Mentions: m.Mentions, TS: m.TS,
	}
	if m.Image != nil {
		e.Body = strings.TrimSpace(imageLine(m.Image.Name) + " " + m.Body)
		e.FormattedBody = ""
		e.MediaURL, e.MediaMIME = m.Image.URL, m.Image.MIME
	}
	return e
}

// imageLine is how an image someone posted reads in the transcript.
func imageLine(name string) string {
	if name == "" {
		return media.Placeholder
	}
	return "[image: " + name + "]"
}

// MaxImages is the most images attached to one run; older ones in the same
// delta are mentioned but not sent.
const MaxImages = 4

// images downloads (as the bot) the images posted in entries by anyone else
// and returns them labelled for the run input, newest MaxImages at most. An
// image that can't be used becomes a short note instead.
func (o *Orchestrator) images(ctx context.Context, w *worker, p Poster, self string, entries []transcript.Entry, log *slog.Logger) ([]hermes.Image, []string) {
	var posted []transcript.Entry
	for _, e := range entries {
		if e.MediaURL != "" && e.Sender != self {
			posted = append(posted, e)
		}
	}
	var notes []string
	if extra := len(posted) - MaxImages; extra > 0 {
		for _, e := range posted[:extra] {
			notes = append(notes, describeImage(w, e)+" is not attached (at most "+strconv.Itoa(MaxImages)+" images per turn).")
		}
		posted = posted[extra:]
	}
	var out []hermes.Image
	for _, e := range posted {
		data, err := p.Download(ctx, e.MediaURL, media.MaxFileBytes)
		mime := ""
		if err == nil {
			if mime = http.DetectContentType(data); !strings.HasPrefix(mime, "image/") {
				err = fmt.Errorf("not an image (%s)", mime)
			}
		}
		if err != nil {
			log.Warn("image not attached", "event", e.EventID, "url", e.MediaURL, "err", err)
			notes = append(notes, describeImage(w, e)+" could not be loaded.")
			continue
		}
		out = append(out, hermes.Image{Label: describeImage(w, e) + ":", MIME: mime, Data: data})
	}
	return out, notes
}

// describeImage names an image by who posted it and its file name.
func describeImage(w *worker, e transcript.Entry) string {
	s := "Image from " + w.roster.DisplayFor(e.Sender)
	if name := imageName(e.Body); name != "" {
		s += " (" + name + ")"
	}
	return s
}

func imageName(body string) string {
	if rest, ok := strings.CutPrefix(body, "[image: "); ok {
		if name, _, ok := strings.Cut(rest, "]"); ok {
			return name
		}
	}
	return ""
}

// imagesNote tells the bot about the attached images (and any left out).
func imagesNote(n int, notes []string) string {
	var b strings.Builder
	switch n {
	case 0:
	case 1:
		b.WriteString("The image posted above is attached after this text; look at it.")
	default:
		b.WriteString("The " + strconv.Itoa(n) + " images posted above are attached after this text, in order; look at them.")
	}
	for _, note := range notes {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(note)
	}
	return b.String()
}
