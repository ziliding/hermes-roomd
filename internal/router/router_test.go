package router

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziliding/hermes-roomd/internal/roster"
	"github.com/ziliding/hermes-roomd/internal/transcript"
	"github.com/ziliding/hermes-roomd/internal/turns"
)

const room = "!r:z"

var testRoster = roster.New("@alex:z", "Alex", []roster.Person{
	{Key: "ada", Display: "Ada", MXID: "@ada:z"},
	{Key: "cleo", Display: "Cleo", MXID: "@cleo:z"},
	{Key: "bram", Display: "Bram", MXID: "@bram:z"},
})

type fakeTranscript struct {
	entries []transcript.Entry
	limit   int
}

func (f *fakeTranscript) Since(_ context.Context, roomID string, after int64, limit int) ([]transcript.Entry, error) {
	f.limit = limit
	if len(f.entries) > limit {
		return f.entries[len(f.entries)-limit:], nil
	}
	return f.entries, nil
}

// recordingFallback is a Decider that records that it was asked.
type recordingFallback struct {
	calls atomic.Int32
	plan  []string
}

func (f *recordingFallback) Plan(context.Context, turns.State, turns.Trigger) ([]string, error) {
	f.calls.Add(1)
	return f.plan, nil
}

type server struct {
	srv      *httptest.Server
	requests atomic.Int32
	lastBody atomic.Value // []byte
}

