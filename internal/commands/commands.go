// Package commands parses the user's in-room commands and holds the parts of
// running them that don't need Matrix or Hermes:
//
//   - /reset: every bot starts a fresh Hermes session for the room. Each
//     bot's session generation goes up by one, so the next call opens
//     grp-<room>-<bot>-g<N+1>. Ids are never reused or deleted: Hermes
//     retires deleted session ids permanently. Every cursor moves to the
//     newest message, so the new sessions start clean.
//   - /explain [bot]: one bot (the one named, else the last bot that spoke)
//     explains plainly what is going on and what, concretely, it wants from
//     the user. This is one turn that bypasses the router.
//
// Only the configured user can run commands. The orchestrator handles them
// before the turn engine and never records them as conversation. The
// documented form is "!reset" / "!explain", because Element intercepts unknown
// slash commands with an "Unrecognised command" dialog; "/reset" and
// "/explain" are accepted too.
package commands

import (
	"context"
	"errors"
	"strings"

	"github.com/ziliding/hermes-roomd/internal/roster"
)

// Kind is a command name.
type Kind string

const (
	Reset   Kind = "reset"
	Explain Kind = "explain"
)

// Command is one parsed command.
type Command struct {
	Kind Kind
	Arg  string // /explain's optional bot name, trimmed; "" otherwise
	// FormattedBody is the event's HTML body, for resolving an Element pill
	// in /explain's argument.
	FormattedBody string
}

// Parse reads a message body as a command. Only an exact, known command word
// at the start of the message counts ("/reset", "/Explain cleo", "!reset");
// anything else (including "/resetting" or "/shrug") is ordinary text.
func Parse(body string) (Command, bool) {
	s := strings.TrimSpace(body)
	if len(s) < 2 || (s[0] != '/' && s[0] != '!') {
		return Command{}, false
	}
	word, rest, _ := strings.Cut(s[1:], " ")
	if i := strings.IndexAny(word, "\t\n"); i >= 0 {
		word, rest = word[:i], word[i:]+" "+rest
	}
	rest = strings.TrimSpace(rest)
	switch Kind(strings.ToLower(word)) {
	case Reset:
		return Command{Kind: Reset}, true
	case Explain:
		return Command{Kind: Explain, Arg: rest}, true
	}
	return Command{}, false
}

// ResetStore is the part of transcript.Store that /reset needs.
type ResetStore interface {
	Reset(ctx context.Context, roomID string, bots []string, upTo int64) error
}

// RunReset bumps every bot's session generation in roomID and moves every
// cursor to upTo, the newest seq recorded when the command arrived.
func RunReset(ctx context.Context, s ResetStore, roomID string, bots []string, upTo int64) error {
	return s.Reset(ctx, roomID, bots, upTo)
}

// ResetNotice is the acknowledgement posted in the room after /reset.
const ResetNotice = "Reset: every character starts a fresh group conversation from here and won't see the messages above."

// ErrNoTarget means /explain had no bot argument and no bot has spoken yet.
var ErrNoTarget = errors.New("no bot has spoken in this room yet")

// ErrUnknownBot means /explain's argument names no bot.
var ErrUnknownBot = errors.New("not a character in this room")

// ExplainTarget picks the bot for /explain. A non-empty arg must name a bot
// (key, display name, localpart or Matrix id, with or without "@", or an
// Element pill); otherwise the last bot that spoke (lastSpeakerMXID, "" if
// none) is used.
func ExplainTarget(r *roster.Roster, c Command, lastSpeakerMXID string) (string, error) {
	if c.Arg != "" || roster.PillTargets(c.FormattedBody) != nil {
		arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(c.Arg), "@"))
		for _, p := range r.Bots() {
			if strings.EqualFold(arg, p.Key) || strings.EqualFold(arg, p.Display) ||
				strings.EqualFold("@"+arg, p.MXID) || strings.EqualFold(arg, roster.Localpart(p.MXID)) {
				return p.Key, nil
			}
		}
		// A pill or a longer argument ("/explain Cleo please").
		if key := r.Addressee(c.Arg, c.FormattedBody, nil, ""); key != "" {
			return key, nil
		}
		return "", ErrUnknownBot
	}
	if p, ok := r.ByMXID(lastSpeakerMXID); ok && p.IsBot {
		return p.Key, nil
	}
	return "", ErrNoTarget
}

// ExplainInstruction is appended to the bot's framed delta for /explain. It
// goes in the user message, never the system prompt, so the system prompt
// stays byte-identical and Hermes' prompt cache stays warm.
func ExplainInstruction(user string) string {
	return user + " asked you to explain: they want you to step back and say plainly what is going on. " +
		"In a few short sentences of everyday language, with no metaphors and no invented terms: " +
		"what the conversation is about right now, what you think, and what, concretely, you want from " + user + ". " +
		"Put that last part in one plain sentence " + user + " can act on, or say you need nothing from " + user + "."
}
