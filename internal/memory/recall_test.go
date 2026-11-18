package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ziliding/hermes-roomd/internal/hindsight"
	"github.com/ziliding/hermes-roomd/internal/roster"
	"github.com/ziliding/hermes-roomd/internal/transcript"
)

const room = "!r:z"

func testRoster() *roster.Roster {
	return roster.New("@alex:z", "Alex", []roster.Person{
		{Key: "ada", Display: "Ada", MXID: "@ada:z"},
		{Key: "cleo", Display: "Cleo", MXID: "@cleo:z"},
	})
}

func openStore(t *testing.T) *transcript.Store {
	t.Helper()
	s, err := transcript.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var evSeq int

// add appends a message from sender (an mxid) at ts and returns its seq.
func add(t *testing.T, s *transcript.Store, sender, body string, ts time.Time) int64 {
	t.Helper()
	evSeq++
	seq, err := s.Append(context.Background(), transcript.Entry{
		RoomID: room, EventID: fmt.Sprintf("$e%d", evSeq), Sender: sender, Body: body, TS: ts.UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

// hsServer is a fake Hindsight recall endpoint.
type hsServer struct {
	mu       sync.Mutex
	requests []map[string]any
	status   int
	answer   string
	delay    time.Duration
}

func (h *hsServer) client(t *testing.T) *hindsight.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		h.mu.Lock()
		h.requests = append(h.requests, body)
		status, answer, delay := h.status, h.answer, h.delay
		h.mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		if status == 0 {
			status = 200
		}
		w.WriteHeader(status)
		io.WriteString(w, answer)
	}))
	t.Cleanup(srv.Close)
	return hindsight.New(srv.URL, "roomd-group", "")
}

func (h *hsServer) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.requests)
}

const twoMemories = `{"results":[
 {"id":"m1","text":"Alex said they prefer tea over coffee.","type":"world","occurred_start":"2026-11-12T10:00:00Z","scores":{"final":0.9,"reranker":0.8}},
 {"id":"m2","text":"Ada suggested a <b>trip</b> & more.","type":"observation","scores":{"final":0.5,"reranker":0.3}},
 {"id":"m3","text":"Off-topic weather fact.","type":"world","scores":{"final":0.4,"reranker":0.05}}]}`

func newRecall(t *testing.T, hs *hsServer, s *transcript.Store, timeout time.Duration) *Recall {
	return NewRecall(RecallOptions{
		Client: hs.client(t), Transcript: s, Roster: testRoster(),
		MaxTokens: 400, Budget: hindsight.BudgetLow, Timeout: timeout, MinScore: 0.25,
		Now: func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) },
	})
}

func TestRecallBlockShapeAndFiltering(t *testing.T) {
	s := openStore(t)
	now := time.Now()
	add(t, s, "@alex:z", "what drink do I like? @ada:z", now)
	hs := &hsServer{answer: twoMemories}
	r := newRecall(t, hs, s, time.Second)

	got, ids := r.Block(context.Background(), room, "grp-x-ada-g1")
	if fmt.Sprint(ids) != "[m1 m2]" {
		t.Fatalf("ids %v", ids)
	}
	want := "Recollections from past group chats. They may be wrong or out of date, and Alex's own words in this chat always win:\n" +
		"<memory source=\"past group chats\">\n" +
		"- Alex said they prefer tea over coffee. (2026-11-12)\n" +
		"- Ada suggested a &lt;b&gt;trip&lt;/b&gt; &amp; more.\n" +
		"</memory>"
	if got != want {
		t.Fatalf("block:\n%s\nwant:\n%s", got, want)
	}
	req := hs.requests[0]
	if req["query"] != "Alex: what drink do I like? (to Ada)" {
		t.Fatalf("query %q", req["query"])
	}
	if req["budget"] != "low" || req["max_tokens"] != float64(400) || req["query_timestamp"] != "2026-11-18T12:00:00" {
		t.Fatalf("request %v", req)
	}
	if types := fmt.Sprint(req["types"]); types != "[world experience observation]" {
		t.Fatalf("types %s", types)
	}
}