// newServer answers every chat-completions call with content, or with the
// given status, or hangs until the request is cancelled when delay > 0.
func newServer(t *testing.T, status int, content string, delay time.Duration) *server {
	s := &server{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		body, _ := io.ReadAll(r.Body)
		s.lastBody.Store(body)
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer rk" {
			t.Errorf("unexpected request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		if delay > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(delay):
			}
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			io.WriteString(w, `{"error":{"message":"nope"}}`)
			return
		}
		resp, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}}}})
		w.Write(resp)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func newRouter(s *server, fb turns.Decider, tr *fakeTranscript, timeout time.Duration) *Router {
	return New(Options{
		BaseURL: s.srv.URL + "/v1", APIKey: "rk", Model: "flash", Timeout: timeout, Window: 3, JSONMode: true,
		Transcript: tr, Roster: testRoster,
		Blurbs:      map[string]string{"ada": "a detective", "cleo": "a singer"},
		MaxBotTurns: 2, Fallback: fb,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func entries() *fakeTranscript {
	return &fakeTranscript{entries: []transcript.Entry{
		{Seq: 1, Sender: "@alex:z", Body: "old line"},
		{Seq: 2, Sender: "@alex:z", Body: "@ada:z any plans tonight?"},
		{Seq: 3, Sender: "@ada:z", Body: "A stakeout. Cleo, you coming?"},
		{Seq: 4, Sender: "@cleo:z", Body: "[SILENT]"},
	}}
}

func TestParseAnswer(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Answer
		wantErr bool
	}{
		{"full", `{"addressed_to": ["Cleo","Ada"], "user_question_pending": false, "plan": ["Cleo","Ada"], "reason": " both named "}`,
			Answer{AddressedTo: []string{"Cleo", "Ada"}, Plan: []string{"Cleo", "Ada"}, Reason: "both named"}, false},
		{"all", `{"addressed_to": "all", "user_question_pending": false, "plan": ["Ada"]}`, Answer{All: true, Plan: []string{"Ada"}}, false},
		{"user and pending", `{"addressed_to": "user", "user_question_pending": true, "plan": []}`, Answer{ToUser: true, UserPending: true}, false},
		{"none", `{"addressed_to": "none", "user_question_pending": false, "plan": []}`, Answer{NoReply: true}, false},
		{"plan as a single name", `{"plan": "@Cleo"}`, Answer{Plan: []string{"Cleo"}}, false},
		{"null plan", `{"plan": null}`, Answer{}, false},
		{"pending as a string", `{"user_question_pending": "true", "plan": []}`, Answer{UserPending: true}, false},
		{"code fence", "```json\n{\"plan\": [\"Ada\"]}\n```", Answer{Plan: []string{"Ada"}}, false},
		{"think block with braces", "<think>maybe {\"plan\": [\"Cleo\"]}? no.</think>\n{\"plan\": []}", Answer{}, false},
		{"prose around", `Sure! {"plan": ["Bram"]} is my pick.`, Answer{Plan: []string{"Bram"}}, false},
		{"skips objects without plan", `{"a":1} then {"plan":["Ada"]}`, Answer{Plan: []string{"Ada"}}, false},
		{"old single-speaker shape", `{"next": "Cleo"}`, Answer{}, true},
		{"not JSON", `Cleo should speak`, Answer{}, true},
		{"number plan", `{"plan": 3}`, Answer{}, true},
		{"bad pending", `{"plan": [], "user_question_pending": "maybe"}`, Answer{}, true},
		{"truncated", `{"plan": ["Ut`, Answer{}, true},
		{"empty", ``, Answer{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseAnswer(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseAnswer(%q) err = %v, want err=%v", tt.in, err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ParseAnswer(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestAddressedKeepsKeywords(t *testing.T) {
	a, _ := ParseAnswer(`{"addressed_to": ["all", "Cleo"], "plan": []}`)
	if got := a.Addressed(); !slices.Equal(got, []string{"all", "Cleo"}) {
		t.Fatalf("Addressed() = %v", got)
	}
}

func TestPrompts(t *testing.T) {
	sys := SystemPrompt(testRoster, map[string]string{"ada": "a detective", "cleo": "a singer"})
	for _, want := range []string{"- Ada: a detective\n", "- Cleo: a singer\n", "- Bram\n", `"addressed_to"`, `"user_question_pending"`,
		"MAIN point", "你们觉得呢?", "question in passing", "between Alex (a human)", "You cannot drop them"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt lacks %q:\n%s", want, sys)
		}
	}
	if strings.Index(sys, `"addressed_to"`) > strings.Index(sys, `"plan"`) {
		t.Error("the analysis fields must come before plan")
	}
	r := newRouter(newServer(t, 200, `{"plan":[]}`, 0), &recordingFallback{}, entries(), 0)
	tr := entries()
	got := r.UserPrompt(turns.State{BotTurns: 1, LastBot: "ada"}, turns.Trigger{RoomID: room, Sender: "ada"}, tr.entries[1:])
	want := "Recent messages, oldest first:\n<messages>\n" +
		"<msg from=\"Alex\">(to Ada) any plans tonight?</msg>\n" +
		"<msg from=\"Ada\">A stakeout. Cleo, you coming?</msg>\n" +
		"</messages>\n\n" +
		"The latest message is from Ada, who just spoke and cannot be picked.\n" +
		"Nobody owes Alex a reply. Characters have replied to each other 1 time(s) since Alex last wrote; at most 2 are allowed.\n" +
		"Messages per character above: Ada 1.\n" +
		"Not heard from above: Cleo, Bram.\n" +
		`Answer {"addressed_to": ..., "user_question_pending": ..., "plan": [...], "reason": "..."}.`
	if got != want {
		t.Fatalf("user prompt:\n%s\nwant:\n%s", got, want)
	}
	owed := r.UserPrompt(turns.State{LastBot: "ada", Owed: []string{"bram", "cleo"}}, turns.Trigger{RoomID: room, Sender: "ada"}, tr.entries)
	if !strings.Contains(owed, "Still owe Alex a reply to their latest message: Cleo, Bram.\n") || strings.Contains(owed, "Nobody owes") {
		t.Fatalf("owed line wrong:\n%s", owed)
	}
	handoff := r.UserPrompt(turns.State{LastBot: "ada"}, turns.Trigger{RoomID: room, Sender: "ada", Handoff: true}, tr.entries)
	if !strings.Contains(handoff, "Ada has taken on the latest request as a task") {
		t.Fatalf("hand-off line missing:\n%s", handoff)
	}
}

func TestUserPromptListsBusyBots(t *testing.T) {
	r := newRouter(newServer(t, 200, `{"plan":[]}`, 0), &recordingFallback{}, entries(), 0)
	tr := entries()
	st := turns.State{Busy: []string{"cleo", "ada"}}
	got := r.UserPrompt(st, turns.Trigger{RoomID: room, FromUser: true}, tr.entries)
	if !strings.Contains(got, "Busy with a task and cannot be picked: Ada, Cleo.\n") {
		t.Fatalf("busy line missing:\n%s", got)
	}
	if strings.Contains(r.UserPrompt(turns.State{}, turns.Trigger{RoomID: room, FromUser: true}, tr.entries), "Busy") {
		t.Fatal("busy line shown with nobody busy")
	}
}

func TestPlanAgainstServer(t *testing.T) {
	afterAda := turns.State{BotTurns: 1, LastBot: "ada"}
	adaSpoke := turns.Trigger{RoomID: room, Sender: "ada", Addressees: []string{"cleo"}, Body: "A stakeout. Cleo, you coming."}
	userSpoke := turns.Trigger{RoomID: room, FromUser: true, Body: "anyone around"}
	plan := func(names ...string) string {
		b, _ := json.Marshal(map[string]any{"addressed_to": "all", "user_question_pending": false, "plan": names})
		return string(b)
	}
	tests := []struct {
		name         string
		status       int
		content      string
		delay        time.Duration
		st           turns.State
		trig         turns.Trigger
		want         []string
		wantRequests int32
		wantFallback int32
	}{
		// Continuation (nobody owes the user): at most one bot.
		{"router picks a bot", 200, plan("Cleo"), 0, afterAda, adaSpoke, []string{"cleo"}, 1, 0},
		{"display name in any case", 200, plan("BRAM"), 0, afterAda, adaSpoke, []string{"bram"}, 1, 0},
		{"only the first valid pick continues", 200, plan("Ada", "Dex", "Bram", "Cleo"), 0, afterAda, adaSpoke, []string{"bram"}, 1, 0},
		{"empty plan is honoured even when a bot was named", 200, plan(), 0, afterAda, adaSpoke, nil, 1, 0},
		{"the user is not a bot", 200, plan("Alex"), 0, afterAda, adaSpoke, nil, 1, 0},
		{"last speaker is on cooldown", 200, plan("Ada"), 0, afterAda, adaSpoke, nil, 1, 0},
		{"busy pick means nobody", 200, plan("Cleo"), 0, turns.State{BotTurns: 1, LastBot: "ada", Busy: []string{"cleo"}}, adaSpoke, nil, 1, 0},
		{"a question that waits for the user stops continuation", 200, `{"addressed_to":"user","user_question_pending":true,"plan":["Cleo"]}`, 0, afterAda, adaSpoke, nil, 1, 0},
		{"cap reached: no call, nobody", 200, plan("Cleo"), 0, turns.State{BotTurns: 2, LastBot: "ada"}, adaSpoke, nil, 0, 0},
		{"!continue cap raises the limit", 200, plan("Cleo"), 0, turns.State{BotTurns: 2, LastBot: "ada", Cap: 3}, adaSpoke, []string{"cleo"}, 1, 0},
		{"!continue cap reached: no call, nobody", 200, plan("Cleo"), 0, turns.State{BotTurns: 3, LastBot: "ada", Cap: 3}, adaSpoke, nil, 0, 0},
		{"malformed output between bots means nobody", 200, `I think Cleo.`, 0, afterAda, adaSpoke, nil, 1, 0},
		{"old single-speaker shape between bots means nobody", 200, `{"next": "Cleo"}`, 0, afterAda, adaSpoke, nil, 1, 0},
		{"malformed output for a user message falls back", 200, `I think Cleo.`, 0, turns.State{}, userSpoke, []string{"bram"}, 1, 1},
		{"malformed output while bots are owed falls back", 200, `I think Cleo.`, 0, turns.State{LastBot: "ada", Owed: []string{"cleo", "bram"}}, adaSpoke, []string{"bram"}, 1, 1},
		{"HTTP 500 falls back to the rules", 500, ``, 0, afterAda, adaSpoke, []string{"bram"}, 1, 1},
		{"HTTP 400 (e.g. json mode rejected) falls back", 400, ``, 0, afterAda, adaSpoke, []string{"bram"}, 1, 1},
		{"timeout falls back to the rules", 200, plan("Cleo"), 2 * time.Second, afterAda, adaSpoke, []string{"bram"}, 1, 1},

		// User messages: everyone who owes a reply.
		{"user message, no addressee: router plans", 200, `{"addressed_to":["Ada"],"user_question_pending":false,"plan":["Ada"]}`, 0, turns.State{}, userSpoke, []string{"ada"}, 1, 0},
		{"user message to the group (model): every bot owes, plan order first", 200, `{"addressed_to":"all","user_question_pending":false,"plan":["Cleo"]}`, 0, turns.State{}, userSpoke, []string{"cleo", "ada", "bram"}, 1, 0},
		{"user message to the group (words): every bot owes even if the model under-selects", 200, `{"addressed_to":["Ada"],"user_question_pending":false,"plan":["Ada"]}`, 0, turns.State{},
			turns.Trigger{RoomID: room, FromUser: true, Group: true, Body: "你们觉得呢?"}, []string{"ada", "cleo", "bram"}, 1, 0},
		{"user message needing no reply", 200, `{"addressed_to":"none","user_question_pending":false,"plan":[]}`, 0, turns.State{}, userSpoke, nil, 1, 0},
		{"user names bots: no call, all of them answer", 200, plan(), 0, turns.State{},
			turns.Trigger{RoomID: room, FromUser: true, Addressees: []string{"cleo", "ada"}, Body: "Cleo, Ada?"}, []string{"cleo", "ada"}, 0, 0},
		{"user names a busy bot: router plans the rest", 200, `{"addressed_to":["Bram"],"user_question_pending":false,"plan":["Bram"]}`, 0, turns.State{Busy: []string{"cleo"}},
			turns.Trigger{RoomID: room, FromUser: true, Addressees: []string{"cleo"}, Body: "Cleo?"}, []string{"bram"}, 1, 0},
		{"busy bots never owe", 200, `{"addressed_to":"all","user_question_pending":false,"plan":["Cleo","Ada"]}`, 0, turns.State{Busy: []string{"cleo"}}, userSpoke, []string{"ada", "bram"}, 1, 0},

		// Bots still owe the user: the model only orders them.
		{"one owed bot speaks, no call", 200, plan("Cleo"), 0, turns.State{LastBot: "ada", Owed: []string{"bram"}}, adaSpoke, []string{"bram"}, 0, 0},
		{"several owed: the model orders them", 200, plan("Bram", "Cleo"), 0, turns.State{LastBot: "ada", Owed: []string{"cleo", "bram"}}, adaSpoke, []string{"bram", "cleo"}, 1, 0},
		{"several owed: a question in passing doesn't drop them", 200, `{"addressed_to":"user","user_question_pending":true,"plan":[]}`, 0,
			turns.State{LastBot: "ada", Owed: []string{"cleo", "bram"}}, turns.Trigger{RoomID: room, Sender: "ada", Body: "Blue. Alex, did you buy paint?"}, nil, 1, 0},
		{"owed bots are served past the cap", 200, plan("Bram"), 0, turns.State{BotTurns: 5, LastBot: "ada", Owed: []string{"cleo", "bram"}}, adaSpoke, []string{"bram"}, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newServer(t, tt.status, tt.content, tt.delay)
			fb := &recordingFallback{plan: []string{"bram"}}
			r := newRouter(s, fb, entries(), 200*time.Millisecond)
			got, err := r.Plan(context.Background(), tt.st, tt.trig)
			if err != nil {
				t.Fatalf("router returned an error: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			if n := s.requests.Load(); n != tt.wantRequests {
				t.Errorf("router made %d requests, want %d", n, tt.wantRequests)
			}
			if n := fb.calls.Load(); n != tt.wantFallback {
				t.Errorf("fallback used %d times, want %d", n, tt.wantFallback)
			}
		})
	}
}

func TestRequestShape(t *testing.T) {
	s := newServer(t, 200, `{"plan": []}`, 0)
	tr := entries()
	r := newRouter(s, &recordingFallback{}, tr, time.Second)
	if _, err := r.Plan(context.Background(), turns.State{}, turns.Trigger{RoomID: room, FromUser: true, Body: "hi"}); err != nil {
		t.Fatal(err)
	}
	if tr.limit != 3 {
		t.Errorf("window %d, want 3", tr.limit)
	}
	var req struct {
		Model          string            `json:"model"`
		Temperature    *float64          `json:"temperature"`
		ResponseFormat map[string]string `json:"response_format"`
		Messages       []chatMessage     `json:"messages"`
	}
	if err := json.Unmarshal(s.lastBody.Load().([]byte), &req); err != nil {
		t.Fatal(err)
	}
	if req.Model != "flash" || req.Temperature == nil || *req.Temperature != 0 || req.ResponseFormat["type"] != "json_object" {
		t.Errorf("request %+v", req)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[0].Content != r.system {
		t.Fatalf("messages %+v", req.Messages)
	}
	// Window of 3: the oldest line is cut, and silence markers are dropped.
	user := req.Messages[1].Content
	if strings.Contains(user, "old line") || strings.Contains(user, "SILENT") || !strings.Contains(user, `<msg from="Ada">A stakeout. Cleo, you coming?</msg>`) {
		t.Errorf("user prompt window wrong:\n%s", user)
	}

	// json_mode off: no response_format at all.
	r.o.JSONMode = false
	r.Plan(context.Background(), turns.State{}, turns.Trigger{RoomID: room, FromUser: true, Body: "hi"})
	if strings.Contains(string(s.lastBody.Load().([]byte)), "response_format") {
		t.Error("response_format sent with json_mode off")
	}
}

// Every model call is logged as one JSON line: input, output, model,
// latency and the tokens the endpoint reports.
func TestCallLog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"{\"plan\": [\"Cleo\"]}"}}],`+
			`"usage":{"prompt_tokens":120,"completion_tokens":30,"total_tokens":150,"completion_tokens_details":{"reasoning_tokens":12}}}`)
	}))
	defer srv.Close()
	var buf bytes.Buffer
	r := New(Options{
		BaseURL: srv.URL + "/v1", APIKey: "rk", Model: "flash", Window: 5,
		Transcript: entries(), Roster: testRoster, MaxBotTurns: 2, Fallback: &recordingFallback{}, CallLog: &buf,
	})
	r.Plan(context.Background(), turns.State{}, turns.Trigger{RoomID: room, FromUser: true, Body: "hi"})
	var rec CallRecord
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log line %q: %v", buf.String(), err)
	}
	if rec.Model != "flash" || !rec.FromUser || rec.Room != room || !strings.Contains(rec.Input, "Recent messages") ||
		rec.Output != `{"plan": ["Cleo"]}` || !slices.Equal(rec.ParsedPlan, []string{"Cleo"}) || rec.System == "" {
		t.Fatalf("record %+v", rec)
	}
	if rec.Usage == nil || rec.Usage.TotalTokens != 150 || rec.Usage.Details == nil || rec.Usage.Details.ReasoningTokens != 12 {
		t.Fatalf("usage %+v", rec.Usage)
	}
	if strings.Count(buf.String(), "\n") != 1 {
		t.Fatalf("want exactly one line, got %q", buf.String())
	}
}

// The bot-to-bot cap holds with the router on even if the model always
// picks someone, and the real turns.Rules fallback (router down) also
// respects it.
func TestCapEnforcedWithRouterOn(t *testing.T) {
	bots := []string{"ada", "cleo", "bram"}
	for _, down := range []bool{false, true} {
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			if down {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			body, _ := io.ReadAll(r.Body)
			pick := "Cleo" // always someone other than the latest speaker
			if strings.Contains(string(body), "from Cleo, who just spoke") {
				pick = "Ada"
			}
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"{\"plan\": [\"`+pick+`\"]}"}}]}`)
		}))
		defer srv.Close()
		r := New(Options{
			BaseURL: srv.URL + "/v1", APIKey: "rk", Model: "flash", Window: 5,
			Transcript: entries(), Roster: testRoster, MaxBotTurns: 2,
			Fallback: turns.NewRules(bots, 2, rand.New(rand.NewPCG(1, 1))),
		})
		st := turns.State{LastBot: "ada"} // ada just answered the user
		trig := turns.Trigger{RoomID: room, Sender: "ada", Body: "And another thing."}
		spoke := 0
		for range 10 {
			plan, _ := r.Plan(context.Background(), st, trig)
			if len(plan) == 0 {
				break
			}
			spoke++
			st.BotSpoke(plan[0], true)
			trig = turns.Trigger{RoomID: room, Sender: plan[0], Body: "And another thing."}
		}
		if spoke != 2 || requests.Load() != 2 {
			t.Fatalf("router down=%v: %d bot turns and %d requests, want 2 and 2", down, spoke, requests.Load())
		}
	}
}

// One character answering again and again shows up in the counts the
// router sees, along with who has been quiet.
func TestParticipationCounts(t *testing.T) {
	r := newRouter(newServer(t, 200, `{"plan":[]}`, 0), &recordingFallback{}, entries(), 0)
	var es []transcript.Entry
	for range 3 {
		es = append(es, transcript.Entry{Sender: "@alex:z", Body: "hm"}, transcript.Entry{Sender: "@ada:z", Body: "yes"})
	}
	es = append(es, transcript.Entry{Sender: "@cleo:z", Body: "me too"})
	got := r.participation(es)
	if want := "Messages per character above: Ada 3, Cleo 1.\nNot heard from above: Bram.\n"; got != want {
		t.Fatalf("participation = %q, want %q", got, want)
	}
}
