package room

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ziliding/hermes-roomd/internal/turns"
)

func postedBots(posts []string) []string {
	var out []string
	for _, p := range posts {
		out = append(out, strings.SplitN(p, ":", 2)[0])
	}
	return out
}

// "你们觉得呢?" is owed by every bot, even with no bot-to-bot turns allowed:
// the cap only limits bots reacting to each other.
func TestGroupMessageEveryBotAnswers(t *testing.T) {
	for _, body := range []string{"你们觉得呢?", "what does everyone think?"} {
		h := newHarness(t, 0, func(bot string, n int) string { return "reply " + bot })
		h.runOne(body)
		got := postedBots(h.posts)
		slices.Sort(got)
		if !slices.Equal(got, []string{"ada", "cleo"}) {
			t.Fatalf("%q: posts %v, want one reply from each bot", body, h.posts)
		}
	}
}

// A message naming several bots is owed by all of them, in the order named.
func TestSeveralNamedBotsAllAnswer(t *testing.T) {
	h := newHarness(t, 0, func(bot string, n int) string { return "reply " + bot })
	h.runOne("Cleo and Ada, thoughts?")
	if got := postedBots(h.posts); !slices.Equal(got, []string{"cleo", "ada"}) {
		t.Fatalf("posts %v, want cleo then ada", h.posts)
	}
}

// A bot that stays silent (or fails) has met its obligation; the others
// still answer.
func TestSilentOrFailedOwedBotDoesNotEndTheChain(t *testing.T) {
	for _, first := range []string{"[SILENT]", "ERR"} {
		h := newHarness(t, 0, func(bot string, n int) string {
			if n == 1 {
				return first
			}
			return "reply " + bot
		})
		h.runOne("你们觉得呢?")
		if len(h.posts) != 1 || len(h.calls) != 2 || h.calls[0].bot == h.calls[1].bot {
			t.Fatalf("first reply %s: posts %v calls %d, want the other bot to answer", first, h.posts, len(h.calls))
		}
	}
}

// questionDecider plans both bots for the user, then answers every bot
// message like a router that thinks the conversation waits for the user
// (a bot asked the user something in passing).
type questionDecider struct{ seen [][]string }

func (d *questionDecider) Plan(_ context.Context, st turns.State, t turns.Trigger) ([]string, error) {
	if t.FromUser {
		return []string{"ada", "cleo"}, nil
	}
	d.seen = append(d.seen, slices.Clone(st.Owed))
	return nil, nil
}

// A bot's question to the user doesn't cancel the replies other bots still
// owe: the decider only orders obligations, it cannot drop them.
func TestQuestionInPassingKeepsObligations(t *testing.T) {
	d := &questionDecider{}
	h := newHarnessWith(t, harnessOpts{maxTurns: 2, decider: func([]string) turns.Decider { return d }},
		func(bot string, n int) string {
			if bot == "ada" {
				return "Blue, I think. Alex, did you already buy paint?"
			}
			return "reply " + bot
		})
	h.runOne("which colour for the kitchen?")
	if got := postedBots(h.posts); !slices.Equal(got, []string{"ada", "cleo"}) {
		t.Fatalf("posts %v, want ada then cleo", h.posts)
	}
	if want := [][]string{{"cleo"}, nil}; len(d.seen) != 2 || !slices.Equal(d.seen[0], want[0]) || len(d.seen[1]) != 0 {
		t.Fatalf("owed seen by the decider %v, want %v", d.seen, want)
	}
}

// A message naming a busy bot and a free one is steered into the busy
// bot's run, and the free bot still answers.
func TestBusyAndFreeBotNamedTogether(t *testing.T) {
	release := make(chan struct{})
	h := newHarnessWith(t, harnessOpts{maxTurns: 0, backgroundAfter: 20 * time.Millisecond}, slowAda(release))
	startBusyAda(t, h)
	runID := h.waitCalls(1)[0].run

	m := h.user("Ada and Cleo, how is it going?")
	h.o.Handle(h.ctx, m)
	if posts := h.waitPosts(1); posts[0] != "cleo: reply cleo" {
		t.Fatalf("posts %v", posts)
	}
	waitFor(t, func() bool { _, _, r, _, _ := h.snap(); return len(r) == 1 })
	if s := h.srv.Get(runID).Steers; len(s) != 1 || !strings.Contains(s[0], "Ada and Cleo") {
		t.Fatalf("steers %q", s)
	}
	close(release)
	h.waitPosts(2)
	h.waitIdle()
}
