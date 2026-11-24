package room

import (
	"context"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/ziliding/hermes-roomd/internal/checkin"
	"github.com/ziliding/hermes-roomd/internal/matrixio"
	"github.com/ziliding/hermes-roomd/internal/turns"
)

// CheckIn turns on check-ins in a room: once nobody has written for the
// schedule's After (moved by its jitter, and never inside its quiet hours),
// one bot is asked to start something. The others may react to it, as
// bot-to-bot continuation under max_bot_turns.
type CheckIn struct {
	Schedule checkin.Schedule
	// MaxUnanswered stops check-ins after this many in a row with no user
	// message or command in between (0: no limit).
	MaxUnanswered int
	// Every is how often the watcher looks at the room (default a minute).
	Every time.Duration
	Rand  *rand.Rand // jitter and the opening bot; used by the watcher only
}

// DefaultCheckInEvery is how often a watcher looks at its room.
const DefaultCheckInEvery = time.Minute

// checkInReq is a check-in the watcher queued for the worker.
type checkInReq struct {
	quiet time.Duration // how long the room had been quiet
	pick  uint64        // chooses the opening bot among the free ones
}

// watchQuiet queues a check-in each time the room has been quiet long
// enough. Quiet is measured from the latest of: the newest message in the
// transcript (anyone's), the last user message or command, the last
// check-in (even one the bot answered with silence), and roomd's start, so
// a restart never sets the bots off at once. The due time is drawn once per
// quiet spell, so the jitter is not re-rolled every minute.
func (o *Orchestrator) watchQuiet(ctx context.Context, w *worker, started time.Time) {
	ci := w.checkIn
	log := o.log.With("room", w.id)
	tick := time.NewTicker(or0(ci.Every, DefaultCheckInEvery))
	defer tick.Stop()

	floor := started // then the last user activity or check-in
	seenEpoch := w.userEpoch.Load()
	unanswered := 0
	var anchor, due time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now := o.now()
		if e := w.userEpoch.Load(); e != seenEpoch {
			seenEpoch, unanswered, floor = e, 0, now
		}
		ms, err := o.store.LatestTS(ctx, w.id)
		if err != nil {
			log.Error("check-in: reading the latest message", "err", err)
			continue
		}
		a := floor
		if last := time.UnixMilli(ms); ms > 0 && last.After(a) {
			a = last
		}
		if !a.Equal(anchor) {
			anchor, due = a, ci.Schedule.Due(a, ci.Rand)
			log.Debug("check-in: next one due", "at", due, "quiet_since", anchor)
		}
		switch {
		case now.Before(due), ci.Schedule.QuietAt(now):
			continue
		case ci.MaxUnanswered > 0 && unanswered >= ci.MaxUnanswered:
			if unanswered == ci.MaxUnanswered {
				log.Info("check-in: the user hasn't answered the last ones; waiting for them", "unanswered", unanswered)
				unanswered++ // log once
			}
			continue
		}
		req := &checkInReq{quiet: now.Sub(anchor), pick: ci.Rand.Uint64()}
		select {
		case w.inbox <- queued{checkIn: req, epoch: seenEpoch}:
			unanswered++
		default:
			log.Warn("check-in: room inbox full; skipped")
		}
		floor = now // the next one is a full spell away, whatever the bot does
	}
}

// runCheckIn asks one free bot to start something, then lets the others
// react as bot-to-bot continuation. A user message that arrived since the
// check-in was queued cancels it: the user has the floor.
func (o *Orchestrator) runCheckIn(ctx context.Context, w *worker, q queued) {
	log := o.log.With("room", w.id)
	if q.epoch != w.userEpoch.Load() {
		log.Info("check-in: the user spoke meanwhile; skipped")
		return
	}
	busy := w.busyBots()
	free := slices.DeleteFunc(slices.Clone(w.botKeys), func(b string) bool { return slices.Contains(busy, b) })
	if len(free) > 1 {
		// Not the bot that spoke last (often the one that opened last time).
		var mxids []string
		for _, p := range w.roster.Bots() {
			mxids = append(mxids, p.MXID)
		}
		if last, err := o.store.LastFrom(ctx, w.id, mxids); err == nil && last != "" {
			if p, ok := w.roster.ByMXID(last); ok && p != nil {
				free = slices.DeleteFunc(free, func(b string) bool { return b == p.Key })
			}
		}
	}
	if len(free) == 0 {
		log.Info("check-in: every bot is busy; skipped")
		return
	}
	bot := free[q.checkIn.pick%uint64(len(free))]
	log.Info("check-in: the room has been quiet; asking a bot to start something", "bot", bot, "quiet", q.checkIn.quiet.Round(time.Minute))

	w.state = turns.State{}  // a fresh exchange: the cap counts from here
	var req matrixio.Message // nothing asked for it: a background result is a plain message
	loc := time.Local
	if w.checkIn != nil && w.checkIn.Schedule.Loc != nil {
		loc = w.checkIn.Schedule.Loc
	}
	text, out := o.speak(ctx, w, bot, checkin.Instruction(w.roster.User().Display, q.checkIn.quiet, o.now().In(loc)), req)
	switch out {
	case silent:
		log.Info("check-in: the bot had nothing to say", "bot", bot)
		return
	case backgrounded:
		return // the others may react to its result once it is posted
	}
	// The opener is not counted against max_bot_turns; the replies to it are.
	w.state.BotSpoke(bot, false)
	trig := o.botTrigger(w, bot, text, false)
	if t, ok := o.results(ctx, w); ok {
		trig = t
	}
	st := w.state
	st.Busy = w.busyBots()
	plan, ok := o.decide(ctx, w, q.epoch, st, trig)
	if !ok {
		return
	}
	o.turns(ctx, w, q.epoch, req, plan, nil)
}
