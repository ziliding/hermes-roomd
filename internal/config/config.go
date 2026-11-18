// Package config loads roomd's YAML config into typed structs and validates
// it at startup. Secrets are never in the file: it names environment
// variables, and Load resolves them so a missing secret fails fast.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// MsgType is the Matrix msgtype roomd posts bot replies with.
type MsgType string

const (
	MsgText   MsgType = "m.text"
	MsgNotice MsgType = "m.notice"
)

// Hermes is how roomd reaches one bot's Hermes profile.
type Hermes struct {
	// BaseURL selects the profile: e.g. http://127.0.0.1:8642/p/ada-group
	// (multiplexed gateway) or http://127.0.0.1:8651 (one port per profile).
	BaseURL string `yaml:"base_url"`
	KeyEnv  string `yaml:"key_env"`
	// Model is optional; when empty the request omits it and the profile's
	// default is used. It is NOT a profile selector.
	Model string `yaml:"model"`

	Key string `yaml:"-"` // resolved from KeyEnv
}

// Bot is one character: a Matrix account roomd posts as, plus a Hermes profile.
type Bot struct {
	Key            string `yaml:"-"` // map key, e.g. "ada"
	Display        string `yaml:"display"`
	MXID           string `yaml:"mxid"`
	AccessTokenEnv string `yaml:"access_token_env"`
	Hermes         Hermes `yaml:"hermes"`
	Blurb          string `yaml:"blurb"`

	AccessToken string `yaml:"-"` // resolved from AccessTokenEnv
}

// Router configures the optional LLM turn router (phase 2). With Enabled
// false (or the block absent) roomd uses the deterministic turn rules alone
// and nothing here is validated.
type Router struct {
	Enabled bool `yaml:"enabled"`
	// BaseURL is an OpenAI-style base that already includes /v1 (e.g.
	// https://api.venice.ai/api/v1); roomd appends /chat/completions.
	BaseURL string        `yaml:"base_url"`
	Model   string        `yaml:"model"`
	KeyEnv  string        `yaml:"key_env"`
	Timeout time.Duration `yaml:"timeout"` // per router call; on timeout the turn rules decide
	Window  int           `yaml:"window"`  // how many recent transcript messages the router sees
	// JSONMode sends response_format {"type":"json_object"}. Default true;
	// set false for endpoints that reject the parameter.
	JSONMode *bool `yaml:"json_mode"`

	Key string `yaml:"-"` // resolved from KeyEnv
}

// Memory configures the shared group bank in Hindsight (phase 3). Only
// roomd reads and writes it. With Enabled false (or the block absent) roomd
// behaves exactly as before and nothing here is validated.
type Memory struct {
	Enabled bool `yaml:"enabled"`
	// APIURL is the Hindsight server, without /v1 (e.g. http://localhost:8888).
	APIURL    string       `yaml:"api_url"`
	BankID    string       `yaml:"bank_id"`
	APIKeyEnv string       `yaml:"api_key_env"` // optional; a local Hindsight needs no key
	Recall    MemoryRecall `yaml:"recall"`
	Retain    MemoryRetain `yaml:"retain"`

	APIKey string `yaml:"-"` // resolved from APIKeyEnv when set
}

// MemoryRecall bounds the recall done before each called bot turn.
type MemoryRecall struct {
	Enabled   *bool         `yaml:"enabled"`    // default true (when memory is enabled)
	MaxTokens int           `yaml:"max_tokens"` // Hindsight max_tokens for the results
	Budget    string        `yaml:"budget"`     // low | mid | high
	Timeout   time.Duration `yaml:"timeout"`    // on timeout the turn goes ahead without memory
	// MinScore drops results whose relevance (reranker score, else final
	// score) is below it. Recall always returns its best matches, related
	// or not; this floor is what lets an off-topic turn get no block.
	MinScore *float64 `yaml:"min_score"`
}

// RetainMode is how a finished conversation is written to the group bank.
type RetainMode string

const (
	RetainSummary    RetainMode = "summary"    // a filtered summary written by the router's model
	RetainTranscript RetainMode = "transcript" // the attributed raw messages
)

// MemoryRetain configures the one write per idle conversation.
type MemoryRetain struct {
	Enabled     *bool         `yaml:"enabled"` // default true (when memory is enabled)
	Mode        RetainMode    `yaml:"mode"`
	IdleMinutes int           `yaml:"idle_minutes"` // quiet time before a conversation is written
	MinMessages int           `yaml:"min_messages"` // shorter conversations are skipped
	Timeout     time.Duration `yaml:"timeout"`      // per summarizer call and per retain call
}

