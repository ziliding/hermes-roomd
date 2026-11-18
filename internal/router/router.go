// Package router is the phase-2 LLM turn router. It implements turns.Decider
// by asking a separate cheap model (any OpenAI-compatible chat-completions
// endpoint) who should speak next, as strict JSON: {"next": "<bot>"} or
// {"next": null}. null, meaning "wait for the user", is the default.
//
// Deterministic code stays in front of the model:
//   - the consecutive bot-turn cap (turns.Rules' MaxBotTurns) is a hard
//     ceiling, checked before any call;
//   - a bot message that asks the user a question ends the chain, with no call;
//   - when the user names a bot, that bot answers, with no call (only the
//     user's words are authoritative, and a null there would leave the user
//     unanswered);
//   - the model's answer is validated: bad JSON, an unknown name, or the bot
//     that just spoke all mean null;
//   - if the call itself fails or times out, the fallback Decider (the MVP
//     turns.Rules) decides instead.
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
	"strings"
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
	Fallback    turns.Decider // used when the router call fails
	Log         *slog.Logger
}

// Router implements turns.Decider.
type Router struct {
	o       Options
	builder *contextbuild.Builder
	system  string // byte-identical on every call (prompt-cache friendly)
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

// Next implements turns.Decider. It never returns an error: a failed router
// call falls back to the fallback Decider, and a bad answer means nobody.
func (r *Router) Next(ctx context.Context, st turns.State, t turns.Trigger) (string, bool, error) {
	log := r.o.Log.With("room", t.RoomID)
	if st.BotTurns >= r.o.MaxBotTurns {
		log.Debug("router: bot-turn cap reached; waiting for the user", "bot_turns", st.BotTurns)
		return "", false, nil
	}
	if t.FromUser && t.Addressee != "" && r.isBot(t.Addressee) && !st.IsBusy(t.Addressee) {
		log.Debug("router: the user named a bot; it answers", "next", t.Addressee)
		return t.Addressee, true, nil
	}
	if !t.FromUser && QuestionToUser(r.o.Roster, t.Body, t.Addressee) {
		log.Info("router: the last message asks the user a question; waiting for the user", "from", t.Sender)
		return "", false, nil
	}

	entries, err := r.o.Transcript.Since(ctx, t.RoomID, 0, r.o.Window)
	if err != nil {
		log.Warn("router: reading transcript failed; using the turn rules", "err", err)
		return r.o.Fallback.Next(ctx, st, t)
	}
	user := r.UserPrompt(st, t, entries)
	content, err := r.call(ctx, user)
	if err != nil {
		log.Warn("router: call failed; using the turn rules", "err", err)
		return r.o.Fallback.Next(ctx, st, t)
	}
	ans, err := ParseAnswer(content)
	if err != nil {
		log.Warn("router: unusable answer; nobody speaks", "err", err, "answer", truncate(content, 200))
		return "", false, nil
	}
	name := ans.Next
	log = log.With("reason", truncate(ans.Reason, 200))
	if name == "" {
		log.Info("router: nobody speaks")
		return "", false, nil
	}
	key := r.resolve(name)
	switch {
	case key == "":
		log.Warn("router: answer names no known character; nobody speaks", "name", name)
		return "", false, nil
	case key == st.LastBot || (!t.FromUser && key == t.Sender):
		log.Info("router: picked the bot that just spoke; nobody speaks", "name", name)
		return "", false, nil
	case st.IsBusy(key):
		log.Info("router: picked a bot that is busy with a task; nobody speaks", "name", name)
		return "", false, nil
	}
	log.Info("router: next speaker", "next", key)
	return key, true, nil
}

// SystemPrompt is the router's fixed instruction, built once per process.
func SystemPrompt(r *roster.Roster, blurbs map[string]string) string {
	user := r.User().Display
	var b strings.Builder
	fmt.Fprintf(&b, "You decide who speaks next in a group chat between %s (a human) and AI characters. You never write chat messages yourself.\n\n", user)
	b.WriteString("The characters:\n")
	for _, p := range r.Bots() {
		line := "- " + p.Display
		if bl := strings.TrimSpace(blurbs[p.Key]); bl != "" {
			line += ": " + bl
		}
		b.WriteString(line + "\n")
	}
	fmt.Fprintf(&b, `
Answer with one JSON object and nothing else: {"next": "<character name>", "reason": "<a few words>"} or {"next": null, "reason": "<a few words>"}.
null means everyone waits for %[1]s. null is the default: if you are unsure, answer null.

Pick a character only if one of these is true:
- the latest message speaks directly to that character, and that character did not write it;
- the latest message asks a question that nobody has answered yet, and that character is the natural one to answer it;
- that character clearly has something new to add: a fact, a disagreement or a concrete suggestion. Agreeing, praising or repeating is not new.

Answer null when:
- the latest message asks %[1]s something, or the conversation is waiting on %[1]s;
- the characters are agreeing, repeating each other or wrapping up;
- the characters have been talking among themselves and %[1]s has not joined in.

If the latest message is from %[1]s and needs a reply, pick the character best suited to answer. Answer null only if it clearly needs no reply (for example "ok", "thanks", "brb").
Spread the conversation: this is a group chat, not a one-on-one. When a message is open to everyone, prefer a character whose personality or knowledge fits the topic, and favour characters who have been quiet over one who has been answering again and again. Keep picking the same character only when %[1]s is clearly talking to that character.
A name or "(to Name)" in a message is a hint about who is meant, not an order. Pick at most one character.`, user)
	return b.String()
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
	} else if p, ok := r.o.Roster.ByKey(t.Sender); ok {
		fmt.Fprintf(&b, "The latest message is from %s, who just spoke and cannot be picked.\n", p.Display)
	}
	fmt.Fprintf(&b, "Characters have replied %d time(s) in a row since %s last wrote; at most %d are allowed.\n",
		st.BotTurns, user, r.o.MaxBotTurns)
	b.WriteString(r.participation(entries))
	if busy := r.busyNames(st); len(busy) > 0 {
		b.WriteString("Busy with a task and cannot be picked: " + strings.Join(busy, ", ") + ".\n")
	}
	b.WriteString(`Who speaks next? Answer {"next": "<character name>", "reason": "<a few words>"} or {"next": null, "reason": "<a few words>"}.`)
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

// busyNames lists the display names of busy bots, in cast order.
func (r *Router) busyNames(st turns.State) []string {
	var out []string
	for _, p := range r.o.Roster.Bots() {
		if st.IsBusy(p.Key) {
			out = append(out, p.Display)
		}
	}
	return out
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
}

// call returns the model's message content. Any error here (transport,
// timeout, non-200, no choices) means "fall back to the turn rules".
func (r *Router) call(ctx context.Context, user string) (string, error) {
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
		return "", err
	}
	url := strings.TrimRight(r.o.BaseURL, "/") + "/chat/completions"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+r.o.APIKey)
	resp, err := r.o.HTTP.Do(hreq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(raw)), 200))
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", fmt.Errorf("bad response JSON: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", errors.New("no choices")
	}
	return cr.Choices[0].Message.Content, nil
}

