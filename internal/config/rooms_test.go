package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const threeBots = `
homeserver: http://127.0.0.1:8008
user: "@alex:example.com"
max_bot_turns: 2
listener: ada
bots:
  ada:   { display: Ada,   mxid: "@ada:example.com",   access_token_env: T_ADA,   hermes: { base_url: "http://h/p/ada",   key_env: K_ADA } }
  cleo:    { display: Cleo,    mxid: "@cleo:example.com",    access_token_env: T_CLEO,    hermes: { base_url: "http://h/p/cleo",    key_env: K_CLEO } }
  bram: { display: Bram, mxid: "@bram:example.com", access_token_env: T_BRAM, hermes: { base_url: "http://h/p/bram", key_env: K_BRAM } }
`

var threeEnv = map[string]string{"T_ADA": "a", "K_ADA": "a", "T_CLEO": "a", "K_CLEO": "a", "T_BRAM": "a", "K_BRAM": "a"}

func TestRoomsBothForms(t *testing.T) {
	y := threeBots + `
rooms:
  - "!plain:example.com"
  - id: "!lounge:example.com"
    name: lounge
    bots: [cleo, bram]
    listener: bram
    max_bot_turns: 3
    memory: { bank_id: roomd-lounge, retain: { idle_minutes: 10 } }
  - id: "!duo:example.com"
    bots: [cleo, bram]
`
	c, err := Parse([]byte(y), env(threeEnv))
	if err != nil {
		t.Fatal(err)
	}
	plain, lounge, duo := c.Rooms[0], c.Rooms[1], c.Rooms[2]
	if plain.ID != "!plain:example.com" || !reflect.DeepEqual(plain.Bots, []string{"ada", "bram", "cleo"}) ||
		plain.Listener != "ada" || plain.MaxBotTurns != 2 || plain.Label() != plain.ID {
		t.Fatalf("plain room defaults: %+v", plain)
	}
	if !reflect.DeepEqual(lounge.Bots, []string{"bram", "cleo"}) || lounge.Listener != "bram" || lounge.MaxBotTurns != 3 || lounge.Label() != "lounge" {
		t.Fatalf("lounge overrides: %+v", lounge)
	}
	if duo.Listener != "bram" {
		t.Fatalf("duo listener %q: the top-level listener (ada) is not in its cast, so the first cast bot listens", duo.Listener)
	}
	if !reflect.DeepEqual(c.RoomIDs(), []string{"!plain:example.com", "!lounge:example.com", "!duo:example.com"}) {
		t.Fatalf("RoomIDs %v", c.RoomIDs())
	}
	want := map[string][]string{"ada": {"!plain:example.com"}, "bram": {"!lounge:example.com", "!duo:example.com"}}
	if got := c.ListenerGroups(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ListenerGroups %v, want %v (one sync loop per distinct listener)", got, want)
	}
}

func TestRoomMemoryOverrides(t *testing.T) {
	y := threeBots + `
rooms:
  - "!a:z"
  - id: "!b:z"
    memory: { bank_id: roomd-b, recall: { enabled: false }, retain: { idle_minutes: 5, min_messages: 2 } }
memory:
  enabled: true
  api_url: http://localhost:8888
  retain: { mode: transcript }
`
	c, err := Parse([]byte(y), env(threeEnv))
	if err != nil {
		t.Fatal(err)
	}
	a, b := c.MemoryFor(c.Rooms[0]), c.MemoryFor(c.Rooms[1])
	if a != (RoomMemoryConfig{BankID: "roomd-group", Recall: true, Retain: true, IdleMinutes: 30, MinMessages: 4}) {
		t.Fatalf("room a memory %+v", a)
	}
	if b != (RoomMemoryConfig{BankID: "roomd-b", Recall: false, Retain: true, IdleMinutes: 5, MinMessages: 2}) {
		t.Fatalf("room b memory %+v", b)
	}
	// memory.enabled stays the global switch.
	c.Memory.Enabled = false
	if m := c.MemoryFor(c.Rooms[1]); m.Recall || m.Retain {
		t.Fatalf("memory off globally, room b still has %+v", m)
	}
}

