// Package router is the LLM turn router. It implements turns.Decider by
// asking a separate cheap model (any OpenAI-compatible chat-completions
// endpoint) for a response plan, as strict JSON:
//
//	{"addressed_to": [...] | "all" | "user" | "none",
//	 "user_question_pending": true | false,
//	 "plan": ["<name>", ...],
//	 "reason": "..."}
//
// The analysis fields come before plan, so the model classifies the message
// before it picks anyone.
//
// Deterministic code stays in front of the model:
//   - a user message that names bots (and doesn't address the group) is owed
//     by exactly those bots, with no call;
//   - while exactly one bot still owes the user a reply, it speaks, with no
//     call; while several do, the model only orders them and cannot drop any
//     (the orchestrator keeps the obligations);
//   - the bot-to-bot cap (MaxBotTurns) is checked before any call for a
//     continuation turn, and user_question_pending only stops continuation;
//   - names are validated: unknown, busy, and (for continuation) the bot
//     that just spoke are dropped;
//   - if the call fails or times out, the fallback Decider (turns.Rules)
//     decides instead; so does an unusable answer, except between bots with
//     nobody owed a reply, where it means nobody speaks.
package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ziliding/hermes-roomd/internal/contextbuild"
	"github.com/ziliding/hermes-roomd/internal/roster"
	"github.com/ziliding/hermes-roomd/internal/silence"
	"github.com/ziliding/hermes-roomd/internal/transcript"
	"github.com/ziliding/hermes-roomd/internal/turns"
)

// Transcript is the part of transcript.Store the router reads.
type Transcript interface {
	Since(ctx context.Context, roomID string, after int64, limit int) ([]transcript.Entry, error)
}

// Options configures New.
type Options struct {
	BaseURL  string // OpenAI-style base including /v1; /chat/completions is appended
	APIKey   string
	Model    string
	Timeout  time.Duration // per call; 0 means no extra timeout
	Window   int           // recent transcript messages shown to the model
	JSONMode bool          // send response_format {"type":"json_object"}
	HTTP     *http.Client  // optional

	Transcript  Transcript
	Roster      *roster.Roster
	Blurbs      map[string]string // bot key -> one-line description
	MaxBotTurns int
	Fallback    turns.Decider // used when the router call fails or answers nonsense
	Log         *slog.Logger
	// CallLog, if set, receives one JSON line per model call (see CallRecord),
	// for building an eval set. It must be safe to write from one goroutine
	// at a time; the router serialises its writes.
	CallLog io.Writer
}

// Router implements turns.Decider.
type Router struct {
	o       Options
	builder *contextbuild.Builder
	system  string // byte-identical on every call (prompt-cache friendly)
	logMu   sync.Mutex
}

