package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const good = `
homeserver: http://127.0.0.1:8008
user: "@alex:example.com"
rooms: ["!abc:example.com"]
bots:
  ada:
    display: Ada
    mxid: "@ada:example.com"
    access_token_env: T_ADA
    hermes: { base_url: "http://127.0.0.1:8642/p/ada-group", key_env: K_ADA }
  cleo:
    display: Cleo
    mxid: "@cleo:example.com"
    access_token_env: T_CLEO
    hermes: { base_url: "http://127.0.0.1:8642/p/cleo-group", key_env: K_CLEO }
`

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

var allEnv = map[string]string{"T_ADA": "tn", "K_ADA": "kn", "T_CLEO": "tu", "K_CLEO": "ku"}

func TestParseDefaultsAndSecrets(t *testing.T) {
	c, err := Parse([]byte(good), env(allEnv))
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxBotTurns != 2 || c.MsgType != MsgText || c.UserDisplay != "Alex" || c.Listener != "ada" || c.HermesTimeout != DefaultHermesTimeout {
		t.Fatalf("defaults not applied: %+v", c)
	}
	if c.Bots["cleo"].AccessToken != "tu" || c.Bots["cleo"].Hermes.Key != "ku" || c.Bots["cleo"].Key != "cleo" {
		t.Fatalf("secrets not resolved: %+v", c.Bots["cleo"])
	}
}

