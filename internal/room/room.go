// Package room is the orchestrator: it records every room message, and for
// each message from the user runs one serialized chain of bot turns
// (decide → build delta → call Hermes → post as the bot → decide again).
//
// One worker goroutine per room means one turn in flight per room. A user
// message that arrives mid-turn lets the in-flight reply finish and post,
// then stops the chain; the worker then handles that newest user message.
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
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ziliding/hermes-roomd/internal/commands"
	"github.com/ziliding/hermes-roomd/internal/contextbuild"
	"github.com/ziliding/hermes-roomd/internal/hermes"
	"github.com/ziliding/hermes-roomd/internal/matrixio"
	"github.com/ziliding/hermes-roomd/internal/roster"
	"github.com/ziliding/hermes-roomd/internal/transcript"
	"github.com/ziliding/hermes-roomd/internal/turns"
)

// Poster posts text into a room as one bot and returns the event id.
// SendNotice posts roomd's own status text, tagged so the listener never
// records its echo as conversation (see matrixio.SendNotice).
type Poster interface {
	Send(ctx context.Context, roomID, text string) (string, error)
	SendNotice(ctx context.Context, roomID, text string) (string, error)
}

// Completer calls one bot's Hermes profile.
type Completer interface {
	Complete(ctx context.Context, sessionID, system, delta string) (hermes.Reply, error)
}

// Memory supplies the recollections block for one bot turn ("" for none)
// and the ids of the memories in it. It must never fail a turn: errors are
// its own to log. Delivered is called only after the Hermes call succeeded
// (Hermes then holds the block), so a failed call offers them again.
type Memory interface {
	Block(ctx context.Context, roomID, sessionID string) (string, []string)
	Delivered(sessionID string, ids []string)
}

// Bot is everything the orchestrator needs to make one character speak.
type Bot struct {
	Key    string
	Poster Poster
	Hermes Completer
	System string // byte-identical on every call (keeps Hermes' prompt cache warm)
}

// Orchestrator owns all configured rooms.
type Orchestrator struct {
	store    *transcript.Store
	maxDelta int
	log      *slog.Logger
	now      func() time.Time

	rooms map[string]*worker
	wg    sync.WaitGroup
}

// worker is one room: its cast and settings, plus its serialized turn queue.
type worker struct {
	id       string
	roster   *roster.Roster // the user plus this room's cast only
	builder  *contextbuild.Builder
	decider  turns.Decider
	memory   Memory // nil: no group memory in this room
	bots     map[string]*Bot
	botKeys  []string // this room's cast, stable order
	notifier string   // bot key that posts roomd's notices in this room

	inbox     chan queued
	userEpoch atomic.Int64 // bumped by every user message and command as it arrives
	resets    atomic.Int64 // bumped by every /reset as it arrives
	state     turns.State  // touched only by the worker goroutine
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
}

type queued struct {
	msg   matrixio.Message
	epoch int64
	cmd   *commands.Command // non-nil for a command
	upTo  int64             // newest seq when the command arrived (for /reset)
}

// Options configures New. Each room comes from RoomSpecs; the rooms listed
// in Rooms (if any) all share Roster, Decider, Bots, Notifier and Memory.
type Options struct {
	Store         *transcript.Store
	RoomSpecs     []RoomSpec
	MaxDeltaLines int
	Log           *slog.Logger

	Rooms    []string
	Roster   *roster.Roster
	Decider  turns.Decider
	Bots     []*Bot
	Notifier string
	Memory   Memory
}