var (
	thinkRe = regexp.MustCompile(`(?is)<think>.*?</think>`)
	fenceRe = regexp.MustCompile("(?m)^\\s*```[a-zA-Z]*\\s*$")
)

// Answer is a parsed router reply. Next is "" for null.
type Answer struct {
	Next   string
	Reason string // optional, for the log only
}

// ParseAnswer extracts the router's answer. Next is "" for null (or an empty
// / "null" / "none" string). It returns an error when no JSON object with a
// "next" field can be found or "next" is not a string or null. It tolerates
// <think> blocks, code fences and prose around the object.
func ParseAnswer(content string) (Answer, error) {
	s := thinkRe.ReplaceAllString(content, "")
	s = fenceRe.ReplaceAllString(s, "")
	for i := strings.IndexByte(s, '{'); i >= 0; {
		var obj map[string]json.RawMessage
		if json.NewDecoder(strings.NewReader(s[i:])).Decode(&obj) == nil {
			if raw, ok := obj["next"]; ok {
				next, err := nextValue(raw)
				if err != nil {
					return Answer{}, err
				}
				var reason string
				_ = json.Unmarshal(obj["reason"], &reason)
				return Answer{Next: next, Reason: strings.TrimSpace(reason)}, nil
			}
		}
		j := strings.IndexByte(s[i+1:], '{')
		if j < 0 {
			break
		}
		i += 1 + j
	}
	return Answer{}, fmt.Errorf("no JSON object with a \"next\" field")
}

func nextValue(raw json.RawMessage) (string, error) {
	if string(bytes.TrimSpace(raw)) == "null" {
		return "", nil
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil {
		return "", fmt.Errorf("\"next\" is neither a string nor null: %s", truncate(string(raw), 50))
	}
	name = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), "@"))
	switch strings.ToLower(name) {
	case "", "null", "none", "nobody":
		return "", nil
	}
	return name, nil
}

// resolve maps a model-given name (bot key or display name, any case) to a
// bot key, or "" if it is not a bot.
func (r *Router) resolve(name string) string {
	for _, p := range r.o.Roster.Bots() {
		if strings.EqualFold(name, p.Key) || strings.EqualFold(name, p.Display) {
			return p.Key
		}
	}
	return ""
}

func (r *Router) isBot(key string) bool {
	p, ok := r.o.Roster.ByKey(key)
	return ok && p.IsBot
}

// QuestionToUser reports whether a bot message asks the user a question: it
// contains a "?" and either names no other bot, or names the user before the
// first bot it names. In a group chat a question to nobody in particular is
// also waiting on the user, so the chain stops there.
func QuestionToUser(r *roster.Roster, body, addressee string) bool {
	if !strings.Contains(body, "?") {
		return false
	}
	if addressee == "" {
		return true
	}
	userPos := r.NamePos(roster.UserKey, body)
	botPos := r.NamePos(addressee, body)
	return userPos >= 0 && (botPos < 0 || userPos < botPos)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
