package turns

import (
	"context"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/ziliding/hermes-roomd/internal/roster"
)

var bots = []string{"ada", "bram", "cleo", "dex"}

func rules(max int) *Rules { return NewRules(bots, max, rand.New(rand.NewPCG(1, 2))) }

func TestPlan(t *testing.T) {
	tests := []struct {
		name   string
		st     State
		trig   Trigger
		want   []string // nil = nobody; {"*"} = one random bot
		notBot string   // for random picks: must not be this bot
	}{
		{"user names a bot", State{}, Trigger{FromUser: true, Addressees: []string{"cleo"}}, []string{"cleo"}, ""},
		{"user names several bots: all owe, in order", State{}, Trigger{FromUser: true, Addressees: []string{"dex", "ada"}}, []string{"dex", "ada"}, ""},
		{"bot names another bot", State{BotTurns: 1, LastBot: "ada"}, Trigger{Sender: "ada", Addressees: []string{"dex"}}, []string{"dex"}, ""},
		{"cap reached: stop even if named", State{BotTurns: 2, LastBot: "ada"}, Trigger{Sender: "ada", Addressees: []string{"cleo"}}, nil, ""},
		{"addressee equal to last speaker ignored", State{BotTurns: 1, LastBot: "cleo"}, Trigger{Sender: "cleo", Addressees: []string{"cleo"}}, []string{"*"}, "cleo"},
		{"unknown addressee ignored", State{}, Trigger{FromUser: true, Addressees: []string{"ghost"}}, []string{"*"}, ""},
		{"no addressee: random, never last speaker", State{BotTurns: 1, LastBot: "bram"}, Trigger{Sender: "bram"}, []string{"*"}, "bram"},
		{"!continue cap above the configured one: go on", State{BotTurns: 2, LastBot: "ada", Cap: 4}, Trigger{Sender: "ada", Addressees: []string{"cleo"}}, []string{"cleo"}, ""},
		{"!continue cap reached: stop", State{BotTurns: 4, LastBot: "ada", Cap: 4}, Trigger{Sender: "ada", Addressees: []string{"cleo"}}, nil, ""},
		{"!continue cap below the configured one: stop", State{BotTurns: 1, LastBot: "ada", Cap: 1}, Trigger{Sender: "ada", Addressees: []string{"cleo"}}, nil, ""},
		{"cap reached but bots still owe the user: they speak", State{BotTurns: 2, LastBot: "ada", Owed: []string{"cleo"}}, Trigger{Sender: "ada"}, []string{"cleo"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := rules(2).Plan(context.Background(), tt.st, tt.trig)
			if err != nil {
				t.Fatal(err)
			}
			if len(tt.want) == 1 && tt.want[0] == "*" {
				if len(got) != 1 || got[0] == tt.notBot || !contains(bots, got[0]) {
					t.Fatalf("want one random bot other than %q, got %v", tt.notBot, got)
				}
				return
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// A message to the whole group is owed by every free bot, the named ones
// first; the cap plays no part.
func TestGroupMessageIsOwedByEveryFreeBot(t *testing.T) {
	got, _ := rules(0).Plan(context.Background(), State{Busy: []string{"bram"}},
		Trigger{FromUser: true, Group: true, Addressees: []string{"dex"}})
	if len(got) != 3 || got[0] != "dex" || contains(got, "bram") {
		t.Fatalf("plan %v: want dex first, then ada and cleo, never busy bram", got)
	}
}

func TestRandomFallbackIsInjectableAndCoversPool(t *testing.T) {
	a, _ := rules(2).Plan(context.Background(), State{}, Trigger{FromUser: true})
	b, _ := rules(2).Plan(context.Background(), State{}, Trigger{FromUser: true})
	if !slices.Equal(a, b) {
		t.Fatalf("same seed gave %v then %v", a, b)
	}
	seen := map[string]bool{}
	r := rules(2)
	for range 200 {
		got, _ := r.Plan(context.Background(), State{BotTurns: 1, LastBot: "ada"}, Trigger{Sender: "ada"})
		if got[0] == "ada" {
			t.Fatal("picked the bot that just spoke")
		}
		seen[got[0]] = true
	}
	if len(seen) != 3 {
		t.Fatalf("random pool should be the 3 other bots, saw %v", seen)
	}
}

// TestChainCapAndUserReset drives State the way the orchestrator does: the
// named bot answers (an obligation, not counted), then max bot-to-bot turns.
func TestChainCapAndUserReset(t *testing.T) {
	for _, max := range []int{0, 1, 2, 3} {
		r := rules(max)
		var st State
		for round := range 2 { // second round proves the user resets counters
			st.UserSpoke()
			plan, _ := r.Plan(context.Background(), st, Trigger{FromUser: true, Addressees: []string{"ada"}})
			if !slices.Equal(plan, []string{"ada"}) {
				t.Fatalf("max %d round %d: named bot should answer first, got %v", max, round, plan)
			}
			st.BotSpoke("ada", false)
			spoke, last := 1, "ada"
			for {
				plan, _ := r.Plan(context.Background(), st, Trigger{Sender: last})
				if len(plan) == 0 {
					break
				}
				st.BotSpoke(plan[0], true)
				spoke, last = spoke+1, plan[0]
				if spoke > 10 {
					t.Fatal("chain did not stop")
				}
			}
			if spoke != max+1 {
				t.Fatalf("max %d round %d: %d bot turns, want %d", max, round, spoke, max+1)
			}
		}
	}
}

func TestAddressees(t *testing.T) {
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
	all := []struct {
		body     string
		mentions []string
		want     []string
	}{
		{"Cleo, Ada, what do you think?", nil, []string{"cleo", "ada"}},
		{"Dex? and you, @ada:z", []string{"@cleo:z"}, []string{"dex", "ada", "cleo"}},
		{"what should we eat?", nil, nil},
	}
	for _, tt := range all {
		if got := r.Addressees(tt.body, "", tt.mentions, ""); !slices.Equal(got, tt.want) {
			t.Errorf("Addressees(%q) = %v, want %v", tt.body, got, tt.want)
		}
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
		{"named busy bot is not picked", State{Busy: []string{"cleo"}}, Trigger{FromUser: true, Addressees: []string{"cleo"}}, true},
		{"bot names a busy bot", State{BotTurns: 1, LastBot: "ada", Busy: []string{"dex"}}, Trigger{Sender: "ada", Addressees: []string{"dex"}}, true},
		{"random pool skips busy", State{Busy: []string{"ada", "bram"}}, Trigger{FromUser: true}, true},
		{"everyone else busy: nobody", State{BotTurns: 1, LastBot: "ada", Busy: []string{"bram", "cleo", "dex"}}, Trigger{Sender: "ada"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i := 0; i < 50; i++ {
				r := NewRules(bots, 3, rand.New(rand.NewPCG(uint64(i), 7)))
				plan, _ := r.Plan(context.Background(), tt.st, tt.trig)
				if (len(plan) > 0) != tt.ok {
					t.Fatalf("plan %v, want someone = %v", plan, tt.ok)
				}
				for _, got := range plan {
					if tt.st.IsBusy(got) || got == tt.st.LastBot {
						t.Fatalf("picked %q with busy %v", got, tt.st.Busy)
					}
				}
			}
		})
	}
}