// RecallOn reports whether recall runs (memory on and recall not disabled).
func (m Memory) RecallOn() bool { return m.Enabled && (m.Recall.Enabled == nil || *m.Recall.Enabled) }

// RetainOn reports whether retain runs (memory on and retain not disabled).
func (m Memory) RetainOn() bool { return m.Enabled && (m.Retain.Enabled == nil || *m.Retain.Enabled) }

// Room is one group room. In YAML it is either a plain room id string
// (every setting from the top level) or a mapping with overrides. Every
// override left empty takes the top-level value.
type Room struct {
	ID          string   `yaml:"id"`
	Name        string   `yaml:"name"`     // a label for logs; optional
	Bots        []string `yaml:"bots"`     // the cast: bot keys taking part (default: all bots)
	Listener    string   `yaml:"listener"` // whose roomd device runs /sync for this room and posts its notices
	MaxBotTurns int      `yaml:"max_bot_turns"`
	// BackgroundAfter overrides the top-level background_after for this room
	// (nil: inherit; 0: never hand a run off to the background).
	BackgroundAfter *Duration  `yaml:"background_after"`
	RulesFile       string     `yaml:"rules_file"` // this room's group rules (default: group_rules_file)
	Memory          RoomMemory `yaml:"memory"`

	Rules string `yaml:"-"` // contents of the effective rules file
}

// RoomMemory overrides the memory settings for one room. memory.enabled
// stays the global switch.
type RoomMemory struct {
	BankID string `yaml:"bank_id"` // default: memory.bank_id
	Recall struct {
		Enabled *bool `yaml:"enabled"`
	} `yaml:"recall"`
	Retain struct {
		Enabled     *bool `yaml:"enabled"`
		IdleMinutes int   `yaml:"idle_minutes"`
		MinMessages int   `yaml:"min_messages"`
	} `yaml:"retain"`
}

// UnmarshalYAML accepts a plain room id string or a mapping.
func (r *Room) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&r.ID)
	}
	type plain Room // no UnmarshalYAML: avoids recursion
	// Node.Decode does not inherit KnownFields, so re-decode strictly: a
	// misspelt room setting must fail at startup, like any other field.
	raw, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode((*plain)(r)); err != nil {
		// The re-decoded text starts at line 1, so name the file line here.
		return fmt.Errorf("room at line %d: %w", n.Line, err)
	}
	return nil
}

// Label names a room in logs: its name, else its id.
func (r *Room) Label() string {
	if r.Name != "" {
		return r.Name
	}
	return r.ID
}

// RoomMemoryConfig is the memory configuration in effect for one room.
type RoomMemoryConfig struct {
	BankID      string
	Recall      bool
	Retain      bool
	IdleMinutes int
	MinMessages int
}

// MemoryFor resolves the memory settings for room (top level + overrides).
func (c *Config) MemoryFor(r *Room) RoomMemoryConfig {
	m := c.Memory
	out := RoomMemoryConfig{
		BankID: m.BankID, Recall: m.RecallOn(), Retain: m.RetainOn(),
		IdleMinutes: m.Retain.IdleMinutes, MinMessages: m.Retain.MinMessages,
	}
	o := r.Memory
	if o.BankID != "" {
		out.BankID = o.BankID
	}
	if o.Recall.Enabled != nil {
		out.Recall = m.Enabled && *o.Recall.Enabled
	}
	if o.Retain.Enabled != nil {
		out.Retain = m.Enabled && *o.Retain.Enabled
	}
	if o.Retain.IdleMinutes > 0 {
		out.IdleMinutes = o.Retain.IdleMinutes
	}
	if o.Retain.MinMessages > 0 {
		out.MinMessages = o.Retain.MinMessages
	}
	return out
}

// RoomIDs returns the configured room ids in config order.
func (c *Config) RoomIDs() []string {
	ids := make([]string, 0, len(c.Rooms))
	for _, r := range c.Rooms {
		ids = append(ids, r.ID)
	}
	return ids
}

// ListenerGroups maps each listener bot to the ids of the rooms it syncs,
// in config order. roomd runs one /sync loop per key.
func (c *Config) ListenerGroups() map[string][]string {
	g := map[string][]string{}
	for _, r := range c.Rooms {
		g[r.Listener] = append(g[r.Listener], r.ID)
	}
	return g
}

