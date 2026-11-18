package router

import (
	"context"
	"io"
	"log/slog"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/ziliding/hermes-roomd/internal/roster"
	"github.com/ziliding/hermes-roomd/internal/turns"
)

// A room's router only knows that room's cast: a bot outside it is never
// picked, and only the cast appears in the prompt.
func TestRouterOnlyPicksFromRoomCast(t *testing.T) {
	lounge := roster.New("@alex:z", "Alex", []roster.Person{
		{Key: "ada", Display: "Ada", MXID: "@ada:z"},
		{Key: "cleo", Display: "Cleo", MXID: "@cleo:z"},
	})
	tests := []struct {
		answer, want string
	}{
		{`{"next":"Cleo"}`, "cleo"},
		{`{"next":"Bram"}`, ""}, // configured globally, but not in this room
	}
	for _, tt := range tests {
		s := newServer(t, 200, tt.answer, 0)
		r := New(Options{
			BaseURL: s.srv.URL + "/v1", APIKey: "rk", Model: "flash", Timeout: time.Second, Window: 3,
			Transcript: entries(), Roster: lounge,
			Blurbs:      map[string]string{"ada": "a detective", "cleo": "a singer", "bram": "a librarian"},
			MaxBotTurns: 2, Fallback: turns.NewRules([]string{"ada", "cleo"}, 2, rand.New(rand.NewPCG(1, 1))),
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		got, ok, err := r.Next(context.Background(), turns.State{}, turns.Trigger{RoomID: room, FromUser: true, Body: "hi all"})
		if err != nil || got != tt.want || ok != (tt.want != "") {
			t.Errorf("answer %s: got %q ok=%v err=%v, want %q", tt.answer, got, ok, err, tt.want)
		}
		if strings.Contains(r.system, "Bram") || strings.Contains(r.system, "librarian") {
			t.Errorf("router prompt mentions a bot outside the room's cast:\n%s", r.system)
		}
	}
}
