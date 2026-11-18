package router

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
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
	next  string
}

func (f *recordingFallback) Next(context.Context, turns.State, turns.Trigger) (string, bool, error) {
	f.calls.Add(1)
	return f.next, f.next != "", nil
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
		name, in, want string
		wantErr        bool
	}{
		{"name", `{"next": "Cleo"}`, "Cleo", false},
		{"null", `{"next": null}`, "", false},
		{"string null", `{"next": "null"}`, "", false},
		{"empty string", `{"next": ""}`, "", false},
		{"none", `{"next": "None"}`, "", false},
		{"at-name", `{"next": "@cleo"}`, "cleo", false},
		{"code fence", "```json\n{\"next\": \"Ada\"}\n```", "Ada", false},
		{"think block with braces", "<think>maybe {\"next\": \"Cleo\"}? no.</think>\n{\"next\": null}", "", false},
		{"prose around", `Sure! {"next": "Bram"} is my pick.`, "Bram", false},
		{"extra fields", `{"reason": "asked", "next": "Cleo"}`, "Cleo", false},
		{"non-string reason ignored", `{"next": "Cleo", "reason": 7}`, "Cleo", false},
		{"skips objects without next", `{"a":1} then {"next":"Ada"}`, "Ada", false},
		{"not JSON", `Cleo should speak`, "", true},
		{"missing next", `{"speaker": "Cleo"}`, "", true},
		{"number", `{"next": 3}`, "", true},
		{"list", `{"next": ["Cleo","Ada"]}`, "", true},
		{"truncated", `{"next": "Ut`, "", true},
		{"empty", ``, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ans, err := ParseAnswer(tt.in)
			got := ans.Next
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("ParseAnswer(%q) = %q, %v; want %q, err=%v", tt.in, got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestPrompts(t *testing.T) {
	sys := SystemPrompt(testRoster, map[string]string{"ada": "a detective", "cleo": "a singer"})
	for _, want := range []string{"- Ada: a detective\n", "- Cleo: a singer\n", "- Bram\n", `{"next": null`, "null is the default", "between Alex (a human)", "JSON", "favour characters who have been quiet"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt lacks %q:\n%s", want, sys)
		}
	}
	r := newRouter(newServer(t, 200, `{"next":null}`, 0), &recordingFallback{}, entries(), 0)
	tr := entries()
	got := r.UserPrompt(turns.State{BotTurns: 1, LastBot: "ada"}, turns.Trigger{RoomID: room, Sender: "ada"}, tr.entries[1:])
	want := "Recent messages, oldest first:\n<messages>\n" +
		"<msg from=\"Alex\">(to Ada) any plans tonight?</msg>\n" +
		"<msg from=\"Ada\">A stakeout. Cleo, you coming?</msg>\n" +
		"</messages>\n\n" +
		"The latest message is from Ada, who just spoke and cannot be picked.\n" +
		"Characters have replied 1 time(s) in a row since Alex last wrote; at most 2 are allowed.\n" +
		"Messages per character above: Ada 1.\n" +
		"Not heard from above: Cleo, Bram.\n" +
		`Who speaks next? Answer {"next": "<character name>", "reason": "<a few words>"} or {"next": null, "reason": "<a few words>"}.`
	if got != want {
		t.Fatalf("user prompt:\n%s\nwant:\n%s", got, want)
	}
}

func TestUserPromptListsBusyBots(t *testing.T) {
	r := newRouter(newServer(t, 200, `{"next":null}`, 0), &recordingFallback{}, entries(), 0)
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

func TestNextAgainstServer(t *testing.T) {
	afterAda := turns.State{BotTurns: 1, LastBot: "ada"}
	adaSpoke := turns.Trigger{RoomID: room, Sender: "ada", Addressee: "cleo", Body: "A stakeout. Cleo, you coming."}
	userSpoke := turns.Trigger{RoomID: room, FromUser: true, Body: "anyone around"}
	tests := []struct {
		name         string
		status       int
		content      string
		delay        time.Duration
		st           turns.State
		trig         turns.Trigger
		want         string // "" = nobody
		wantRequests int32
		wantFallback int32
	}{
		{"router picks a bot", 200, `{"next": "Cleo"}`, 0, afterAda, adaSpoke, "cleo", 1, 0},
		{"display name in any case", 200, `{"next": "BRAM"}`, 0, afterAda, adaSpoke, "bram", 1, 0},
		{"bot key accepted", 200, `{"next": "bram"}`, 0, afterAda, adaSpoke, "bram", 1, 0},
		{"null is honoured even when a bot was named", 200, `{"next": null}`, 0, afterAda, adaSpoke, "", 1, 0},
		{"unknown bot means null", 200, `{"next": "Dex"}`, 0, afterAda, adaSpoke, "", 1, 0},
		{"the user is not a bot", 200, `{"next": "Alex"}`, 0, afterAda, adaSpoke, "", 1, 0},
		{"last speaker is on cooldown", 200, `{"next": "Ada"}`, 0, afterAda, adaSpoke, "", 1, 0},
		{"malformed output means null", 200, `I think Cleo.`, 0, afterAda, adaSpoke, "", 1, 0},
		{"empty content means null", 200, ``, 0, afterAda, adaSpoke, "", 1, 0},
		{"user message, no addressee: router picks", 200, `{"next": "Ada"}`, 0, turns.State{}, userSpoke, "ada", 1, 0},
		{"HTTP 500 falls back to the rules", 500, ``, 0, afterAda, adaSpoke, "bram", 1, 1},
		{"HTTP 400 (e.g. json mode rejected) falls back", 400, ``, 0, afterAda, adaSpoke, "bram", 1, 1},
		{"timeout falls back to the rules", 200, `{"next": "Cleo"}`, 2 * time.Second, afterAda, adaSpoke, "bram", 1, 1},
		{"cap reached: no call, nobody", 200, `{"next": "Cleo"}`, 0, turns.State{BotTurns: 2, LastBot: "ada"}, adaSpoke, "", 0, 0},
		{"user names a bot: no call, it answers", 200, `{"next": null}`, 0, turns.State{}, turns.Trigger{RoomID: room, FromUser: true, Addressee: "cleo", Body: "Cleo?"}, "cleo", 0, 0},
		{"bot asks the room a question: no call, nobody", 200, `{"next": "Cleo"}`, 0, afterAda, turns.Trigger{RoomID: room, Sender: "ada", Body: "What do you all think?"}, "", 0, 0},
		{"bot asks the user by name: no call, nobody", 200, `{"next": "Cleo"}`, 0, afterAda, turns.Trigger{RoomID: room, Sender: "ada", Addressee: "cleo", Body: "Alex, should Cleo come too?"}, "", 0, 0},
		{"bot asks another bot: router decides", 200, `{"next": "Cleo"}`, 0, afterAda, turns.Trigger{RoomID: room, Sender: "ada", Addressee: "cleo", Body: "Cleo, you coming?"}, "cleo", 1, 0},
		{"busy pick means nobody", 200, `{"next": "Cleo"}`, 0, turns.State{BotTurns: 1, LastBot: "ada", Busy: []string{"cleo"}}, adaSpoke, "", 1, 0},
		{"user names a busy bot: no shortcut, router decides", 200, `{"next": "Bram"}`, 0, turns.State{Busy: []string{"cleo"}}, turns.Trigger{RoomID: room, FromUser: true, Addressee: "cleo", Body: "Cleo?"}, "bram", 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newServer(t, tt.status, tt.content, tt.delay)
			fb := &recordingFallback{next: "bram"}
			r := newRouter(s, fb, entries(), 200*time.Millisecond)
			got, ok, err := r.Next(context.Background(), tt.st, tt.trig)
			if err != nil {
				t.Fatalf("router returned an error: %v", err)
			}
			if (tt.want == "") == ok || got != tt.want {
				t.Fatalf("got %q ok=%v, want %q", got, ok, tt.want)
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
	s := newServer(t, 200, `{"next": null}`, 0)
	tr := entries()
	r := newRouter(s, &recordingFallback{}, tr, time.Second)
	if _, _, err := r.Next(context.Background(), turns.State{}, turns.Trigger{RoomID: room, FromUser: true, Body: "hi"}); err != nil {
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
	r.Next(context.Background(), turns.State{}, turns.Trigger{RoomID: room, FromUser: true, Body: "hi"})
	if strings.Contains(string(s.lastBody.Load().([]byte)), "response_format") {
		t.Error("response_format sent with json_mode off")
	}
}

// The cap holds with the router on even if the model always picks someone,
// and the real turns.Rules fallback (router down) also respects it.
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
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"{\"next\": \"`+pick+`\"}"}}]}`)
		}))
		defer srv.Close()
		r := New(Options{
			BaseURL: srv.URL + "/v1", APIKey: "rk", Model: "flash", Window: 5,
			Transcript: entries(), Roster: testRoster, MaxBotTurns: 2,
			Fallback: turns.NewRules(bots, 2, rand.New(rand.NewPCG(1, 1))),
		})
		st := turns.State{}
		trig := turns.Trigger{RoomID: room, FromUser: true, Body: "go on"}
		spoke := 0
		for range 10 {
			next, ok, _ := r.Next(context.Background(), st, trig)
			if !ok {
				break
			}
			spoke++
			st.BotSpoke(next)
			trig = turns.Trigger{RoomID: room, Sender: next, Body: "And another thing."}
		}
		if spoke != 2 || requests.Load() != 2 {
			t.Fatalf("router down=%v: %d bot turns and %d requests, want 2 and 2", down, spoke, requests.Load())
		}
	}
}

func TestParseAnswerReason(t *testing.T) {
	ans, err := ParseAnswer(`{"next": null, "reason": " Alex was asked "}`)
	if err != nil || ans.Next != "" || ans.Reason != "Alex was asked" {
		t.Fatalf("got %+v, %v", ans, err)
	}
}

// One character answering again and again shows up in the counts the
// router sees, along with who has been quiet.
func TestParticipationCounts(t *testing.T) {
	r := newRouter(newServer(t, 200, `{"next":null}`, 0), &recordingFallback{}, entries(), 0)
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

func TestQuestionToUser(t *testing.T) {
	tests := []struct {
		body, addressee string
		want            bool
	}{
		{"Nice weather.", "", false},
		{"What do you think?", "", true},
		{"Alex, what do you think?", "", true},
		{"Cleo, what do you think?", "cleo", false},
		{"Alex, should Cleo come?", "cleo", true},
		{"Cleo, should we ask Alex?", "cleo", false},
		{"Cleo is right. Thoughts?", "cleo", false},
	}
	for _, tt := range tests {
		if got := QuestionToUser(testRoster, tt.body, tt.addressee); got != tt.want {
			t.Errorf("QuestionToUser(%q, %q) = %v, want %v", tt.body, tt.addressee, got, tt.want)
		}
	}
}
