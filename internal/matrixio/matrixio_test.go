package matrixio

import (
	"context"
	"encoding/json"
	"io"
	"maunium.net/go/mautrix/event"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"maunium.net/go/mautrix"
)

func TestCheckRooms(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string // "" = ok
	}{
		{"unencrypted (no state event)", 404, `{"errcode":"M_NOT_FOUND","error":"not found"}`, ""},
		{"encrypted", 200, `{"algorithm":"m.megolm.v1.aes-sha2"}`, "end-to-end encrypted"},
		{"listener not joined", 403, `{"errcode":"M_FORBIDDEN","error":"not in room"}`, "not joined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var path string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path = r.URL.EscapedPath()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			cli, err := mautrix.NewClient(srv.URL, "@ada:z", "tok")
			if err != nil {
				t.Fatal(err)
			}
			err = CheckRooms(context.Background(), cli, []string{"!K2xh2cckIpNY0GU1mLjjGKFzK9MS1veWcGbeKysPcoU"})
			if !strings.Contains(path, "/state/m.room.encryption") {
				t.Errorf("unexpected request path %q", path)
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestSendNoticeIsTaggedAndEchoIsMarkedControl(t *testing.T) {
	var sent []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"event_id":"$n1"}`)
	}))
	defer srv.Close()
	cli, err := mautrix.NewClient(srv.URL, "@ada:z", "tok")
	if err != nil {
		t.Fatal(err)
	}
	s := &Sender{cli: cli, msgType: event.MsgText}
	if id, err := s.SendNotice(context.Background(), "!r:z", "Reset done"); err != nil || id != "$n1" {
		t.Fatalf("SendNotice = %q, %v", id, err)
	}
	// Feed the sent content back as a synced event, as the listener would see it.
	evt := &event.Event{Type: event.EventMessage, RoomID: "!r:z", ID: "$n1", Sender: "@ada:z"}
	if err := json.Unmarshal(sent, &evt.Content); err != nil {
		t.Fatal(err)
	}
	if err := evt.Content.ParseRaw(evt.Type); err != nil {
		t.Fatal(err)
	}
	m, ok := toMessage(evt)
	if !ok || !m.Control || m.Body != "Reset done" {
		t.Fatalf("echo = %+v ok=%v (sent %s)", m, ok, sent)
	}
	if !strings.Contains(string(sent), `"msgtype":"m.notice"`) {
		t.Errorf("notice msgtype missing: %s", sent)
	}

	// An ordinary bot message is not control.
	plain := &event.Event{Type: event.EventMessage, RoomID: "!r:z", ID: "$m", Sender: "@ada:z"}
	json.Unmarshal([]byte(`{"msgtype":"m.notice","body":"hi"}`), &plain.Content)
	plain.Content.ParseRaw(plain.Type)
	if m, ok := toMessage(plain); !ok || m.Control {
		t.Fatalf("plain notice = %+v ok=%v", m, ok)
	}
}