// Config is the whole roomd configuration.
type Config struct {
	Homeserver     string        `yaml:"homeserver"`
	User           string        `yaml:"user"`
	UserDisplay    string        `yaml:"user_display"`
	Rooms          []*Room       `yaml:"rooms"`
	MaxBotTurns    int           `yaml:"max_bot_turns"`
	MaxDeltaLines  int           `yaml:"max_delta_lines"`
	MsgType        MsgType       `yaml:"msgtype"`
	StateDB        string        `yaml:"state_db"`
	GroupRulesFile string        `yaml:"group_rules_file"`
	HermesTimeout  time.Duration `yaml:"hermes_timeout"`
	// BackgroundAfter: a run still going after this long continues in the
	// background and the room moves on (nil: the default; 0: never).
	BackgroundAfter *Duration       `yaml:"background_after"`
	Listener        string          `yaml:"listener"` // bot key whose roomd device runs /sync
	Bots            map[string]*Bot `yaml:"bots"`
	Router          Router          `yaml:"router"`
	Memory          Memory          `yaml:"memory"`

	GroupRules string `yaml:"-"` // contents of GroupRulesFile
}

// Defaults applied when a field is left zero.
const (
	DefaultMaxBotTurns     = 2
	DefaultMaxDeltaLines   = 40
	DefaultHermesTimeout   = 5 * time.Minute
	DefaultBackgroundAfter = 20 * time.Second
	DefaultRouterTimeout   = 15 * time.Second
	DefaultRouterWindow    = 12

	DefaultMemoryBank        = "roomd-group"
	DefaultRecallMaxTokens   = 400
	DefaultRecallBudget      = "low"
	DefaultRecallTimeout     = 3 * time.Second
	DefaultRecallMinScore    = 0.25
	DefaultRetainIdleMinutes = 30
	DefaultRetainMinMessages = 4
	DefaultRetainTimeout     = 2 * time.Minute
	DefaultRetainMode        = RetainSummary
)

var botKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,31}$`)

// Load reads, defaults, resolves env secrets and validates the config at path.
// Relative state_db / group_rules_file paths resolve against the config's directory.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(raw, os.LookupEnv)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	dir := filepath.Dir(path)
	if !filepath.IsAbs(cfg.StateDB) {
		cfg.StateDB = filepath.Join(dir, cfg.StateDB)
	}
	if !filepath.IsAbs(cfg.GroupRulesFile) {
		cfg.GroupRulesFile = filepath.Join(dir, cfg.GroupRulesFile)
	}
	rules, err := os.ReadFile(cfg.GroupRulesFile)
	if err != nil {
		return nil, fmt.Errorf("group_rules_file: %w", err)
	}
	cfg.GroupRules = strings.TrimSpace(string(rules))
	if cfg.GroupRules == "" {
		return nil, fmt.Errorf("group_rules_file %s is empty", cfg.GroupRulesFile)
	}
	var errs []error
	for _, r := range cfg.Rooms {
		if r.RulesFile == "" {
			r.Rules = cfg.GroupRules
			continue
		}
		if !filepath.IsAbs(r.RulesFile) {
			r.RulesFile = filepath.Join(dir, r.RulesFile)
		}
		raw, err := os.ReadFile(r.RulesFile)
		if err != nil {
			errs = append(errs, fmt.Errorf("room %s: rules_file: %w", r.Label(), err))
			continue
		}
		if r.Rules = strings.TrimSpace(string(raw)); r.Rules == "" {
			errs = append(errs, fmt.Errorf("room %s: rules_file %s is empty", r.Label(), r.RulesFile))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes and validates YAML. lookupEnv resolves secret env vars.
func Parse(raw []byte, lookupEnv func(string) (string, bool)) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.validate(lookupEnv); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.MaxBotTurns == 0 {
		c.MaxBotTurns = DefaultMaxBotTurns
	}
	if c.MaxDeltaLines == 0 {
		c.MaxDeltaLines = DefaultMaxDeltaLines
	}
	if c.MsgType == "" {
		c.MsgType = MsgText
	}
	if c.StateDB == "" {
		c.StateDB = "roomd.db"
	}
	if c.GroupRulesFile == "" {
		c.GroupRulesFile = "rules.md"
	}
	if c.HermesTimeout == 0 {
		c.HermesTimeout = DefaultHermesTimeout
	}
	if c.BackgroundAfter == nil {
		d := Duration(DefaultBackgroundAfter)
		c.BackgroundAfter = &d
	}
	if c.UserDisplay == "" {
		c.UserDisplay = "Alex"
	}
	if c.Listener == "" {
		c.Listener = c.BotKeys().first()
	}
	for _, r := range c.Rooms {
		if r == nil {
			continue
		}
		if len(r.Bots) == 0 {
			r.Bots = c.BotKeys()
		} else {
			sort.Strings(r.Bots) // stable order, like the top-level bots
		}
		if r.MaxBotTurns == 0 {
			r.MaxBotTurns = c.MaxBotTurns
		}
		if r.BackgroundAfter == nil {
			r.BackgroundAfter = c.BackgroundAfter
		}
		if r.Listener == "" {
			r.Listener = keyList(r.Bots).first()
			for _, b := range r.Bots {
				if b == c.Listener {
					r.Listener = c.Listener // the top-level listener when it is in the cast
				}
			}
		}
	}
	if c.Router.Timeout == 0 {
		c.Router.Timeout = DefaultRouterTimeout
	}
	if c.Router.Window == 0 {
		c.Router.Window = DefaultRouterWindow
	}
	if c.Router.JSONMode == nil {
		on := true
		c.Router.JSONMode = &on
	}
	m := &c.Memory
	if m.BankID == "" {
		m.BankID = DefaultMemoryBank
	}
	if m.Recall.MaxTokens == 0 {
		m.Recall.MaxTokens = DefaultRecallMaxTokens
	}
	if m.Recall.Budget == "" {
		m.Recall.Budget = DefaultRecallBudget
	}
	if m.Recall.Timeout == 0 {
		m.Recall.Timeout = DefaultRecallTimeout
	}
	if m.Recall.MinScore == nil {
		v := DefaultRecallMinScore
		m.Recall.MinScore = &v
	}
	if m.Retain.Mode == "" {
		m.Retain.Mode = DefaultRetainMode
	}
	if m.Retain.IdleMinutes == 0 {
		m.Retain.IdleMinutes = DefaultRetainIdleMinutes
	}
	if m.Retain.MinMessages == 0 {
		m.Retain.MinMessages = DefaultRetainMinMessages
	}
	if m.Retain.Timeout == 0 {
		m.Retain.Timeout = DefaultRetainTimeout
	}
}

// BotKeys returns bot keys sorted, so iteration (system prompts, roster) is stable.
func (c *Config) BotKeys() keyList {
	keys := make(keyList, 0, len(c.Bots))
	for k := range c.Bots {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type keyList []string

func (k keyList) first() string {
	if len(k) == 0 {
		return ""
	}
	return k[0]
}

func (c *Config) validate(lookupEnv func(string) (string, bool)) error {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if !strings.HasPrefix(c.Homeserver, "http://") && !strings.HasPrefix(c.Homeserver, "https://") {
		fail("homeserver must be an http(s) URL")
	}
	if !isMXID(c.User) {
		fail("user %q is not a Matrix user id", c.User)
	}
	if len(c.Rooms) == 0 {
		fail("rooms must list at least one room id")
	}
	seenRoom := map[string]bool{}
	for i, room := range c.Rooms {
		if room == nil {
			fail("rooms[%d] is empty", i)
			continue
		}
		r := room.ID
		// Room IDs are "!opaque:server" before room version 12 and "!<hash>"
		// (no server part) from v12 on; aliases ("#…") are not resolved.
		if len(r) < 2 || r[0] != '!' || strings.ContainsAny(r, " \t\n") {
			fail("room %q is not a room id (copy it from Room settings → Advanced; aliases are not resolved)", r)
		}
		if seenRoom[r] {
			fail("room %q listed twice", r)
		}
		seenRoom[r] = true
	}
	if c.MaxBotTurns < 1 {
		fail("max_bot_turns must be >= 1")
	}
	if c.MaxDeltaLines < 1 {
		fail("max_delta_lines must be >= 1")
	}
	if c.MsgType != MsgText && c.MsgType != MsgNotice {
		fail("msgtype must be %q or %q", MsgText, MsgNotice)
	}
	if c.BackgroundAfter != nil {
		checkBackgroundAfter("background_after", time.Duration(*c.BackgroundAfter), c.HermesTimeout, fail)
	}
	if c.HermesTimeout < 0 {
		fail("hermes_timeout must be positive")
	}
	if len(c.Bots) == 0 {
		fail("bots must define at least one bot")
	}
	if _, ok := c.Bots[c.Listener]; !ok && len(c.Bots) > 0 {
		fail("listener %q is not a bot key", c.Listener)
	}

	names := map[string]string{strings.ToLower(c.UserDisplay): "user_display"}
	mxids := map[string]string{c.User: "user"}
	for _, key := range c.BotKeys() {
		b := c.Bots[key]
		if b == nil {
			fail("bots.%s is empty", key)
			continue
		}
		b.Key = key
		if !botKeyRe.MatchString(key) {
			fail("bot key %q must match %s (it is part of the Hermes session id)", key, botKeyRe)
		}
		if strings.TrimSpace(b.Display) == "" {
			fail("bots.%s.display is required", key)
		} else if prev, dup := names[strings.ToLower(b.Display)]; dup {
			fail("bots.%s.display %q collides with %s", key, b.Display, prev)
		} else {
			names[strings.ToLower(b.Display)] = "bots." + key
		}
		if !isMXID(b.MXID) {
			fail("bots.%s.mxid %q is not a Matrix user id", key, b.MXID)
		} else if prev, dup := mxids[b.MXID]; dup {
			fail("bots.%s.mxid %q collides with %s", key, b.MXID, prev)
		} else {
			mxids[b.MXID] = "bots." + key
		}
		if !strings.HasPrefix(b.Hermes.BaseURL, "http://") && !strings.HasPrefix(b.Hermes.BaseURL, "https://") {
			fail("bots.%s.hermes.base_url must be an http(s) URL", key)
		}
		b.AccessToken = resolveEnv(lookupEnv, b.AccessTokenEnv, "bots."+key+".access_token_env", fail)
		b.Hermes.Key = resolveEnv(lookupEnv, b.Hermes.KeyEnv, "bots."+key+".hermes.key_env", fail)
	}
	c.validateRooms(fail)
	if c.Router.Enabled {
		if !strings.HasPrefix(c.Router.BaseURL, "http://") && !strings.HasPrefix(c.Router.BaseURL, "https://") {
			fail("router.base_url must be an http(s) URL")
		}
		if strings.TrimSpace(c.Router.Model) == "" {
			fail("router.model is required when the router is enabled")
		}
		if c.Router.Timeout < 0 {
			fail("router.timeout must be positive")
		}
		if c.Router.Window < 1 {
			fail("router.window must be >= 1")
		}
		c.Router.Key = resolveEnv(lookupEnv, c.Router.KeyEnv, "router.key_env", fail)
	}
	c.validateMemory(lookupEnv, fail)
	return errors.Join(errs...)
}

func (c *Config) validateRooms(fail func(string, ...any)) {
	for _, r := range c.Rooms {
		if r == nil {
			continue
		}
		where := "room " + r.Label()
		inCast := map[string]bool{}
		for _, b := range r.Bots {
			if _, ok := c.Bots[b]; !ok {
				fail("%s: bots: %q is not a bot key", where, b)
			}
			if inCast[b] {
				fail("%s: bots: %q listed twice", where, b)
			}
			inCast[b] = true
		}
		if _, ok := c.Bots[r.Listener]; !ok {
			fail("%s: listener %q is not a bot key", where, r.Listener)
		} else if !inCast[r.Listener] {
			fail("%s: listener %q is not in the room's bots %v (it must be in the room to sync it)", where, r.Listener, r.Bots)
		}
		if r.MaxBotTurns < 1 {
			fail("%s: max_bot_turns must be >= 1", where)
		}
		if r.BackgroundAfter != nil && r.BackgroundAfter != c.BackgroundAfter {
			checkBackgroundAfter(where+": background_after", time.Duration(*r.BackgroundAfter), c.HermesTimeout, fail)
		}
		if r.Memory.BankID != "" && !bankIDRe.MatchString(r.Memory.BankID) {
			fail("%s: memory.bank_id %q must match %s", where, r.Memory.BankID, bankIDRe)
		}
		if r.Memory.Retain.IdleMinutes < 0 || r.Memory.Retain.MinMessages < 0 {
			fail("%s: memory.retain.idle_minutes and min_messages must be positive", where)
		}
	}
}

// Duration is a time.Duration ("20s", "1m30s") that also accepts a bare 0,
// meaning off.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode && n.Value == "0" {
		*d = 0
		return nil
	}
	var td time.Duration
	if err := n.Decode(&td); err != nil {
		return err
	}
	*d = Duration(td)
	return nil
}

// checkBackgroundAfter: 0 (off) or a positive duration shorter than
// hermes_timeout, which would otherwise stop every run first.
func checkBackgroundAfter(key string, d, hermesTimeout time.Duration, fail func(string, ...any)) {
	switch {
	case d < 0:
		fail("%s must be 0 (off) or a positive duration", key)
	case d > 0 && hermesTimeout > 0 && d >= hermesTimeout:
		fail("%s (%v) must be shorter than hermes_timeout (%v), or 0 to turn it off", key, d, hermesTimeout)
	}
}

func (c *Config) validateMemory(lookupEnv func(string) (string, bool), fail func(string, ...any)) {
	m := &c.Memory
	if !m.Enabled {
		return
	}
	if !strings.HasPrefix(m.APIURL, "http://") && !strings.HasPrefix(m.APIURL, "https://") {
		fail("memory.api_url must be an http(s) URL (the Hindsight server, without /v1)")
	}
	if !bankIDRe.MatchString(m.BankID) {
		fail("memory.bank_id %q must match %s", m.BankID, bankIDRe)
	}
	if m.APIKeyEnv != "" {
		m.APIKey = resolveEnv(lookupEnv, m.APIKeyEnv, "memory.api_key_env", fail)
	}
	if m.Recall.MaxTokens < 1 {
		fail("memory.recall.max_tokens must be >= 1")
	}
	switch m.Recall.Budget {
	case "low", "mid", "high":
	default:
		fail("memory.recall.budget must be low, mid or high")
	}
	if m.Recall.Timeout < 0 {
		fail("memory.recall.timeout must be positive")
	}
	if s := *m.Recall.MinScore; s < 0 || s > 1 {
		fail("memory.recall.min_score must be between 0 and 1")
	}
	if m.Retain.Mode != RetainSummary && m.Retain.Mode != RetainTranscript {
		fail("memory.retain.mode must be %q or %q", RetainSummary, RetainTranscript)
	}
	if m.Retain.IdleMinutes < 1 {
		fail("memory.retain.idle_minutes must be >= 1")
	}
	if m.Retain.MinMessages < 1 {
		fail("memory.retain.min_messages must be >= 1")
	}
	if m.Retain.Timeout < 0 {
		fail("memory.retain.timeout must be positive")
	}
	anyRetain := m.RetainOn()
	for _, r := range c.Rooms {
		if r != nil && c.MemoryFor(r).Retain {
			anyRetain = true
		}
	}
	if anyRetain && m.Retain.Mode == RetainSummary {
		// The summary is written by the router's model, which is configured
		// even when the router itself is off.
		r := &c.Router
		if !strings.HasPrefix(r.BaseURL, "http://") && !strings.HasPrefix(r.BaseURL, "https://") {
			fail("memory.retain.mode summary uses the router's model: router.base_url must be an http(s) URL (router.enabled may stay false)")
		}
		if strings.TrimSpace(r.Model) == "" {
			fail("memory.retain.mode summary uses the router's model: router.model is required (router.enabled may stay false)")
		}
		if !r.Enabled { // when enabled, the key was resolved above
			r.Key = resolveEnv(lookupEnv, r.KeyEnv, "router.key_env (for memory.retain.mode summary)", fail)
		}
	}
}

var bankIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-]{0,63}$`)

// PlaceholderBlurb is the blurb the example config ships with.
const PlaceholderBlurb = "One-line character description"

// MissingBlurbs returns the bot keys whose blurb is empty or still the
// example placeholder. The router tells characters apart by their blurbs.
func (c *Config) MissingBlurbs() []string {
	var keys []string
	for _, k := range c.BotKeys() {
		if b := strings.TrimSpace(c.Bots[k].Blurb); b == "" || strings.EqualFold(b, PlaceholderBlurb) {
			keys = append(keys, k)
		}
	}
	return keys
}

func resolveEnv(lookupEnv func(string) (string, bool), name, field string, fail func(string, ...any)) string {
	if name == "" {
		fail("%s is required", field)
		return ""
	}
	v, ok := lookupEnv(name)
	if !ok || strings.TrimSpace(v) == "" {
		fail("%s: environment variable %s is not set", field, name)
		return ""
	}
	return strings.TrimSpace(v)
}

func isMXID(s string) bool {
	return strings.HasPrefix(s, "@") && strings.Contains(s, ":") && !strings.ContainsAny(s, " \t")
}
