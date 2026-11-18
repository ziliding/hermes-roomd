package memory

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSummaryPrompt(t *testing.T) {
	p := SummaryPrompt(testRoster())
	for _, want := range []string{
		"between Alex (a human) and AI characters (Ada, Cleo)",
		"Only Alex's own words say anything about Alex",
		"never a fact",
		"Do not record terms, nicknames, metaphors or concepts the characters made up",
		"(a term Ada coined)",
		"answer exactly NOTHING",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("summary prompt lacks %q", want)
		}
	}
	u := SummaryUserPrompt(Segment{
		Start:        time.Date(2026, 11, 18, 14, 0, 0, 0, time.UTC),
		End:          time.Date(2026, 11, 18, 14, 40, 0, 0, time.UTC),
		Participants: []string{"Alex", "Ada"},
		Messages:     `<msg from="Alex">hi</msg>`,
	})
	want := "Group chat on 2026-11-18, from 14:00 to 14:40 UTC, between Alex, Ada.\nMessages, oldest first:\n<messages>\n<msg from=\"Alex\">hi</msg>\n</messages>\n\nWrite the memory notes."
	if u != want {
		t.Fatalf("user prompt:\n%s\nwant:\n%s", u, want)
	}
}

func TestCleanSummary(t *testing.T) {
	tests := []struct{ in, want string }{
		{"- Alex said hi.", "- Alex said hi."},
		{"<think>hmm</think>\n- Alex said hi.", "- Alex said hi."},
		{"```\n- Alex said hi.\n```", "- Alex said hi."},
		{"NOTHING", Nothing},
		{"nothing.", Nothing},
		{"**NOTHING**", Nothing},
		{"<think>x</think>  Nothing ", Nothing},
		{"- Nothing was decided, Alex said.", "- Nothing was decided, Alex said."},
	}
	for _, tt := range tests {
		if got := CleanSummary(tt.in); got != tt.want {
			t.Errorf("CleanSummary(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestChatSummarizerRequest(t *testing.T) {
	var body map[string]any
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"<think>x</think>- Alex said hi."}}]}`)
	}))
	defer srv.Close()
	s := &ChatSummarizer{BaseURL: srv.URL + "/v1", APIKey: "k", Model: "cheap", System: "SYS"}
	got, err := s.Summarize(context.Background(), Segment{Participants: []string{"Alex"}, Messages: "m"})
	if err != nil || got != "- Alex said hi." {
		t.Fatalf("got %q err %v", got, err)
	}
	if path != "/v1/chat/completions" || auth != "Bearer k" || body["model"] != "cheap" || body["temperature"] != float64(0) {
		t.Fatalf("request %s %s %v", path, auth, body)
	}
	msgs := body["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != "SYS" || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("messages %v", msgs)
	}
}

func TestChatSummarizerErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		answer string
	}{
		{"500", 500, "boom"},
		{"no choices", 200, `{"choices":[]}`},
		{"empty", 200, `{"choices":[{"message":{"content":"  "}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.answer)
			}))
			defer srv.Close()
			s := &ChatSummarizer{BaseURL: srv.URL, Model: "m"}
			if _, err := s.Summarize(context.Background(), Segment{}); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}
