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
	"time"

	"github.com/ziliding/hermes-roomd/internal/media"
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

func TestReplyReactionTypingWireShapes(t *testing.T) {
	type req struct {
		method, path string
		body         map[string]any
	}
	var got []req
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		got = append(got, req{r.Method, r.URL.EscapedPath(), body})
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"event_id":"$e"}`)
	}))
	defer srv.Close()
	cli, err := mautrix.NewClient(srv.URL, "@ada:z", "tok")
	if err != nil {
		t.Fatal(err)
	}
	s := &Sender{cli: cli, msgType: event.MsgText}
	ctx := context.Background()
	if _, err := s.SendReply(ctx, "!r:z", "here it is", "$req"); err != nil {
		t.Fatal(err)
	}
	if err := s.React(ctx, "!r:z", "$req", "✓"); err != nil {
		t.Fatal(err)
	}
	if err := s.Typing(ctx, "!r:z", true, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("requests %+v", got)
	}
	rel, _ := got[0].body["m.relates_to"].(map[string]any)
	irt, _ := rel["m.in_reply_to"].(map[string]any)
	if !strings.Contains(got[0].path, "/send/m.room.message/") || irt["event_id"] != "$req" || got[0].body["body"] != "here it is" {
		t.Errorf("reply %+v", got[0])
	}
	if m, ok := got[0].body["m.mentions"].(map[string]any); !ok || len(m) != 0 {
		t.Errorf("reply must ping nobody: %v", got[0].body["m.mentions"])
	}
	ann, _ := got[1].body["m.relates_to"].(map[string]any)
	if !strings.Contains(got[1].path, "/send/m.reaction/") || ann["rel_type"] != "m.annotation" || ann["event_id"] != "$req" || ann["key"] != "✓" {
		t.Errorf("reaction %+v", got[1])
	}
	if got[2].method != http.MethodPut || !strings.Contains(got[2].path, "/typing/") || got[2].body["typing"] != true || got[2].body["timeout"] != float64(30000) {
		t.Errorf("typing %+v", got[2])
	}
}

func TestSendImageUploadsThenPostsMImage(t *testing.T) {
	var uploadPath, uploadType, uploadQuery string
	var uploaded []byte
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/media/") {
			uploadPath, uploadType, uploadQuery, uploaded = r.URL.Path, r.Header.Get("Content-Type"), r.URL.RawQuery, raw
			io.WriteString(w, `{"content_uri":"mxc://example.com/abc"}`)
			return
		}
		_ = json.Unmarshal(raw, &sent)
		io.WriteString(w, `{"event_id":"$img"}`)
	}))
	defer srv.Close()
	cli, err := mautrix.NewClient(srv.URL, "@ada:z", "tok")
	if err != nil {
		t.Fatal(err)
	}
	s := &Sender{cli: cli, msgType: event.MsgText}
	img := media.Image{Alt: "a cat", MIME: "image/png", Data: []byte("PNGDATA"), Width: 3, Height: 2}
	evID, err := s.SendImage(context.Background(), "!r:z", img, "$req")
	if err != nil || evID != "$img" {
		t.Fatalf("event %q err %v", evID, err)
	}
	if !strings.HasSuffix(uploadPath, "/_matrix/media/v3/upload") || uploadType != "image/png" || string(uploaded) != "PNGDATA" || !strings.Contains(uploadQuery, "filename=image.png") {
		t.Errorf("upload %q %q %q %q", uploadPath, uploadType, uploadQuery, uploaded)
	}
	info, _ := sent["info"].(map[string]any)
	rel, _ := sent["m.relates_to"].(map[string]any)
	irt, _ := rel["m.in_reply_to"].(map[string]any)
	if sent["msgtype"] != "m.image" || sent["url"] != "mxc://example.com/abc" || sent["body"] != "a cat" ||
		info["mimetype"] != "image/png" || info["size"] != float64(7) || info["w"] != float64(3) || info["h"] != float64(2) || irt["event_id"] != "$req" {
		t.Errorf("m.image content %v", sent)
	}

	// Without a reply target there is no relation.
	sent = nil
	if _, err := s.SendImage(context.Background(), "!r:z", img, ""); err != nil {
		t.Fatal(err)
	}
	if _, has := sent["m.relates_to"]; has {
		t.Errorf("unexpected relation %v", sent["m.relates_to"])
	}
}

func TestImageMessages(t *testing.T) {
	tests := []struct {
		name, content string
		ok            bool
		want          Message
	}{
		{"image without caption", `{"msgtype":"m.image","body":"cat.jpg","url":"mxc://example.com/a","info":{"mimetype":"image/jpeg","size":1234}}`,
			true, Message{Image: &ImageRef{URL: "mxc://example.com/a", MIME: "image/jpeg", Name: "cat.jpg", Size: 1234}}},
		{"image with caption", `{"msgtype":"m.image","body":"Ada, look at this","filename":"cat.jpg","url":"mxc://example.com/a","m.mentions":{"user_ids":["@ada:z"]}}`,
			true, Message{Body: "Ada, look at this", Mentions: []string{"@ada:z"}, Image: &ImageRef{URL: "mxc://example.com/a", Name: "cat.jpg"}}},
		{"encrypted image is dropped", `{"msgtype":"m.image","body":"cat.jpg","file":{"url":"mxc://example.com/a"}}`, false, Message{}},
		{"video is dropped", `{"msgtype":"m.video","body":"v.mp4","url":"mxc://example.com/v"}`, false, Message{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evt := &event.Event{Type: event.EventMessage, RoomID: "!r:z", ID: "$i", Sender: "@alex:z"}
			if err := json.Unmarshal([]byte(tt.content), &evt.Content); err != nil {
				t.Fatal(err)
			}
			if err := evt.Content.ParseRaw(evt.Type); err != nil {
				t.Fatal(err)
			}
			m, ok := toMessage(evt)
			if ok != tt.ok {
				t.Fatalf("ok = %v (%+v)", ok, m)
			}
			if !ok {
				return
			}
			if m.Body != tt.want.Body || strings.Join(m.Mentions, ",") != strings.Join(tt.want.Mentions, ",") || m.Image == nil || *m.Image != *tt.want.Image {
				t.Fatalf("got %+v image %+v", m, m.Image)
			}
		})
	}
}

func TestDownloadLimit(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		io.WriteString(w, "0123456789")
	}))
	defer srv.Close()
	cli, err := mautrix.NewClient(srv.URL, "@ada:z", "tok")
	if err != nil {
		t.Fatal(err)
	}
	s := &Sender{cli: cli}
	data, err := s.Download(context.Background(), "mxc://example.com/abc", 10)
	if err != nil || string(data) != "0123456789" || !strings.HasSuffix(path, "/download/example.com/abc") {
		t.Fatalf("data %q err %v path %q", data, err, path)
	}
	if _, err := s.Download(context.Background(), "mxc://example.com/abc", 9); err == nil {
		t.Fatal("over the limit must fail")
	}
	if _, err := s.Download(context.Background(), "https://example.com/x", 9); err == nil {
		t.Fatal("not an mxc URI must fail")
	}
}