func TestRoomValidation(t *testing.T) {
	tests := []struct{ name, rooms, wantErr string }{
		{"unknown bot", `[{id: "!a:z", bots: [ada, dex]}]`, `"dex" is not a bot key`},
		{"bot twice", `[{id: "!a:z", bots: [ada, ada]}]`, `"ada" listed twice`},
		{"listener not in cast", `[{id: "!a:z", bots: [cleo], listener: ada}]`, `listener "ada" is not in the room's bots`},
		{"listener unknown", `[{id: "!a:z", listener: dex}]`, `listener "dex" is not a bot key`},
		{"duplicate id", `["!a:z", {id: "!a:z", name: again}]`, `"!a:z" listed twice`},
		{"alias", `[{id: "#lounge:z"}]`, "is not a room id"},
		{"bad max turns", `[{id: "!a:z", max_bot_turns: -1}]`, "max_bot_turns must be >= 1"},
		{"bad bank", `[{id: "!a:z", memory: {bank_id: "a b"}}]`, "memory.bank_id"},
		{"unknown field", `[{id: "!a:z", listner: ada}]`, "listner"},
		{"unknown field names the line", "\n  - id: \"!a:z\"\n    memory: {bank-id: x}", "room at line"},
		{"unknown memory field", `[{id: "!a:z", memory: {bank: x}}]`, "bank"},
		{"empty list", `[]`, "at least one room"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(threeBots+"rooms: "+tt.rooms+"\n"), env(threeEnv))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestRoomRetainOnNeedsSummaryModel(t *testing.T) {
	// Retain off globally but on in one room: summary mode still needs the router's model.
	y := threeBots + `
rooms: [{id: "!a:z", memory: {retain: {enabled: true}}}]
memory: {enabled: true, api_url: "http://localhost:8888", retain: {enabled: false}}
`
	if _, err := Parse([]byte(y), env(threeEnv)); err == nil || !strings.Contains(err.Error(), "router.base_url") {
		t.Fatalf("err %v", err)
	}
}

func TestRoomRulesFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("rules.md", "default rules for {{name}}\n")
	write("rules-lounge.md", "lounge rules for {{name}}\n")
	write("roomd.yaml", threeBots+`
rooms:
  - "!a:z"
  - { id: "!b:z", rules_file: rules-lounge.md }
`)
	for k := range threeEnv {
		t.Setenv(k, "x")
	}
	c, err := Load(filepath.Join(dir, "roomd.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Rooms[0].Rules != "default rules for {{name}}" || c.Rooms[1].Rules != "lounge rules for {{name}}" {
		t.Fatalf("rules %q / %q", c.Rooms[0].Rules, c.Rooms[1].Rules)
	}
	write("roomd.yaml", threeBots+`rooms: [{ id: "!b:z", name: lounge, rules_file: missing.md }]`+"\n")
	if _, err := Load(filepath.Join(dir, "roomd.yaml")); err == nil || !strings.Contains(err.Error(), "room lounge: rules_file") {
		t.Fatalf("missing rules file: err %v", err)
	}
	write("empty.md", "  \n")
	write("roomd.yaml", threeBots+`rooms: [{ id: "!b:z", rules_file: empty.md }]`+"\n")
	if _, err := Load(filepath.Join(dir, "roomd.yaml")); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("empty rules file: err %v", err)
	}
}

func TestBackgroundAfter(t *testing.T) {
	rooms := `
rooms:
  - "!plain:example.com"
  - { id: "!off:example.com", background_after: 0s }
  - { id: "!slow:example.com", background_after: 45s }
`
	tests := []struct {
		name    string
		top     string
		want    []time.Duration // per room
		wantErr string
	}{
		{"default 20s, rooms override", "", []time.Duration{20 * time.Second, 0, 45 * time.Second}, ""},
		{"top level 0 turns it off", "background_after: 0\n", []time.Duration{0, 0, 45 * time.Second}, ""},
		{"top level value", "background_after: 10s\n", []time.Duration{10 * time.Second, 0, 45 * time.Second}, ""},
		{"negative", "background_after: -1s\n", nil, "background_after must be 0 (off)"},
		{"not below hermes_timeout", "hermes_timeout: 30s\n", nil, "room !slow:example.com: background_after (45s) must be shorter than hermes_timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Parse([]byte(threeBots+tt.top+rooms), env(threeEnv))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for i, r := range c.Rooms {
				if time.Duration(*r.BackgroundAfter) != tt.want[i] {
					t.Errorf("room %s: %v, want %v", r.ID, *r.BackgroundAfter, tt.want[i])
				}
			}
		})
	}
}
