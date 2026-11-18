package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const evalSet = `{"id":"group","model":"old","system":"S","input":"I1","expect":{"addressed_to":"all","user_question_pending":false,"plan":["Ada","Cleo"],"first":"Cleo"}}
{"model":"old","system":"S","input":"unlabelled"}
{"id":"wait","model":"old","system":"S","input":"I2","expect":{"user_question_pending":true,"plan":[]}}
`

// fakeEndpoint answers like two models: "good" gets everything right,
// "lazy" picks one bot for the group and never waits for the user.
func fakeEndpoint(t *testing.T, calls *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad request", 400)
			return
		}
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.ResponseFormat["type"] != "json_object" || req.ReasoningEffort != "low" || req.Messages[0].Content != "S" {
			t.Errorf("request %+v", req)
		}
		answers := map[string]map[string]string{
			"good": {
				"I1": `{"addressed_to":"all","user_question_pending":false,"plan":["Cleo","Ada"],"reason":"x"}`,
				"I2": `{"addressed_to":"user","user_question_pending":true,"plan":[],"reason":"x"}`,
			},
			"lazy": {
				"I1": "<think>hm</think>```json\n{\"addressed_to\":[\"Cleo\"],\"user_question_pending\":false,\"plan\":[\"Cleo\"]}\n```",
				"I2": `not json at all`,
			},
		}
		content := answers[req.Model][req.Messages[1].Content]
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content, "reasoning_content": "thinking"}}},
			"usage": map[string]any{"prompt_tokens": 1000, "completion_tokens": 200, "total_tokens": 1200,
				"completion_tokens_details": map[string]any{"reasoning_tokens": 150}},
		})
	}))
}

func TestReplayReport(t *testing.T) {
	var calls atomic.Int32
	srv := fakeEndpoint(t, &calls)
	defer srv.Close()
	dir := t.TempDir()
	set, out := filepath.Join(dir, "set.jsonl"), filepath.Join(dir, "out.jsonl")
	if err := os.WriteFile(set, []byte(evalSet), 0o600); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	err := run(context.Background(), []string{
		"-base-url", srv.URL + "/v1", "-key-env", "K", "-models", "good,lazy", "-reasoning-effort", "low",
		"-price", "good=1/10", "-n", "2", "-out", out, "-v", set,
	}, &b, func(k string) string { return map[string]string{"K": "k"}[k] }, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	report := b.String()
	if calls.Load() != 8 {
		t.Fatalf("%d calls, want 2 cases x 2 runs x 2 models", calls.Load())
	}
	for _, want := range []string{
		"2 labelled case(s) (1 unlabelled line(s) skipped), 2 model(s), 2 run(s) each",
		// good: every field right
		"good  4/4 100%      2/2 100%               4/4 100%  4/4 100%  2/2 100%     4/4 100%",
		// lazy: I2 never parses (every field wrong); I1 has the wrong addressee and plan, the right first
		"lazy  2/4  50%      0/2   0%               2/4  50%  0/4   0%  2/2 100%     0/4   0%",
		// 4 calls of 1000 prompt + 200 completion at $1/$10 per 1M = 4 * 0.003
		"0.0120",
		"WRONG lazy group: addressed_to: got [Cleo], want [all]",
		"WRONG lazy wait: no JSON object",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	for _, line := range strings.Split(report, "\n") {
		f := strings.Fields(line)
		if len(f) == 10 && f[0] == "good" && strings.Join(f[4:], " ") != "1000 200 150 8 0.0120 3.000" {
			t.Errorf("good's token and cost columns %v", f[4:])
		}
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	for sc := bufio.NewScanner(f); sc.Scan(); n++ {
		var r Result
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		if r.Usage == nil || r.Usage.Details.ReasoningTokens != 150 {
			t.Fatalf("result %+v", r)
		}
	}
	if n != 8 {
		t.Fatalf("%d result lines", n)
	}
}

func TestErrors(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(s), 0o600)
		return p
	}
	key := func(string) string { return "k" }
	tests := []struct {
		name string
		args []string
		env  func(string) string
		want string
	}{
		{"no files", nil, key, "no eval files"},
		{"no key", []string{write("a.jsonl", evalSet)}, func(string) string { return "" }, "ROOMD_ROUTER_API_KEY is not set"},
		{"nothing labelled", []string{write("b.jsonl", `{"system":"S","input":"I"}`+"\n")}, key, "no labelled cases"},
		{"bad label", []string{write("c.jsonl", `{"system":"S","input":"I","expect":{"addressed_to":3}}`+"\n")}, key, "c.jsonl:1: expect.addressed_to"},
		{"no input", []string{write("d.jsonl", `{"system":"S","expect":{}}`+"\n")}, key, "logged system and input"},
		{"bad price", []string{"-price", "m=1", write("e.jsonl", evalSet)}, key, "model=IN/OUT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := run(context.Background(), tt.args, io.Discard, tt.env, http.DefaultClient)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err %v, want %q", err, tt.want)
			}
		})
	}
}

// The shipped examples parse and are all labelled.
func TestExamplesParse(t *testing.T) {
	cases, skipped, err := readCases("../../evals/examples.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) < 4 || skipped != 0 {
		t.Fatalf("%d cases, %d unlabelled", len(cases), skipped)
	}
	for _, c := range cases {
		if c.ID == "" || c.Expect.Note == "" {
			t.Errorf("case %s lacks an id or a note", c.Name())
		}
	}
}
