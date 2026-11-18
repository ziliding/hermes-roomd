package room

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ziliding/hermes-roomd/internal/media"
)

// A fake 1 KB "PNG": roomd only decodes base64 and reads the header if it can.
var fakePNG = strings.Repeat("x", 1024)

func inline(data string) string {
	return "![image](data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte(data)) + ")"
}

func TestReplyWithImagesPostsMImagesInOrder(t *testing.T) {
	reply := "Here it is:\n\n" + inline(fakePNG) + "\n\nLike it?\nMEDIA:/home/bot/.hermes/out/notes.pdf"
	h := newHarness(t, 2, func(bot string, n int) string {
		if bot == "ada" {
			return reply
		}
		return "nice " + bot
	})
	h.runOne("Ada, draw a cat")
	posts, _, _, _, calls := h.snap()
	want := []string{"ada: Here it is:", "ada: <image image/png 1024B>", "ada: Like it?", "cleo: nice cleo"}
	if !slices.Equal(posts, want) {
		t.Fatalf("posts %q\nwant %q", posts, want)
	}
	// Cleo sees the reply with a placeholder: no base64, no server path.
	d := calls[1].delta
	if !strings.Contains(d, `<msg from="Ada">[image]</msg>`) || strings.Contains(d, "base64") || strings.Contains(d, "MEDIA:") || strings.Contains(d, "notes.pdf") {
		t.Fatalf("cleo's delta %q", d)
	}
	entries, err := h.o.store.Since(h.ctx, roomID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for _, e := range entries {
		bodies = append(bodies, e.Body)
	}
	if !slices.Equal(bodies[1:4], []string{"Here it is:", media.Placeholder, "Like it?"}) {
		t.Fatalf("transcript %q", bodies)
	}
}

func TestBackgroundImageReplyRelationOnFirstEventOnly(t *testing.T) {
	release := make(chan struct{})
	h := newHarnessWith(t, harnessOpts{maxTurns: 1, backgroundAfter: 20 * time.Millisecond}, func(bot string, n int) string {
		<-release
		return inline(fakePNG) + "\nDone: a cat."
	})
	reqID := startBusyAda(t, h)
	close(release)
	posts := h.waitPosts(2)
	if want := []string{"ada: <image image/png 1024B> ↩ " + reqID, "ada: Done: a cat."}; !slices.Equal(posts, want) {
		t.Fatalf("posts %q\nwant %q", posts, want)
	}
}

func TestImageUploadFailureStillPostsTheText(t *testing.T) {
	h := newHarness(t, 1, func(bot string, n int) string { return "Your cat:\n" + inline(fakePNG) })
	h.imageFail = true
	h.runOne("Ada, draw a cat")
	posts, _, _, _, _ := h.snap()
	if want := []string{"ada: Your cat:", "ada: " + media.Unsent}; !slices.Equal(posts, want) {
		t.Fatalf("posts %q", posts)
	}
}

func TestOversizedTextIsSplit(t *testing.T) {
	long := strings.Repeat(strings.Repeat("word ", 200)+"\n\n", 80) // ~80 KB
	h := newHarness(t, 1, func(bot string, n int) string { return long })
	h.runOne("Ada, write a lot")
	posts, _, _, _, _ := h.snap()
	if len(posts) < 3 {
		t.Fatalf("%d posts", len(posts))
	}
	total := 0
	for _, p := range posts {
		body := strings.TrimPrefix(p, "ada: ")
		if len(body) > media.MaxText {
			t.Fatalf("event of %d bytes", len(body))
		}
		total += len(strings.Fields(body))
	}
	if total != len(strings.Fields(long)) {
		t.Fatalf("words posted %d, want %d", total, len(strings.Fields(long)))
	}
}

// Runs return MEDIA: tags (or bare paths) as they are: roomd reads the file
// from media_dirs and posts it, and no host path reaches the room or the
// transcript.
func TestReplyNamingAnImageFile(t *testing.T) {
	dir := t.TempDir()
	png := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 64))
	if err := os.WriteFile(filepath.Join(dir, "cat_00002_.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := media.NewFiles([]string{dir}, media.MaxFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	cat, missing := filepath.Join(dir, "cat_00002_.png"), filepath.Join(dir, "gone.png")
	tests := []struct {
		name  string
		reply string
		want  []string
	}{
		{"MEDIA tag", "画好了！\nMEDIA:" + cat, []string{"ada: 画好了！", "ada: <image image/png 72B>"}},
		{"bare path", "Here: " + cat + " enjoy", []string{"ada: Here:", "ada: <image image/png 72B>", "ada: enjoy"}},
		{"missing file: note, no path", "Here: " + missing, []string{"ada: Here: " + media.Unsent}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarnessWith(t, harnessOpts{maxTurns: 1, files: files}, func(string, int) string { return tt.reply })
			h.runOne("Ada, draw a cat")
			posts, _, _, _, _ := h.snap()
			if !slices.Equal(posts, tt.want) {
				t.Fatalf("posts %q\nwant %q", posts, tt.want)
			}
			entries, _ := h.o.store.Since(h.ctx, roomID, 0, 50)
			for _, e := range entries {
				if strings.Contains(e.Body, dir) {
					t.Fatalf("host path in the transcript: %q", e.Body)
				}
			}
		})
	}
}
