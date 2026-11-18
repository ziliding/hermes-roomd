package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ziliding/hermes-roomd/internal/config"
	"github.com/ziliding/hermes-roomd/internal/hermes"
	"github.com/ziliding/hermes-roomd/internal/media"
	"github.com/ziliding/hermes-roomd/internal/memory"
	"github.com/ziliding/hermes-roomd/internal/room"
	"github.com/ziliding/hermes-roomd/internal/transcript"
)

type nopPoster struct{}

func (nopPoster) Send(context.Context, string, string) (string, error)       { return "$x", nil }
func (nopPoster) SendNotice(context.Context, string, string) (string, error) { return "$n", nil }
func (nopPoster) SendReply(context.Context, string, string, string) (string, error) {
	return "$r", nil
}
func (nopPoster) SendImage(context.Context, string, media.Image, string) (string, error) {
	return "$i", nil
}
func (nopPoster) React(context.Context, string, string, string) error       { return nil }
func (nopPoster) Typing(context.Context, string, bool, time.Duration) error { return nil }

const cfgYAML = `
homeserver: http://127.0.0.1:8008
user: "@alex:z"
bots:
  ada:   { display: Ada,   mxid: "@ada:z",   access_token_env: T, hermes: { base_url: "http://h/p/ada",   key_env: T } }
  cleo:    { display: Cleo,    mxid: "@cleo:z",    access_token_env: T, hermes: { base_url: "http://h/p/cleo",    key_env: T } }
  bram: { display: Bram, mxid: "@bram:z", access_token_env: T, hermes: { base_url: "http://h/p/bram", key_env: T } }
rooms:
  - id: "!a:z"
    bots: [ada, cleo]
    listener: cleo
    max_bot_turns: 3
    memory: { bank_id: roomd-a }
  - id: "!b:z"
    bots: [bram, ada]
    memory: { bank_id: roomd-b }
memory:
  enabled: true
  api_url: http://placeholder
  retain: { mode: transcript, min_messages: 2 }
`

// Each room gets its own cast, rules, listener-notifier and bank: recall in
// room A only queries roomd-a, and room B's conversation is written to roomd-b.
func TestBuildRoomsPerRoomCastAndBank(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/recall"):
			io.WriteString(w, `{"results":[{"id":"m1","text":"Alex likes tea.","scores":{"final":0.9,"reranker":0.9}}]}`)
		case r.URL.Path == "/v1/default/banks":
			io.WriteString(w, `{"banks":[]}`)
		case r.Method == "PUT":
			io.WriteString(w, `{}`)
		default:
			io.WriteString(w, `{"success":true,"bank_id":"x","items_count":1,"async":false}`)
		}
	}))
	defer srv.Close()

	cfg, err := config.Parse([]byte(cfgYAML), func(string) (string, bool) { return "tok", true })
	if err != nil {
		t.Fatal(err)
	}
	cfg.Memory.APIURL = srv.URL
	cfg.Rooms[0].Rules, cfg.Rooms[1].Rules = "A rules for {{name}} with {{others}}", "B rules for {{name}} with {{others}}"
	store, err := transcript.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	posters, runners := map[string]room.Poster{}, map[string]room.Runner{}
	for _, k := range cfg.BotKeys() {
		posters[k], runners[k] = nopPoster{}, hermes.New("http://h/p/"+k, "k", "")
	}
	specs, retainers := buildRooms(cfg, store, posters, runners, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if len(specs) != 2 || len(retainers) != 2 {
		t.Fatalf("specs %d retainers %d", len(specs), len(retainers))
	}
	a, b := specs[0], specs[1]
	var aBots, bBots []string
	for _, x := range a.Bots {
		aBots = append(aBots, x.Key)
	}
	for _, x := range b.Bots {
		bBots = append(bBots, x.Key)
	}
	if !reflect.DeepEqual(aBots, []string{"ada", "cleo"}) || !reflect.DeepEqual(bBots, []string{"ada", "bram"}) {
		t.Fatalf("casts %v / %v", aBots, bBots)
	}
	if a.BackgroundAfter != config.DefaultBackgroundAfter || b.BackgroundAfter != config.DefaultBackgroundAfter {
		t.Fatalf("background_after %v / %v", a.BackgroundAfter, b.BackgroundAfter)
	}
	if a.Notifier != "cleo" || b.Notifier != "ada" {
		t.Fatalf("notifiers %q / %q", a.Notifier, b.Notifier)
	}
	if got := a.Bots[0].System; got != "A rules for Ada with - Cleo" {
		t.Fatalf("room A system for ada %q (room rules, room cast only)", got)
	}
	if got := b.Bots[0].System; got != "B rules for Ada with - Bram" {
		t.Fatalf("room B system for ada %q", got)
	}

	ctx := context.Background()
	old := time.Now().Add(-2 * time.Hour).UnixMilli()
	retainersRunOnce(ctx, retainers) // first pass on empty rooms: cursors start at 0
	for i, body := range []string{"I like tea", "noted"} {
		sender := []string{"@alex:z", "@bram:z"}[i]
		if _, err := store.Append(ctx, transcript.Entry{RoomID: "!b:z", EventID: "$b" + body, Sender: sender, Body: body, TS: old}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Append(ctx, transcript.Entry{RoomID: "!a:z", EventID: "$a1", Sender: "@alex:z", Body: "tea?", TS: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if block, _ := a.Memory.Block(ctx, "!a:z", "sid"); !strings.Contains(block, "Alex likes tea.") {
		t.Fatalf("room A block %q", block)
	}
	retainersRunOnce(ctx, retainers)

	got := strings.Join(paths, "\n")
	for _, want := range []string{
		"POST /v1/default/banks/roomd-a/memories/recall",
		"PUT /v1/default/banks/roomd-b",
		"POST /v1/default/banks/roomd-b/memories",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing request %q in:\n%s", want, got)
		}
	}
	for _, bad := range []string{"roomd-group", "roomd-b/memories/recall", "PUT /v1/default/banks/roomd-a"} {
		if strings.Contains(got, bad) {
			t.Errorf("unexpected request with %q in:\n%s", bad, got)
		}
	}
}

func retainersRunOnce(ctx context.Context, rs []*memory.Retainer) {
	for _, r := range rs {
		r.RunOnce(ctx)
	}
}

func (nopPoster) Download(context.Context, string, int64) ([]byte, error) { return nil, nil }