// New builds a Router.
func New(o Options) *Router {
	if o.HTTP == nil {
		o.HTTP = &http.Client{}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Router{o: o, builder: contextbuild.New(o.Roster), system: SystemPrompt(o.Roster, o.Blurbs)}
}

// Plan implements turns.Decider. It never returns an error: a failed or
// unusable router call falls back to the fallback Decider.
func (r *Router) Plan(ctx context.Context, st turns.State, t turns.Trigger) ([]string, error) {
	log := r.o.Log.With("room", t.RoomID)
	if t.FromUser {
		if named := turns.Named(r.cast(), st, t); len(named) > 0 && !t.Group {
			log.Debug("router: the user named bots; they answer", "plan", named)
			return named, nil
		}
	} else {
		switch {
		case len(st.Owed) == 1:
			return st.Owed, nil
		case len(st.Owed) == 0 && st.BotTurns >= r.o.MaxBotTurns:
			log.Debug("router: bot-turn cap reached; waiting for the user", "bot_turns", st.BotTurns)
			return nil, nil
		}
	}

	entries, err := r.o.Transcript.Since(ctx, t.RoomID, 0, r.o.Window)
	if err != nil {
		log.Warn("router: reading transcript failed; using the turn rules", "err", err)
		return r.o.Fallback.Plan(ctx, st, t)
	}
	ans, err := r.ask(ctx, st, t, entries)
	var bad *badAnswer
	switch {
	case errors.As(err, &bad) && !t.FromUser && len(st.Owed) == 0:
		// Nobody is owed a reply: a nonsense answer means nobody, not a
		// random bot-to-bot turn.
		log.Warn("router: unusable answer; nobody speaks", "err", err)
		return nil, nil
	case err != nil:
		log.Warn("router: no usable answer; using the turn rules", "err", err)
		return r.o.Fallback.Plan(ctx, st, t)
	}
	from := "user"
	if !t.FromUser {
		from = t.Sender
		if t.Handoff {
			from += " (hand-off)"
		}
	}
	log = log.With("from", from, "reason", truncate(ans.Reason, 200), "addressed_to", ans.Addressed(), "user_question_pending", ans.UserPending)

	var plan []string
	switch {
	case t.FromUser:
		plan = r.userPlan(st, t, ans)
	case len(st.Owed) > 0:
		for _, k := range r.keys(ans.Plan) {
			if slices.Contains(st.Owed, k) {
				plan = append(plan, k)
			}
		}
	case ans.UserPending:
		log.Info("router: the conversation waits for the user")
		return nil, nil
	default:
		for _, k := range r.keys(ans.Plan) {
			if k != st.LastBot && k != t.Sender && !st.IsBusy(k) {
				plan = []string{k}
				break
			}
		}
	}
	log.Info("router: plan", "plan", plan, "owed", st.Owed)
	return plan, nil
}

// userPlan turns the model's answer to a user message into the bots that
// owe a reply: the model's plan first, then the bots the user named, the
// ones the model says the message addresses, and everyone when it is for
// the whole group. Busy bots are left out.
func (r *Router) userPlan(st turns.State, t turns.Trigger, ans Answer) []string {
	var out []string
	add := func(keys ...string) {
		for _, k := range keys {
			if k != "" && !st.IsBusy(k) && !slices.Contains(out, k) {
				out = append(out, k)
			}
		}
	}
	add(r.keys(ans.Plan)...)
	add(turns.Named(r.cast(), st, t)...)
	add(r.keys(ans.AddressedTo)...)
	if t.Group || ans.All {
		add(r.cast()...)
	}
	return out
}

// keys resolves model-given names to bot keys, dropping unknown names.
func (r *Router) keys(names []string) []string {
	var out []string
	for _, n := range names {
		if k := r.resolve(n); k != "" && !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

func (r *Router) cast() []string {
	var out []string
	for _, p := range r.o.Roster.Bots() {
		out = append(out, p.Key)
	}
	return out
}

// SystemPrompt is the router's fixed instruction, built once per process.
func SystemPrompt(r *roster.Roster, blurbs map[string]string) string {
	user := r.User().Display
	var b strings.Builder
	fmt.Fprintf(&b, "You plan who replies next in a group chat between %s (a human) and AI characters. You never write chat messages yourself.\n\n", user)
	b.WriteString("The characters:\n")
	for _, p := range r.Bots() {
		line := "- " + p.Display
		if bl := strings.TrimSpace(blurbs[p.Key]); bl != "" {
			line += ": " + bl
		}
		b.WriteString(line + "\n")
	}
	x, y := exampleNames(r)
	fmt.Fprintf(&b, `
Answer with one JSON object and nothing else, with the fields in this order:
{"addressed_to": ..., "user_question_pending": true or false, "plan": [...], "reason": "<a few words>"}

1. addressed_to: who the latest message speaks to. Decide this first.
   - a list of character names, when it speaks to particular characters;
   - "all", when it speaks to the whole group or asks for everyone's view ("what do you think?", "你们觉得呢?", "anyone?");
   - "user", when it is written to %[1]s;
   - "none", when it needs no reply (for example "ok", "thanks", "brb").
2. user_question_pending: true only when the MAIN point of the latest message is a direct question to %[1]s, so the conversation cannot go on until %[1]s answers. A question asked in passing, a rhetorical question, or a question tucked inside a longer reply is false. A message from %[1]s is always false.
3. plan: who should reply now, best first.
   - Latest message from %[1]s: everyone who should answer it. If addressed_to is "all", every character with something to say, usually all of them. If %[1]s asks something open without naming anyone, at least one character. If it needs no reply, [].
   - If the request says characters still owe %[1]s a reply, list those characters, in the order that fits the conversation best. You cannot drop them.
   - Otherwise the characters are talking among themselves: at most one character, and only if that character is addressed or clearly has something new to add (a fact, a disagreement, a concrete suggestion). Agreeing, praising or repeating is not new. If user_question_pending is true, or the characters are agreeing or wrapping up, plan is [].
   - Never the character who wrote the latest message, and never a busy character.
4. reason: a few words.

A name or "(to Name)" in a message is a hint about who is meant. Spread the conversation: when a message is open to everyone, prefer characters whose personality or knowledge fits, and favour quiet characters.

Examples:
- Latest from %[1]s: "你们觉得呢?"
  {"addressed_to": "all", "user_question_pending": false, "plan": ["%[3]s", "%[2]s"], "reason": "asks everyone's view"}
- Latest from %[2]s, %[3]s still owes %[1]s a reply: "I'd pick the blue one, it's calmer. %[1]s, did you already buy paint?"
  {"addressed_to": "user", "user_question_pending": false, "plan": ["%[3]s"], "reason": "question in passing; %[3]s still owes a reply"}
- Latest from %[2]s, nobody owes a reply: "Who doesn't love rain, honestly? Anyway, the café is the better spot."
  {"addressed_to": "all", "user_question_pending": false, "plan": [], "reason": "rhetorical; nothing new to add"}
- Latest from %[2]s, nobody owes a reply: "%[1]s, which day works for you, Friday or Saturday?"
  {"addressed_to": "user", "user_question_pending": true, "plan": [], "reason": "waits for %[1]s's answer"}
- Latest from %[1]s: "ok thanks"
  {"addressed_to": "none", "user_question_pending": false, "plan": [], "reason": "needs no reply"}`, user, x, y)
	return b.String()
}

// exampleNames picks two character names for the prompt's examples.
func exampleNames(r *roster.Roster) (string, string) {
	bots := r.Bots()
	switch len(bots) {
	case 0:
		return "Ada", "Cleo"
	case 1:
		return bots[0].Display, bots[0].Display
	}
	return bots[0].Display, bots[1].Display
}

// UserPrompt renders the recent transcript (same <msg from="Name"> format
// the bots see) plus the turn state.
func (r *Router) UserPrompt(st turns.State, t turns.Trigger, entries []transcript.Entry) string {
	user := r.o.Roster.User().Display
	var b strings.Builder
	b.WriteString("Recent messages, oldest first:\n<messages>\n")
	b.WriteString(r.builder.Build("", entries))
	b.WriteString("\n</messages>\n\n")
	if t.FromUser {
		fmt.Fprintf(&b, "The latest message is from %s.\n", user)
	} else if p, ok := r.o.Roster.ByKey(t.Sender); ok && t.Handoff {
		fmt.Fprintf(&b, "%s has taken on the latest request as a task and works on it in the background; the result will be posted later. %s cannot be picked.\n", p.Display, p.Display)
	} else if ok {
		fmt.Fprintf(&b, "The latest message is from %s, who just spoke and cannot be picked.\n", p.Display)
	}
	if owed := r.names(st.Owed); len(owed) > 0 {
		fmt.Fprintf(&b, "Still owe %s a reply to their latest message: %s.\n", user, strings.Join(owed, ", "))
	} else if !t.FromUser {
		fmt.Fprintf(&b, "Nobody owes %s a reply. Characters have replied to each other %d time(s) since %s last wrote; at most %d are allowed.\n",
			user, st.BotTurns, user, r.o.MaxBotTurns)
	}
	b.WriteString(r.participation(entries))
	if busy := r.names(st.Busy); len(busy) > 0 {
		b.WriteString("Busy with a task and cannot be picked: " + strings.Join(busy, ", ") + ".\n")
	}
	b.WriteString(`Answer {"addressed_to": ..., "user_question_pending": ..., "plan": [...], "reason": "..."}.`)
	return b.String()
}

// participation summarises how often each character spoke in the window, so
// the model can see when one character is taking over and who has been quiet.
func (r *Router) participation(entries []transcript.Entry) string {
	count := map[string]int{}
	for _, e := range entries {
		if !silence.Is(e.Body) { // silence markers are not shown above either
			count[e.Sender]++
		}
	}
	var spoke, quiet []string
	for _, p := range r.o.Roster.Bots() {
		if n := count[p.MXID]; n > 0 {
			spoke = append(spoke, fmt.Sprintf("%s %d", p.Display, n))
		} else {
			quiet = append(quiet, p.Display)
		}
	}
	var b strings.Builder
	if len(spoke) > 0 {
		b.WriteString("Messages per character above: " + strings.Join(spoke, ", ") + ".\n")
	}
	if len(quiet) > 0 {
		b.WriteString("Not heard from above: " + strings.Join(quiet, ", ") + ".\n")
	}
	return b.String()
}

// names maps bot keys to display names, in cast order.
func (r *Router) names(keys []string) []string {
	var out []string
	for _, p := range r.o.Roster.Bots() {
		if slices.Contains(keys, p.Key) {
			out = append(out, p.Display)
		}
	}
	return out
}

// CallRecord is one line of the call log.
type CallRecord struct {
	Time       time.Time `json:"time"`
	Room       string    `json:"room"`
	Model      string    `json:"model"`
	FromUser   bool      `json:"from_user"`
	Sender     string    `json:"sender,omitempty"`
	Owed       []string  `json:"owed,omitempty"`
	BotTurns   int       `json:"bot_turns"`
	System     string    `json:"system"`
	Input      string    `json:"input"`
	Output     string    `json:"output,omitempty"`
	Error      string    `json:"error,omitempty"`
	LatencyMS  int64     `json:"latency_ms"`
	Usage      *Usage    `json:"usage,omitempty"`
	ParsedPlan []string  `json:"parsed_plan,omitempty"`
}

// Usage is the token count an endpoint reports.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	Details          *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details,omitempty"`
}

// ask calls the model and parses its answer, logging the call.
func (r *Router) ask(ctx context.Context, st turns.State, t turns.Trigger, entries []transcript.Entry) (Answer, error) {
	input := r.UserPrompt(st, t, entries)
	start := time.Now()
	content, usage, err := r.call(ctx, input)
	rec := CallRecord{
		Time: start.UTC(), Room: t.RoomID, Model: r.o.Model, FromUser: t.FromUser, Sender: t.Sender,
		Owed: st.Owed, BotTurns: st.BotTurns, System: r.system, Input: input, Output: content,
		LatencyMS: time.Since(start).Milliseconds(), Usage: usage,
	}
	var ans Answer
	if err == nil {
		if ans, err = ParseAnswer(content); err != nil {
			err = &badAnswer{err}
		}
		rec.ParsedPlan = ans.Plan
	}
	if err != nil {
		rec.Error = err.Error()
	}
	r.record(rec)
	return ans, err
}

// badAnswer is a call that worked but answered something unusable.
type badAnswer struct{ err error }

func (e *badAnswer) Error() string { return "unusable answer: " + e.err.Error() }
func (e *badAnswer) Unwrap() error { return e.err }

func (r *Router) record(rec CallRecord) {
	if r.o.CallLog == nil {
		return
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	r.logMu.Lock()
	defer r.logMu.Unlock()
	if _, err := r.o.CallLog.Write(append(line, '\n')); err != nil {
		r.o.Log.Warn("router: writing the call log", "err", err)
	}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Temperature    float64         `json:"temperature"`
	Stream         bool            `json:"stream"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

// call returns the model's message content and token usage. Any error here
// (transport, timeout, non-200, no choices) means "fall back to the turn
// rules".
func (r *Router) call(ctx context.Context, user string) (string, *Usage, error) {
	if r.o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.o.Timeout)
		defer cancel()
	}
	req := chatRequest{
		Model:    r.o.Model,
		Messages: []chatMessage{{Role: "system", Content: r.system}, {Role: "user", Content: user}},
	}
	if r.o.JSONMode {
		req.ResponseFormat = &responseFormat{Type: "json_object"}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", nil, err
	}
	url := strings.TrimRight(r.o.BaseURL, "/") + "/chat/completions"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+r.o.APIKey)
	resp, err := r.o.HTTP.Do(hreq)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(raw)), 200))
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", nil, fmt.Errorf("bad response JSON: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", cr.Usage, errors.New("no choices")
	}
	return cr.Choices[0].Message.Content, cr.Usage, nil
}

var (
	thinkRe = regexp.MustCompile(`(?is)<think>.*?</think>`)
	fenceRe = regexp.MustCompile("(?m)^\\s*```[a-zA-Z]*\\s*$")
)

// Answer is a parsed router reply.
type Answer struct {
	AddressedTo []string // character names (All, ToUser and NoReply are the keywords)
	All         bool     // addressed_to "all"
	ToUser      bool     // addressed_to "user"
	NoReply     bool     // addressed_to "none"
	UserPending bool     // user_question_pending
	Plan        []string // character names, best first (not yet validated)
	Reason      string   // for the log only
}

// Addressed is addressed_to as the model gave it: the keywords ("all",
// "user", "none") and the names.
func (a Answer) Addressed() []string {
	var out []string
	for _, kw := range []struct {
		on   bool
		name string
	}{{a.All, "all"}, {a.ToUser, "user"}, {a.NoReply, "none"}} {
		if kw.on {
			out = append(out, kw.name)
		}
	}
	return append(out, a.AddressedTo...)
}

// ParseAnswer extracts the router's answer. It tolerates <think> blocks,
// code fences and prose around the object, and returns an error when no JSON
// object with a "plan" field can be found or a field has the wrong shape.
func ParseAnswer(content string) (Answer, error) {
	s := thinkRe.ReplaceAllString(content, "")
	s = fenceRe.ReplaceAllString(s, "")
	for i := strings.IndexByte(s, '{'); i >= 0; {
		var obj map[string]json.RawMessage
		if json.NewDecoder(strings.NewReader(s[i:])).Decode(&obj) == nil {
			if _, ok := obj["plan"]; ok {
				return parseObject(obj)
			}
		}
		j := strings.IndexByte(s[i+1:], '{')
		if j < 0 {
			break
		}
		i += 1 + j
	}
	return Answer{}, errors.New(`no JSON object with a "plan" field`)
}

func parseObject(obj map[string]json.RawMessage) (Answer, error) {
	var a Answer
	plan, err := nameList(obj["plan"])
	if err != nil {
		return Answer{}, fmt.Errorf(`"plan": %w`, err)
	}
	a.Plan = plan
	if raw, ok := obj["addressed_to"]; ok {
		names, err := nameList(raw)
		if err != nil {
			return Answer{}, fmt.Errorf(`"addressed_to": %w`, err)
		}
		for _, n := range names {
			switch strings.ToLower(n) {
			case "all", "everyone":
				a.All = true
			case "user":
				a.ToUser = true
			case "none", "nobody":
				a.NoReply = true
			default:
				a.AddressedTo = append(a.AddressedTo, n)
			}
		}
	}
	if raw, ok := obj["user_question_pending"]; ok && string(bytes.TrimSpace(raw)) != "null" {
		if err := json.Unmarshal(raw, &a.UserPending); err != nil {
			var s string // "true" / "false" as a string
			if json.Unmarshal(raw, &s) != nil || (s != "true" && s != "false") {
				return Answer{}, fmt.Errorf(`"user_question_pending" is not a boolean: %s`, truncate(string(raw), 50))
			}
			a.UserPending = s == "true"
		}
	}
	_ = json.Unmarshal(obj["reason"], &a.Reason)
	a.Reason = strings.TrimSpace(a.Reason)
	return a, nil
}

// nameList reads a list of names, a single name, or null/empty. Names are
// trimmed of a leading "@"; "", "null", "none" in a list are dropped.
func nameList(raw json.RawMessage) ([]string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		var one string
		if json.Unmarshal(raw, &one) != nil {
			return nil, fmt.Errorf("neither a list of names nor a name: %s", truncate(string(raw), 50))
		}
		list = []string{one}
	}
	var out []string
	for _, n := range list {
		n = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(n), "@"))
		if n == "" || strings.EqualFold(n, "null") {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

// resolve maps a model-given name (bot key or display name, any case) to a
// bot key, or "" if it is not a bot in this room.
func (r *Router) resolve(name string) string {
	for _, p := range r.o.Roster.Bots() {
		if strings.EqualFold(name, p.Key) || strings.EqualFold(name, p.Display) {
			return p.Key
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