func TestRecallOncePerUserMessageAndOncePerSession(t *testing.T) {
	s := openStore(t)
	now := time.Now()
	add(t, s, "@alex:z", "tea or coffee?", now)
	hs := &hsServer{answer: twoMemories}
	r := newRecall(t, hs, s, time.Second)
	ctx := context.Background()

	block := func(sid string) string {
		b, ids := r.Block(ctx, room, sid)
		r.Delivered(sid, ids) // the Hermes call succeeded
		return b
	}
	if b, _ := r.Block(ctx, room, "ada-g1"); b == "" {
		t.Fatal("no block")
	}
	// ada's call failed (no Delivered): its retry gets the block again.
	if block("ada-g1") == "" || block("cleo-g1") == "" {
		t.Fatal("every called bot must get the block, and a failed call must not use it up")
	}
	add(t, s, "@ada:z", "tea, obviously", now) // a bot reply in the same chain
	if block("cleo-g1") != "" || block("ada-g1") != "" {
		t.Fatal("a session got the same memories twice")
	}
	if hs.count() != 1 {
		t.Fatalf("recall calls %d, want 1 for one user message", hs.count())
	}
	if block("ada-g2") == "" {
		t.Fatal("a new session generation (after /reset) must get the memories again")
	}
	add(t, s, "@alex:z", "and for breakfast?", now)
	block("ada-g2")
	if hs.count() != 2 {
		t.Fatalf("recall calls %d, want a new recall for a new user message", hs.count())
	}
}

func TestRecallFailsOpen(t *testing.T) {
	tests := []struct {
		name string
		hs   *hsServer
	}{
		{"server error", &hsServer{status: 500, answer: "boom"}},
		{"bank missing", &hsServer{status: 404, answer: `{"detail":"Bank 'roomd-group' not found"}`}},
		{"bad json", &hsServer{answer: "not json"}},
		{"timeout", &hsServer{answer: twoMemories, delay: 2 * time.Second}},
		{"nothing relevant", &hsServer{answer: `{"results":[{"id":"m3","text":"x","scores":{"final":0.1,"reranker":0.1}}]}`}},
		{"empty", &hsServer{answer: `{"results":[]}`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openStore(t)
			add(t, s, "@alex:z", "hi", time.Now())
			r := newRecall(t, tt.hs, s, 100*time.Millisecond)
			start := time.Now()
			if got, _ := r.Block(context.Background(), room, "sid"); got != "" {
				t.Fatalf("block %q, want none", got)
			}
			if took := time.Since(start); took > time.Second {
				t.Fatalf("recall took %v; the timeout must bound it", took)
			}
		})
	}
}

func TestRecallServerDown(t *testing.T) {
	s := openStore(t)
	add(t, s, "@alex:z", "hi", time.Now())
	r := NewRecall(RecallOptions{
		Client: hindsight.New("http://127.0.0.1:1", "roomd-group", ""), Transcript: s, Roster: testRoster(),
		MaxTokens: 100, Budget: hindsight.BudgetLow, Timeout: time.Second,
	})
	if got, _ := r.Block(context.Background(), room, "sid"); got != "" {
		t.Fatalf("block %q with Hindsight down", got)
	}
}

func TestRecallQueryIsBounded(t *testing.T) {
	s := openStore(t)
	for i := range 6 {
		add(t, s, "@alex:z", fmt.Sprintf("message %d %s", i, strings.Repeat("word ", 60)), time.Now())
	}
	add(t, s, "@cleo:z", "[SILENT]", time.Now())
	r := newRecall(t, &hsServer{}, s, time.Second)
	recent, _ := s.Since(context.Background(), room, 0, 6)
	q := r.Query(recent)
	if len(q) > 800 || !strings.HasPrefix(q, "Alex: message ") || !strings.Contains(q, "message 5") {
		t.Fatalf("query (%d chars) should keep whole newest lines: %q", len(q), q)
	}
	if strings.Contains(q, "SILENT") {
		t.Fatal("silence marker in the query")
	}
}