// New builds an orchestrator; call Run to start the room workers.
func New(o Options) *Orchestrator {
	or := &Orchestrator{
		store: o.Store, maxDelta: o.MaxDeltaLines,
		log: o.Log, now: time.Now, rooms: map[string]*worker{},
	}
	specs := append([]RoomSpec{}, o.RoomSpecs...)
	for _, r := range o.Rooms {
		specs = append(specs, RoomSpec{ID: r, Roster: o.Roster, Decider: o.Decider, Bots: o.Bots, Notifier: o.Notifier, Memory: o.Memory})
	}
	for _, sp := range specs {
		w := &worker{
			id: sp.ID, roster: sp.Roster, builder: contextbuild.New(sp.Roster), decider: sp.Decider,
			memory: sp.Memory, bots: map[string]*Bot{}, notifier: sp.Notifier, inbox: make(chan queued, 64),
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

// Run starts one worker per room and blocks until ctx is done and they exit.
func (o *Orchestrator) Run(ctx context.Context) {
	for _, w := range o.rooms {
		o.wg.Add(1)
		go func() {
			defer o.wg.Done()
			o.work(ctx, w)
		}()
	}
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
	if m.Sender == w.roster.User().MXID {
		if cmd, ok := commands.Parse(m.Body); ok {
			o.enqueueCommand(ctx, w, m, cmd)
			return
		}
	}
	if _, err := o.store.Append(ctx, entryOf(m)); err != nil {
		o.log.Error("recording message", "room", m.RoomID, "event", m.EventID, "err", err)
		return
	}
	if m.Sender != w.roster.User().MXID {
		o.log.Debug("recorded; not a trigger (sender is not the configured user)", "room", m.RoomID, "sender", m.Sender)
		return
	}
	epoch := w.userEpoch.Add(1)
	select {
	case w.inbox <- queued{msg: m, epoch: epoch}:
	default:
		// Inbox full: the message is recorded and will reach bots as context;
		// the newest queued user message still triggers.
		o.log.Warn("room inbox full; message recorded without its own trigger", "room", m.RoomID)
	}
}

// enqueueCommand queues a user command without recording it. Like any user
// message it stops a running chain; a /reset also discards a reply that is
// still being generated, since it belongs to the old sessions.
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
		log.Info("explain", "bot", bot)
		// One called turn: no router, no chain after it.
		if _, spoke := o.speak(ctx, w, bot, commands.ExplainInstruction(w.roster.User().Display)); spoke {
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
		next, ok, err := w.decider.Next(ctx, w.state, trig)
		if err != nil {
			o.log.Error("turn decision failed", "room", w.id, "err", err)
			return
		}
		if !ok {
			o.log.Debug("turn decision: nobody speaks", "room", w.id, "bot_turns", w.state.BotTurns)
			return
		}
		if w.userEpoch.Load() != q.epoch {
			// The user (or a command) arrived while the decider was thinking
			// (the LLM router can take seconds): that decision is stale.
			o.log.Info("user spoke while deciding; stopping", "room", w.id)
			return
		}
		o.log.Debug("turn decision", "room", w.id, "next", next, "addressee", trig.Addressee, "bot_turns", w.state.BotTurns)
		text, spoke := o.speak(ctx, w, next, "")
		if !spoke {
			return // silence or failure ends the chain; it is not a turn
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

// speak makes one bot take one turn. It reports the posted text and whether
// anything was posted. A non-empty instruction (used by /explain) is added
// to the framed delta, and the bot is called even when it has nothing new.
func (o *Orchestrator) speak(ctx context.Context, w *worker, botKey, instruction string) (string, bool) {
	roomID := w.id
	resets := w.resets.Load()
	bot, ok := w.bots[botKey]
	person, _ := w.roster.ByKey(botKey)
	if !ok || person == nil {
		o.log.Error("decider chose unknown bot", "bot", botKey)
		return "", false
	}
	log := o.log.With("room", roomID, "bot", botKey)

	cursor, err := o.store.Cursor(ctx, roomID, botKey)
	if err != nil {
		log.Error("reading cursor", "err", err)
		return "", false
	}
	entries, err := o.store.Since(ctx, roomID, cursor, o.maxDelta)
	if err != nil {
		log.Error("reading transcript", "err", err)
		return "", false
	}
	if len(entries) == 0 && instruction == "" {
		log.Debug("nothing new for this bot", "cursor", cursor)
		return "", false
	}
	upTo := cursor
	if len(entries) > 0 {
		upTo = entries[len(entries)-1].Seq
	}
	delta := w.builder.Build(botKey, entries)
	if delta == "" && instruction == "" {
		log.Debug("delta empty after filtering (own lines / silence)")
		_ = o.store.SetCursor(ctx, roomID, botKey, upTo)
		return "", false
	}
	gen, err := o.store.Generation(ctx, roomID, botKey)
	if err != nil {
		log.Error("reading session generation", "err", err)
		return "", false
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
	reply, err := bot.Hermes.Complete(ctx, sid, bot.System, FrameMemory(delta, person.Display, instruction, memory))
	if err != nil {
		// Cursor stays put: the same delta is offered next time. (Whether
		// Hermes stored the user turn of a failed run is unverified.)
		lvl := slog.LevelWarn
		if !errors.Is(err, hermes.ErrNoReply) {
			lvl = slog.LevelError
		}
		log.Log(ctx, lvl, "hermes call produced no reply", "session", sid, "err", err)
		return "", false
	}
	// Hermes now holds the delta (and any memory block) as a user turn, so
	// never resend it.
	if err := o.store.SetCursor(ctx, roomID, botKey, upTo); err != nil {
		log.Error("advancing cursor", "err", err)
	}
	if w.memory != nil {
		w.memory.Delivered(sid, memIDs)
	}
	if reply.Silent {
		log.Info("bot stayed silent", "session", sid)
		return "", false
	}
	log.Debug("raw reply", "text", reply.Text)
	reply.Text = CleanReply(reply.Text, person.Display, w.roster, delta)
	if reply.Text == "" {
		log.Warn("reply only echoed the context or spoke for others; posting nothing", "session", sid)
		return "", false
	}
	if w.resets.Load() != resets {
		log.Info("discarding a reply from before /reset", "session", sid)
		return "", false
	}
	if reply.FinishReason == "length" {
		log.Warn("posting partial reply (finish_reason=length)", "session", sid)
	}
	eventID, err := bot.Poster.Send(ctx, roomID, reply.Text)
	if err != nil {
		log.Error("posting reply", "err", err)
		return "", false
	}
	if _, err := o.store.Append(ctx, transcript.Entry{
		RoomID: roomID, EventID: eventID, Sender: person.MXID, Body: reply.Text, TS: o.now().UnixMilli(),
	}); err != nil {
		log.Error("recording own reply", "err", err)
	}
	log.Info("bot spoke", "session", sid, "took", o.now().Sub(start).Round(time.Millisecond), "chars", len(reply.Text))
	return reply.Text, true
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
	return transcript.Entry{
		RoomID: m.RoomID, EventID: m.EventID, Sender: m.Sender, Body: m.Body,
		FormattedBody: m.FormattedBody, Mentions: m.Mentions, TS: m.TS,
	}
}
