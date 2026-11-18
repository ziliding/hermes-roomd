package hermes_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ziliding/hermes-roomd/internal/hermes"
	"github.com/ziliding/hermes-roomd/internal/hermes/hermestest"
)

func TestStartRequestShape(t *testing.T) {
	var got struct {
		path, auth, ctype string
		body              map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.auth, got.ctype = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.body)
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, `{"run_id":"run_1","status":"started","replayed":false}`)
	}))
	defer srv.Close()

	for _, tc := range []struct{ name, base, model string }{
		{"profile prefix, no model", srv.URL + "/p/ada/", ""},
		{"explicit model", srv.URL + "/p/ada", "hermes-agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, err := hermes.New(tc.base, "sekrit", tc.model).Start(context.Background(), "grp-abc-ada-g1", "SYSTEM", "<msg from=\"Alex\">hi</msg>")
			if err != nil || id != "run_1" {
				t.Fatalf("id %q err %v", id, err)
			}
			if got.path != "/p/ada/v1/runs" || got.auth != "Bearer sekrit" || got.ctype != "application/json" {
				t.Errorf("path %q auth %q ctype %q", got.path, got.auth, got.ctype)
			}
			if got.body["input"] != "<msg from=\"Alex\">hi</msg>" || got.body["session_id"] != "grp-abc-ada-g1" || got.body["instructions"] != "SYSTEM" {
				t.Errorf("body %v", got.body)
			}
			model, has := got.body["model"]
			if (tc.model == "") == has || (has && model != tc.model) {
				t.Errorf("model %v (has %v)", model, has)
			}
		})
	}
}

func TestStartWithImagesSendsContentParts(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		io.WriteString(w, `{"run_id":"run_1"}`)
	}))
	defer srv.Close()
	img := hermes.Image{Label: "Image from Alex (cat.png):", MIME: "image/png", Data: []byte{1, 2, 3}}
	if _, err := hermes.New(srv.URL, "k", "").Start(context.Background(), "s", "sys", "look", img); err != nil {
		t.Fatal(err)
	}
	want := `[{"content":[{"text":"look","type":"text"},{"text":"Image from Alex (cat.png):","type":"text"},{"image_url":{"url":"data:image/png;base64,AQID"},"type":"image_url"}],"role":"user"}]`
	if got, _ := json.Marshal(body["input"]); string(got) != want {
		t.Fatalf("input %s", got)
	}

	// The fake server reads the same shape back.
	fake := hermestest.New(t)
	id, err := hermes.New(fake.BaseURL("ada"), "k", "").Start(context.Background(), "s", "sys", "look", img)
	if err != nil {
		t.Fatal(err)
	}
	r := fake.Get(id)
	if r.Input != "look\nImage from Alex (cat.png):" || len(r.Images) != 1 || r.Images[0] != "data:image/png;base64,AQID" {
		t.Fatalf("fake saw %+v", r)
	}
}

func TestRunReply(t *testing.T) {
	tests := []struct {
		name       string
		run        hermes.Run
		wantText   string
		wantSilent bool
		wantErr    bool
	}{
		{"normal", hermes.Run{Status: "completed", Output: "  Sure.  "}, "Sure.", false, false},
		{"[SILENT]", hermes.Run{Status: "completed", Output: "[SILENT]"}, "", true, false},
		{"NO_REPLY", hermes.Run{Status: "completed", Output: "NO_REPLY"}, "", true, false},
		{"empty output", hermes.Run{Status: "completed"}, "", true, false},
		{"word silent inside a sentence is speech", hermes.Run{Status: "completed", Output: "I'll stay silent on that."}, "I'll stay silent on that.", false, false},
		{"failed", hermes.Run{Status: "failed", Error: "boom"}, "", false, true},
		{"failed with no output", hermes.Run{Status: "failed"}, "", false, true},
		{"partial turn still posted", hermes.Run{Status: "failed", Output: "Half an ans", Partial: true}, "Half an ans", false, false},
		{"iteration budget: output, no error", hermes.Run{Status: "failed", Output: "Got this far.", TurnExitReason: "max_iterations"}, "Got this far.", false, false},
		{"cancelled", hermes.Run{Status: "cancelled"}, "", false, true},
		{"interrupted", hermes.Run{Status: "interrupted"}, "", false, true},
		{"unknown", hermes.Run{Status: "unknown"}, "", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := tt.run.Reply()
			if tt.wantErr {
				if !errors.Is(err, hermes.ErrNoReply) {
					t.Fatalf("want ErrNoReply, got %v", err)
				}
				return
			}
			if err != nil || r.Text != tt.wantText || r.Silent != tt.wantSilent {
				t.Fatalf("got %+v %v", r, err)
			}
		})
	}
}

func TestStartErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"forbidden", 403, `{"error":{"message":"Session continuation requires API key authentication."}}`},
		{"foreign session", 409, `{"error":{"message":"owned by another surface","code":"permission_denied"}}`},
		{"no run id", 202, `{"status":"started"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			if _, err := hermes.New(srv.URL, "k", "").Start(context.Background(), "s", "sys", "d"); !errors.Is(err, hermes.ErrNoReply) {
				t.Fatalf("want ErrNoReply, got %v", err)
			}
		})
	}
}

func TestStart429Backoff(t *testing.T) {
	tests := []struct {
		name      string
		fails     int
		wantErr   bool
		wantSleep int
	}{
		{"retry-after honoured then success", 2, false, 2},
		{"gives up after max retries", 10, true, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := hermestest.New(t)
			fake.StartStatus = func(n int) int {
				if n <= tt.fails {
					return http.StatusTooManyRequests
				}
				return 0
			}
			var slept []time.Duration
			c := hermes.New(fake.BaseURL("ada"), "k", "")
			c.Sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
			_, err := c.Start(context.Background(), "s", "sys", "d")
			if tt.wantErr != (err != nil) {
				t.Fatalf("err = %v", err)
			}
			if len(slept) != tt.wantSleep {
				t.Fatalf("slept %v, want %d sleeps", slept, tt.wantSleep)
			}
			for _, d := range slept {
				if d != time.Second {
					t.Fatalf("Retry-After not honoured: %v", slept)
				}
			}
		})
	}
}

func TestStatusSteerStop(t *testing.T) {
	fake := hermestest.New(t)
	release := make(chan struct{})
	fake.Handle = func(s *hermestest.Server, r hermestest.Run) {
		<-release
		s.Finish(r.ID, "completed", "done", false) // the agent never drained the steer
	}
	c := hermes.New(fake.BaseURL("ada"), "k", "")
	ctx := context.Background()
	id, err := c.Start(ctx, "s1", "sys", "go")
	if err != nil {
		t.Fatal(err)
	}
	run, err := c.Status(ctx, id)
	if err != nil || run.Status != "running" || run.Terminal() {
		t.Fatalf("status %+v %v", run, err)
	}
	if err := c.Steer(ctx, id, `<msg from="Alex">also blue</msg>`); err != nil {
		t.Fatal(err)
	}
	close(release)
	for !run.Terminal() {
		if run, err = c.Status(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if run.Output != "done" || run.PendingSteer != `<msg from="Alex">also blue</msg>` {
		t.Fatalf("run %+v", run)
	}
	if err := c.Steer(ctx, id, "late"); !errors.Is(err, hermes.ErrSteerRefused) {
		t.Fatalf("steer on a finished run: %v", err)
	}
	if err := c.Stop(ctx, id); err != nil {
		t.Fatalf("stop on a finished run: %v", err)
	}
	if _, err := c.Status(ctx, "run_nope"); !errors.Is(err, hermes.ErrRunNotFound) {
		t.Fatalf("unknown run id: %v", err)
	}
	if err := c.ResolveUnknown(ctx, hermes.Run{ID: id, AdmissionID: "adm", ExecutionGeneration: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestPendingSteerDecodedLeniently(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`null`, ""},
		{`"a\nb"`, "a\nb"},
		{`["a","b"]`, `["a","b"]`},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"run_id":"run_1","status":"completed","output":"x","pending_steer":`+tc.raw+`}`)
		}))
		run, err := hermes.New(srv.URL, "k", "").Status(context.Background(), "run_1")
		srv.Close()
		if err != nil || run.PendingSteer != tc.want {
			t.Errorf("%s: got %q %v", tc.raw, run.PendingSteer, err)
		}
	}
}
