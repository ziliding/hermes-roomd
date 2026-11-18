package hermes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func reply(content, finish string) string {
	b, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-x", "object": "chat.completion",
		"choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": content}, "finish_reason": finish}},
	})
	return string(b)
}

func TestCompleteRequestShape(t *testing.T) {
	var got struct {
		path, auth, session, ctype string
		body                       map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.auth = r.URL.Path, r.Header.Get("Authorization")
		got.session, got.ctype = r.Header.Get("X-Hermes-Session-Id"), r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.body)
		io.WriteString(w, reply("  Hello, Alex.  ", "stop"))
	}))
	defer srv.Close()

	for _, tc := range []struct{ name, base, model string }{
		{"profile prefix, no model", srv.URL + "/p/ada-group/", ""},
		{"explicit model", srv.URL + "/p/ada-group", "hermes-agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(tc.base, "sekrit", tc.model, time.Second)
			r, err := c.Complete(context.Background(), "grp-abc-ada-g1", "SYSTEM", "[Alex] hi")
			if err != nil {
				t.Fatal(err)
			}
			if r.Text != "Hello, Alex." || r.Silent {
				t.Fatalf("reply = %+v", r)
			}
			if got.path != "/p/ada-group/v1/chat/completions" {
				t.Errorf("path %q", got.path)
			}
			if got.auth != "Bearer sekrit" {
				t.Errorf("auth %q", got.auth)
			}
			if got.session != "grp-abc-ada-g1" {
				t.Errorf("session header %q", got.session)
			}
			if got.ctype != "application/json" {
				t.Errorf("content-type %q", got.ctype)
			}
			msgs, _ := got.body["messages"].([]any)
			if len(msgs) != 2 {
				t.Fatalf("want exactly [system, user], got %v", got.body["messages"])
			}
			want := []map[string]any{{"role": "system", "content": "SYSTEM"}, {"role": "user", "content": "[Alex] hi"}}
			for i, m := range msgs {
				mm := m.(map[string]any)
				if mm["role"] != want[i]["role"] || mm["content"] != want[i]["content"] {
					t.Errorf("message %d = %v, want %v", i, mm, want[i])
				}
			}
			if got.body["stream"] != false {
				t.Errorf("stream = %v", got.body["stream"])
			}
			model, has := got.body["model"]
			if tc.model == "" && has {
				t.Errorf("model should be omitted, got %v", model)
			}
			if tc.model != "" && model != tc.model {
				t.Errorf("model = %v", model)
			}
		})
	}
}

func TestCompleteOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantText   string
		wantSilent bool
		wantErr    bool
	}{
		{"normal", 200, reply("Sure.", "stop"), "Sure.", false, false},
		{"[SILENT]", 200, reply("[SILENT]", "stop"), "", true, false},
		{"SILENT bare", 200, reply("SILENT", "stop"), "", true, false},
		{"NO_REPLY", 200, reply("NO_REPLY", "stop"), "", true, false},
		{"no reply spaced lower", 200, reply("no reply.", "stop"), "", true, false},
		{"narrated", 200, reply("(silent)", "stop"), "", true, false},
		{"stays silent narration", 200, reply("*stays silent*", "stop"), "", true, false},
		{"mute emoji", 200, reply("🔇", "stop"), "", true, false},
		{"dot", 200, reply(".", "stop"), "", true, false},
		{"ellipsis", 200, reply("…", "stop"), "", true, false},
		{"muted empty content", 200, reply("", "stop"), "", true, false},
		{"word silent inside sentence is speech", 200, reply("I'll stay silent on that, but Cleo knows.", "stop"), "I'll stay silent on that, but Cleo knows.", false, false},
		{"emoji-only reply is speech", 200, reply("👍", "stop"), "👍", false, false},
		{"question mark is speech", 200, reply("?", "stop"), "?", false, false},
		{"soft partial still posted", 200, reply("Half an ans", "length"), "Half an ans", false, false},
		{"finish error", 200, reply("Agent failed", "error"), "", false, true},
		{"finish cancelled", 200, reply("", "cancelled"), "", false, true},
		{"hard failure 502", 502, `{"error":{"message":"Agent run did not produce a response.","code":"agent_incomplete"}}`, "", false, true},
		{"forbidden", 403, `{"error":{"message":"Session continuation requires API key authentication."}}`, "", false, true},
		{"no choices", 200, `{"choices":[]}`, "", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			r, err := New(srv.URL, "k", "", time.Second).Complete(context.Background(), "s", "sys", "d")
			if tt.wantErr {
				if !errors.Is(err, ErrNoReply) {
					t.Fatalf("want ErrNoReply, got %v (reply %+v)", err, r)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.Text != tt.wantText || r.Silent != tt.wantSilent {
				t.Fatalf("got %+v, want text=%q silent=%v", r, tt.wantText, tt.wantSilent)
			}
		})
	}
}

func TestComplete429Backoff(t *testing.T) {
	tests := []struct {
		name      string
		fails     int
		wantErr   bool
		wantSleep []time.Duration
	}{
		{"retry-after honoured then success", 2, false, []time.Duration{7 * time.Second, 7 * time.Second}},
		{"gives up after max retries", 10, true, []time.Duration{7 * time.Second, 7 * time.Second, 7 * time.Second}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls <= tt.fails {
					w.Header().Set("Retry-After", "7")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				io.WriteString(w, reply("ok", "stop"))
			}))
			defer srv.Close()
			var slept []time.Duration
			c := New(srv.URL, "k", "", time.Second)
			c.Sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
			r, err := c.Complete(context.Background(), "s", "sys", "d")
			if tt.wantErr != (err != nil) {
				t.Fatalf("err = %v", err)
			}
			if !tt.wantErr && r.Text != "ok" {
				t.Fatalf("reply %+v", r)
			}
			if len(slept) != len(tt.wantSleep) {
				t.Fatalf("slept %v, want %v", slept, tt.wantSleep)
			}
			for i := range slept {
				if slept[i] != tt.wantSleep[i] {
					t.Fatalf("slept %v, want %v", slept, tt.wantSleep)
				}
			}
		})
	}
}

func TestBackoffDefaultAndCap(t *testing.T) {
	c := &Client{MaxBackoff: 5 * time.Second}
	if d := c.backoff("", 0); d != 2*time.Second {
		t.Errorf("attempt 0 = %v", d)
	}
	if d := c.backoff("", 4); d != 5*time.Second {
		t.Errorf("capped = %v", d)
	}
	if d := c.backoff("3600", 0); d != 5*time.Second {
		t.Errorf("retry-after capped = %v", d)
	}
}
