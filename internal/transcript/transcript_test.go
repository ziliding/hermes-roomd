package transcript

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestSteeredMarksArePrunedByTheCursor(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 1; i <= 5; i++ {
		if _, err := s.Append(ctx, Entry{RoomID: "!r:z", EventID: fmt.Sprintf("$%d", i), Sender: "@alex:z", Body: "m", TS: 1}); err != nil {
			t.Fatal(err)
		}
	}
	for _, seq := range []int64{3, 5} {
		if err := s.MarkSteered(ctx, "!r:z", "ada", seq); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkSteered(ctx, "!r:z", "ada", 3); err != nil { // idempotent
		t.Fatal(err)
	}
	got, err := s.Steered(ctx, "!r:z", "ada", 0)
	if err != nil || len(got) != 2 || !got[3] || !got[5] {
		t.Fatalf("steered %v %v", got, err)
	}
	if other, _ := s.Steered(ctx, "!r:z", "cleo", 0); len(other) != 0 {
		t.Fatalf("marks leak to another bot: %v", other)
	}
	if err := s.SetCursor(ctx, "!r:z", "ada", 4); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Steered(ctx, "!r:z", "ada", 0); len(got) != 1 || !got[5] {
		t.Fatalf("after cursor 4: %v", got)
	}
}

func TestMediaColumnsAndMigration(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/old.db"
	// A database from before the media columns existed.
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE messages (seq INTEGER PRIMARY KEY AUTOINCREMENT, room_id TEXT NOT NULL,
		event_id TEXT NOT NULL UNIQUE, sender TEXT NOT NULL, body TEXT NOT NULL, formatted_body TEXT NOT NULL DEFAULT '',
		mentions TEXT NOT NULL DEFAULT '', ts INTEGER NOT NULL);
		INSERT INTO messages(room_id, event_id, sender, body, ts) VALUES('!r:z', '$old', '@alex:z', 'hi', 1)`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	for i := 0; i < 2; i++ { // opening twice: the migration is idempotent
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, err := s.Append(ctx, Entry{RoomID: "!r:z", EventID: "$img", Sender: "@alex:z", Body: "[image: cat.jpg]",
				TS: 2, MediaURL: "mxc://example.com/abc", MediaMIME: "image/jpeg"}); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.Since(ctx, "!r:z", 0, 10)
		s.Close()
		if err != nil || len(got) != 2 || got[0].MediaURL != "" || got[1].MediaURL != "mxc://example.com/abc" || got[1].MediaMIME != "image/jpeg" {
			t.Fatalf("entries %+v %v", got, err)
		}
	}
}

func TestSenderOf(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Append(ctx, Entry{RoomID: "!a", EventID: "$1", Sender: "@ada:z", Body: "hi"}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ room, event, want string }{
		{"!a", "$1", "@ada:z"},
		{"!a", "$2", ""}, // not recorded
		{"!b", "$1", ""}, // another room
	} {
		if got, err := s.SenderOf(ctx, tt.room, tt.event); err != nil || got != tt.want {
			t.Errorf("SenderOf(%s, %s) = %q, %v; want %q", tt.room, tt.event, got, err, tt.want)
		}
	}
}

func TestLastSegmentBreak(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.LastSegmentBreak(ctx, "!a"); err != nil || got != 0 {
		t.Fatalf("no breaks: %d, %v", got, err)
	}
	for _, b := range []struct {
		room string
		seq  int64
	}{{"!a", 5}, {"!a", 9}, {"!b", 12}} {
		if err := s.AddSegmentBreak(ctx, b.room, b.seq); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.LastSegmentBreak(ctx, "!a"); err != nil || got != 9 {
		t.Fatalf("LastSegmentBreak(!a) = %d, %v; want 9", got, err)
	}
}
