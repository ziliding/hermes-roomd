package turns

import (
	"context"
	"math/rand/v2"
	"testing"

	"github.com/ziliding/hermes-roomd/internal/roster"
)

var bots = []string{"ada", "bram", "cleo", "dex"}

func rules(max int) *Rules { return NewRules(bots, max, rand.New(rand.NewPCG(1, 2))) }

func TestNext(t *testing.T) {
	tests := []struct {
		name   string
		st     State
		trig   Trigger
		want   string // "" = nobody
		notBot string // for random picks: must not be this bot
	}{
		{"user names a bot", State{}, Trigger{FromUser: true, Addressee: "cleo"}, "cleo", ""},
		{"bot names another bot", State{BotTurns: 1, LastBot: "ada"}, Trigger{Sender: "ada", Addressee: "dex"}, "dex", ""},
		{"cap reached: stop even if named", State{BotTurns: 2, LastBot: "ada"}, Trigger{Sender: "ada", Addressee: "cleo"}, "", ""},
		{"addressee equal to last speaker ignored", State{BotTurns: 1, LastBot: "cleo"}, Trigger{Sender: "cleo", Addressee: "cleo"}, "*", "cleo"},
		{"unknown addressee ignored", State{}, Trigger{FromUser: true, Addressee: "ghost"}, "*", ""},
		{"no addressee: random, never last speaker", State{BotTurns: 1, LastBot: "bram"}, Trigger{Sender: "bram"}, "*", "bram"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := rules(2).Next(context.Background(), tt.st, tt.trig)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tt.want == "":
				if ok {
					t.Fatalf("want nobody, got %q", got)
				}
			case tt.want == "*":
				if !ok || got == tt.notBot || !contains(bots, got) {
					t.Fatalf("want a random bot other than %q, got %q ok=%v", tt.notBot, got, ok)
				}
			default:
				if !ok || got != tt.want {
					t.Fatalf("want %q, got %q ok=%v", tt.want, got, ok)
				}
			}
		})
	}
}

func TestRandomFallbackIsInjectableAndCoversPool(t *testing.T) {
	a, _, _ := rules(2).Next(context.Background(), State{}, Trigger{FromUser: true})
	b, _, _ := rules(2).Next(context.Background(), State{}, Trigger{FromUser: true})
	if a != b {
		t.Fatalf("same seed gave %q then %q", a, b)
	}
	seen := map[string]bool{}
	r := rules(2)
	for range 200 {
		got, _, _ := r.Next(context.Background(), State{BotTurns: 1, LastBot: "ada"}, Trigger{Sender: "ada"})
		if got == "ada" {
			t.Fatal("picked the bot that just spoke")
		}
		seen[got] = true
	}
	if len(seen) != 3 {
		t.Fatalf("random pool should be the 3 other bots, saw %v", seen)
	}
}

// TestChainCapAndUserReset drives State the way the orchestrator does.
func TestChainCapAndUserReset(t *testing.T) {
	tests := []struct {
		name  string
		max   int
		turns int // expected bot turns after one user message
	}{
		{"default cap of 2", 2, 2},
		{"cap of 1 means only one reply", 1, 1},
		{"cap of 3", 3, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := rules(tt.max)
			var st State
			for round := range 2 { // second round proves the user resets counters
				st.UserSpoke()
				trig := Trigger{FromUser: true, Addressee: "ada"}
				spoke := 0
				for {
					next, ok, _ := r.Next(context.Background(), st, trig)
					if !ok {
						break
					}
					if spoke == 0 && next != "ada" {
						t.Fatalf("round %d: named bot should answer first, got %q", round, next)
					}
					st.BotSpoke(next)
					spoke++
					trig = Trigger{Sender: next}
					if spoke > 10 {
						t.Fatal("chain did not stop")
					}
				}
				if spoke != tt.turns {
					t.Fatalf("round %d: %d bot turns, want %d", round, spoke, tt.turns)
				}
			}
		})
	}
}

func TestAddresseeAtMostOne(t *testing.T) {
	r := roster.New("@alex:z", "Alex", []roster.Person{
		{Key: "ada", Display: "Ada", MXID: "@ada:z"},
		{Key: "cleo", Display: "Cleo", MXID: "@cleo:z"},
		{Key: "dex", Display: "Dex", MXID: "@dex:z"},
	})
	pill := func(id, text string) string { return `<a href="https://matrix.to/#/` + id + `">` + text + `</a>` }
	tests := []struct {
		name, body, formatted string
		mentions              []string
		exclude, want         string
	}{
		{"earliest named wins", "Cleo, what did Ada mean?", "", nil, "", "cleo"},
		{"handle counts", "@ada:z and @dex", "", nil, "", "ada"},
		{"case-insensitive name", "hey DEX", "", nil, "", "dex"},
		{"name inside a word does not count", "Cleopatra is far, Ada", "", nil, "", "ada"},
		{"no name -> none", "what should we eat?", "", nil, "", ""},
		{"user is not an addressee", "Alex said so", "", nil, "", ""},
		{"self-mention excluded", "I, Ada, think Cleo is right", "", nil, "ada", "cleo"},
		{"only self -> none", "Ada here", "", nil, "ada", ""},
		{"m.mentions without text", "what do you think?", "", []string{"@dex:z"}, "", "dex"},
		{"text beats mention-only", "Cleo?", "", []string{"@dex:z"}, "", "cleo"},
		{"pill hint", "hmm", "hmm " + pill("@ada:z", "N"), nil, "", "ada"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.Addressee(tt.body, tt.formatted, tt.mentions, tt.exclude); got != tt.want {
				t.Fatalf("Addressee(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func TestBusyBotsAreNeverPicked(t *testing.T) {
	tests := []struct {
		name string
		st   State
		trig Trigger
		ok   bool
	}{
		{"named busy bot is not picked", State{Busy: []string{"cleo"}}, Trigger{FromUser: true, Addressee: "cleo"}, true},
		{"bot names a busy bot", State{BotTurns: 1, LastBot: "ada", Busy: []string{"dex"}}, Trigger{Sender: "ada", Addressee: "dex"}, true},
		{"random pool skips busy", State{Busy: []string{"ada", "bram"}}, Trigger{FromUser: true}, true},
		{"everyone else busy: nobody", State{BotTurns: 1, LastBot: "ada", Busy: []string{"bram", "cleo", "dex"}}, Trigger{Sender: "ada"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i := 0; i < 50; i++ {
				r := NewRules(bots, 3, rand.New(rand.NewPCG(uint64(i), 7)))
				got, ok, _ := r.Next(context.Background(), tt.st, tt.trig)
				if ok != tt.ok {
					t.Fatalf("ok = %v, want %v (got %q)", ok, tt.ok, got)
				}
				if ok && (tt.st.IsBusy(got) || got == tt.st.LastBot) {
					t.Fatalf("picked %q with busy %v", got, tt.st.Busy)
				}
			}
		})
	}
}
