// Package turns decides which bot speaks next in a room, deterministically.
//
// The Decider interface is the seam for the phase-2 LLM router: the
// orchestrator owns State (counters) and asks a Decider for the next speaker
// after every message; Rules is the MVP implementation.
package turns

import (
	"context"
	"math/rand/v2"
)

// State is a room's turn-taking counters. Any user message resets it.
type State struct {
	BotTurns int    // consecutive bot replies since the user last spoke
	LastBot  string // bot key that spoke last ("" after a user message)
}

// UserSpoke resets the counters: the user always regains the floor.
func (s *State) UserSpoke() { *s = State{} }

// BotSpoke records a (non-silent) bot reply.
func (s *State) BotSpoke(bot string) {
	s.BotTurns++
	s.LastBot = bot
}

// Trigger describes the message that just landed.
type Trigger struct {
	RoomID    string // the room the message landed in (routers read its transcript)
	FromUser  bool
	Sender    string // bot key when !FromUser
	Addressee string // the single bot it names, "" if none (see roster.Addressee)
	Body      string // for routers that read text; Rules ignores it
}

// Decider picks the next speaker. ok=false means nobody speaks: wait for the user.
type Decider interface {
	Next(ctx context.Context, st State, t Trigger) (bot string, ok bool, err error)
}

// Rules is the MVP decider:
//   - after MaxBotTurns consecutive bot replies, nobody speaks until the user does;
//   - a named bot answers (at most one addressee per message; a bot naming
//     itself names nobody);
//   - otherwise a random bot, never the one that just spoke.
type Rules struct {
	Bots        []string // candidate bot keys, stable order
	MaxBotTurns int
	Rand        *rand.Rand // injectable for tests
}

// NewRules builds Rules with a seeded RNG.
func NewRules(bots []string, maxBotTurns int, rng *rand.Rand) *Rules {
	return &Rules{Bots: bots, MaxBotTurns: maxBotTurns, Rand: rng}
}

// Next implements Decider.
func (r *Rules) Next(_ context.Context, st State, t Trigger) (string, bool, error) {
	if st.BotTurns >= r.MaxBotTurns {
		return "", false, nil
	}
	if t.Addressee != "" && t.Addressee != st.LastBot && r.known(t.Addressee) {
		return t.Addressee, true, nil
	}
	var pool []string
	for _, b := range r.Bots {
		if b != st.LastBot {
			pool = append(pool, b)
		}
	}
	if len(pool) == 0 {
		return "", false, nil
	}
	return pool[r.Rand.IntN(len(pool))], true, nil
}

func (r *Rules) known(bot string) bool {
	for _, b := range r.Bots {
		if b == bot {
			return true
		}
	}
	return false
}
