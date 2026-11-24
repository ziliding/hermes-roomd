// Package turns decides which bots speak, deterministically.
//
// The orchestrator owns State and asks a Decider for a plan after every
// message. Rules is the plain implementation; the LLM router (package
// router) implements the same interface and falls back to Rules.
//
// Two kinds of bot turns exist:
//   - Obligations: after a user message, every bot in the plan owes the user
//     a reply. They are an unordered set (State.Owed); whenever the floor is
//     free the orchestrator asks for a plan again and the first owed bot in
//     it speaks next. Obligations are not capped.
//   - Continuation: once nobody owes the user anything, bots may react to
//     each other, at most MaxBotTurns times (State.BotTurns) until the user
//     speaks again. The user's !continue [n] grants another round: it
//     resets BotTurns and sets State.Cap to n for that round only.
package turns

import (
	"context"
	"math/rand/v2"
	"slices"
)

// State is a room's turn-taking state. Any user message resets it.
type State struct {
	BotTurns int    // continuation replies since the user last spoke (obligations not counted)
	LastBot  string // bot key that spoke last ("" after a user message)
	// Busy lists bots working on a background task in this room. They are
	// never picked. The orchestrator fills it before every decision.
	Busy []string
	// Owed lists the bots that still owe the user a reply to their latest
	// message (busy ones left out). The orchestrator fills it before every
	// decision after a bot message.
	Owed []string
	// Cap, when > 0, replaces the decider's MaxBotTurns until the user next
	// speaks. !continue n sets it; 0 keeps the configured cap.
	Cap int
}

// Limit is the bot-to-bot cap in force: Cap when set, else maxBotTurns.
func (s State) Limit(maxBotTurns int) int {
	if s.Cap > 0 {
		return s.Cap
	}
	return maxBotTurns
}

// IsBusy reports whether bot is in Busy.
func (s State) IsBusy(bot string) bool { return slices.Contains(s.Busy, bot) }

// UserSpoke resets the counters: the user always regains the floor.
func (s *State) UserSpoke() { *s = State{} }

// BotSpoke records a (non-silent) bot reply. continuation is false when the
// reply fulfilled an obligation to the user, which the cap does not count.
func (s *State) BotSpoke(bot string, continuation bool) {
	if continuation {
		s.BotTurns++
	}
	s.LastBot = bot
}

// Trigger describes the message that just landed.
type Trigger struct {
	RoomID     string // the room the message landed in (routers read its transcript)
	FromUser   bool
	Sender     string   // bot key when !FromUser
	Addressees []string // bots it names, in order (see roster.Addressees)
	Group      bool     // addressed to everyone ("你们", "everyone"; see roster.GroupMatcher)
	// Handoff: no new message; Sender's run went on in the background (its
	// result is posted later). Sender is busy and cannot be picked.
	Handoff bool
	Body    string // for routers that read text
}

// Decider plans who speaks.
//
// For a user message, Plan returns every bot that owes the user a reply, in
// the preferred order; empty means nobody answers. For a bot message, it
// returns the preferred order of State.Owed when that is not empty (the
// orchestrator takes the first owed bot in it, or the first owed bot if it
// names none); otherwise at most one bot to continue, or nobody.
type Decider interface {
	Plan(ctx context.Context, st State, t Trigger) ([]string, error)
}

// Rules is the plain decider:
//   - a user message that names bots is owed by those bots, in the order
//     named; one addressed to the group is owed by every bot; otherwise by
//     one random bot;
//   - while bots still owe the user, they speak in random order;
//   - after MaxBotTurns continuation replies (State.Cap when set), nobody
//     speaks until the user does;
//   - otherwise a bot named in the last message continues, or a random bot,
//     never the one that just spoke.
type Rules struct {
	Bots        []string // candidate bot keys, stable order
	MaxBotTurns int
	Rand        *rand.Rand // injectable for tests
}

// NewRules builds Rules with a seeded RNG.
func NewRules(bots []string, maxBotTurns int, rng *rand.Rand) *Rules {
	return &Rules{Bots: bots, MaxBotTurns: maxBotTurns, Rand: rng}
}

// Plan implements Decider.
func (r *Rules) Plan(_ context.Context, st State, t Trigger) ([]string, error) {
	if t.FromUser {
		if owed := r.UserOwed(st, t); len(owed) > 0 {
			return owed, nil
		}
		return r.pickOne(st, ""), nil
	}
	if len(st.Owed) > 0 {
		owed := slices.Clone(st.Owed)
		r.Rand.Shuffle(len(owed), func(i, j int) { owed[i], owed[j] = owed[j], owed[i] })
		return owed, nil
	}
	if st.BotTurns >= st.Limit(r.MaxBotTurns) {
		return nil, nil
	}
	for _, a := range t.Addressees {
		if a != st.LastBot && a != t.Sender && r.known(a) && !st.IsBusy(a) {
			return []string{a}, nil
		}
	}
	return r.pickOne(st, t.Sender), nil
}

// UserOwed is the part of a user message's plan that needs no model: the
// bots it names, in order, then (if it addresses the group) every other
// free bot in random order. Busy and unknown bots are left out. Empty means
// the message names nobody and is not addressed to the group.
func (r *Rules) UserOwed(st State, t Trigger) []string {
	out := Named(r.Bots, st, t)
	if t.Group {
		rest := r.free(st, "")
		r.Rand.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
		for _, b := range rest {
			if !slices.Contains(out, b) {
				out = append(out, b)
			}
		}
	}
	return out
}

// Named is the trigger's addressees that are in cast and not busy, in
// order, without duplicates.
func Named(cast []string, st State, t Trigger) []string {
	var out []string
	for _, a := range t.Addressees {
		if slices.Contains(cast, a) && !st.IsBusy(a) && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// pickOne is one random free bot other than the last speaker and sender.
func (r *Rules) pickOne(st State, sender string) []string {
	var pool []string
	for _, b := range r.free(st, sender) {
		if b != st.LastBot {
			pool = append(pool, b)
		}
	}
	if len(pool) == 0 {
		return nil
	}
	return []string{pool[r.Rand.IntN(len(pool))]}
}

// free lists the bots that are not busy (and not sender), in cast order.
func (r *Rules) free(st State, sender string) []string {
	var out []string
	for _, b := range r.Bots {
		if b != sender && !st.IsBusy(b) {
			out = append(out, b)
		}
	}
	return out
}

func (r *Rules) known(bot string) bool { return slices.Contains(r.Bots, bot) }