func TestParseRoomIDShapes(t *testing.T) {
	for _, id := range []string{"!abc:example.com", "!K2xh2cckIpNY0GU1mLjjGKFzK9MS1veWcGbeKysPcoU"} {
		y := strings.Replace(good, "!abc:example.com", id, 1)
		if _, err := Parse([]byte(y), env(allEnv)); err != nil {
			t.Errorf("room %q rejected: %v", id, err)
		}
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name, yaml string
		env        map[string]string
		wantErr    string
	}{
		{"missing secret", good, map[string]string{"T_ADA": "x", "K_ADA": "x", "T_CLEO": "x"}, "K_CLEO is not set"},
		{"unknown field", good + "bogus: 1\n", allEnv, "bogus"},
		{"bad msgtype", good + "msgtype: m.emote\n", allEnv, "msgtype"},
		{"negative turns", good + "max_bot_turns: -1\n", allEnv, "max_bot_turns"},
		{"alias instead of room id", strings.Replace(good, "!abc:example.com", "#group:example.com", 1), allEnv, "not a room id"},
		{"display collides with user", good + "user_display: Ada\n", allEnv, "collides"},
		{"display collides case-insensitive", strings.Replace(good, "display: Cleo", "display: ADA", 1), allEnv, "collides"},
		{"listener not a bot", good + "listener: bram\n", allEnv, "listener"},
		{"bad bot key", strings.Replace(good, "  cleo:\n", "  Cleo-Bot:\n", 1), allEnv, "bot key"},
		{"router enabled without key env", good + routerOn, allEnv, "ROOMD_ROUTER_KEY is not set"},
		{"router enabled without model", good + strings.Replace(routerOn, "  model: flash\n", "", 1), withRouterKey, "router.model"},
		{"router enabled with bad url", good + strings.Replace(routerOn, "https://api.example.com/v1", "api.example.com", 1), withRouterKey, "router.base_url"},
		{"router unknown field", good + routerOn + "  bogus: 1\n", withRouterKey, "bogus"},
		{"relative media dir", good + "media_dirs: [output]\n", allEnv, "media_dirs: \"output\" must be an absolute path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml), env(tt.env))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

const routerOn = `router:
  enabled: true
  base_url: https://api.example.com/v1
  model: flash
  key_env: ROOMD_ROUTER_KEY
`

var withRouterKey = func() map[string]string {
	m := map[string]string{"ROOMD_ROUTER_KEY": "rk"}
	for k, v := range allEnv {
		m[k] = v
	}
	return m
}()

func TestRouterConfig(t *testing.T) {
	// Absent block: router off, no key needed (phase-1 configs keep working).
	c, err := Parse([]byte(good), env(allEnv))
	if err != nil {
		t.Fatal(err)
	}
	if c.Router.Enabled {
		t.Fatal("router enabled by default")
	}
	// Disabled block: nothing validated, key env not required.
	if _, err := Parse([]byte(good+"router:\n  enabled: false\n  key_env: ROOMD_ROUTER_KEY\n"), env(allEnv)); err != nil {
		t.Fatalf("disabled router block rejected: %v", err)
	}
	c, err = Parse([]byte(good+routerOn), env(withRouterKey))
	if err != nil {
		t.Fatal(err)
	}
	r := c.Router
	if !r.Enabled || r.Key != "rk" || r.Timeout != DefaultRouterTimeout || r.Window != DefaultRouterWindow || r.JSONMode == nil || !*r.JSONMode {
		t.Fatalf("router defaults/secrets: %+v", r)
	}
	c, err = Parse([]byte(good+routerOn+"  json_mode: false\n  window: 5\n  timeout: 3s\n"), env(withRouterKey))
	if err != nil {
		t.Fatal(err)
	}
	if *c.Router.JSONMode || c.Router.Window != 5 || c.Router.Timeout != 3*time.Second {
		t.Fatalf("router overrides: %+v", c.Router)
	}
}

func TestShippedExampleConfigLoads(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "rules"), 0o700); err != nil {
		t.Fatal(err)
	}
	for src, dst := range map[string]string{"../../deploy/roomd.example.yaml": "roomd.yaml", "../../deploy/rules/rules.md": "rules/rules.md"} {
		raw, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, dst), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, bot := range []string{"ADA", "CLEO", "BRAM", "DEX"} {
		t.Setenv("ROOMD_"+bot+"_TOKEN", "tok")
		t.Setenv("ROOMD_"+bot+"_API_KEY", "key")
	}
	c, err := Load(filepath.Join(dir, "roomd.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Bots) != 4 || c.HermesTimeout != 5*time.Minute || !strings.Contains(c.GroupRules, "{{name}}") {
		t.Fatalf("unexpected example config: bots=%d timeout=%v", len(c.Bots), c.HermesTimeout)
	}
	if c.StateDB != filepath.Join(dir, "roomd.db") {
		t.Fatalf("state_db not resolved relative to config: %s", c.StateDB)
	}
}

func TestMissingBlurbs(t *testing.T) {
	y := strings.Replace(good, "    hermes: { base_url: \"http://127.0.0.1:8642/p/cleo-group\", key_env: K_CLEO }\n",
		"    hermes: { base_url: \"http://127.0.0.1:8642/p/cleo-group\", key_env: K_CLEO }\n    blurb: a cheerful singer\n", 1)
	c, err := Parse([]byte(y), env(allEnv))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.MissingBlurbs(); len(got) != 1 || got[0] != "ada" {
		t.Fatalf("MissingBlurbs = %v, want [ada]", got)
	}
	c.Bots["ada"].Blurb = " " + PlaceholderBlurb
	if got := c.MissingBlurbs(); len(got) != 1 || got[0] != "ada" {
		t.Fatalf("placeholder not detected: %v", got)
	}
}

const memoryOn = `
memory:
  enabled: true
  api_url: http://localhost:8888
`

const routerModelOnly = `
router:
  enabled: false
  base_url: https://api.example.com/v1
  model: cheap
  key_env: ROOMD_ROUTER_KEY
`

func TestMemoryConfigDefaults(t *testing.T) {
	// Absent block: memory off, nothing required (phase-2 configs keep working).
	c, err := Parse([]byte(good), env(allEnv))
	if err != nil {
		t.Fatal(err)
	}
	if c.Memory.Enabled || c.Memory.RecallOn() || c.Memory.RetainOn() {
		t.Fatalf("memory on by default: %+v", c.Memory)
	}
	// Disabled block with junk: not validated.
	if _, err := Parse([]byte(good+"memory:\n  enabled: false\n  api_url: nope\n  retain: {mode: bogus}\n"), env(allEnv)); err != nil {
		t.Fatalf("disabled memory block rejected: %v", err)
	}

	c, err = Parse([]byte(good+routerModelOnly+memoryOn), env(withRouterKey))
	if err != nil {
		t.Fatal(err)
	}
	m := c.Memory
	if !m.RecallOn() || !m.RetainOn() || m.BankID != "roomd-group" || m.APIKey != "" ||
		m.Recall.MaxTokens != DefaultRecallMaxTokens || m.Recall.Budget != "low" || m.Recall.Timeout != DefaultRecallTimeout ||
		*m.Recall.MinScore != DefaultRecallMinScore || m.Retain.Mode != RetainSummary || m.Retain.IdleMinutes != DefaultRetainIdleMinutes ||
		m.Retain.MinMessages != DefaultRetainMinMessages || m.Retain.Timeout != DefaultRetainTimeout {
		t.Fatalf("memory defaults: %+v", m)
	}
	if c.Router.Key != "rk" {
		t.Fatal("summary mode must resolve the router key even with the router off")
	}
}

func TestMemoryConfigValidation(t *testing.T) {
	tests := []struct {
		name, yaml string
		env        map[string]string
		wantErr    string
	}{
		{"transcript mode needs no router", good + memoryOn + "  retain: {mode: transcript}\n", allEnv, ""},
		{"recall only needs no router", good + memoryOn + "  retain: {enabled: false}\n", allEnv, ""},
		{"summary needs router model", good + memoryOn, allEnv, "router.base_url"},
		{"summary needs router key", good + routerModelOnly + memoryOn, allEnv, "ROOMD_ROUTER_KEY"},
		{"summary with router on", good + routerOn + memoryOn, withRouterKey, ""},
		{"bad url", good + routerModelOnly + "memory:\n  enabled: true\n  api_url: localhost:8888\n", withRouterKey, "memory.api_url"},
		{"bad budget", good + routerModelOnly + memoryOn + "  recall: {budget: huge}\n", withRouterKey, "memory.recall.budget"},
		{"bad mode", good + routerModelOnly + memoryOn + "  retain: {mode: diary}\n", withRouterKey, "memory.retain.mode"},
		{"bad idle", good + routerModelOnly + memoryOn + "  retain: {idle_minutes: -5}\n", withRouterKey, "idle_minutes"},
		{"bad min messages", good + routerModelOnly + memoryOn + "  retain: {min_messages: -1}\n", withRouterKey, "min_messages"},
		{"bad min score", good + routerModelOnly + memoryOn + "  recall: {min_score: 2}\n", withRouterKey, "min_score"},
		{"bad recall timeout", good + routerModelOnly + memoryOn + "  recall: {timeout: -1s}\n", withRouterKey, "recall.timeout"},
		{"bad bank", good + routerModelOnly + memoryOn + "  bank_id: \"a b\"\n", withRouterKey, "bank_id"},
		{"missing api key env", good + routerModelOnly + memoryOn + "  api_key_env: HS_KEY\n", withRouterKey, "HS_KEY"},
		{"unknown field", good + routerModelOnly + memoryOn + "  recal: {}\n", withRouterKey, "recal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml), env(tt.env))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("err %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}
