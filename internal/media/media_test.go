package media

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func pngData(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func tag(alt, mime string, data []byte) string {
	return "![" + alt + "](data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data) + ")"
}

// describe renders parts as "T:text" / "I:mime WxH body" for comparison.
func describe(parts []Part) string {
	var out []string
	for _, p := range parts {
		if p.Image != nil {
			out = append(out, "I:"+p.Image.MIME+" "+itoa(p.Image.Width)+"x"+itoa(p.Image.Height)+" "+p.Image.Body())
		} else {
			out = append(out, "T:"+p.Text)
		}
	}
	return strings.Join(out, " | ")
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestSplit(t *testing.T) {
	p := pngData(t, 3, 2)
	tests := []struct {
		name        string
		in          string
		want        string
		wantDropped int
	}{
		{"plain text", "Just words.", "T:Just words.", 0},
		{"one image alone", tag("image", "image/png", p), "I:image/png 3x2 image.png", 0},
		{"text image text, in order", "Here it is:\n\n" + tag("image", "image/png", p) + "\n\nLike it?",
			"T:Here it is: | I:image/png 3x2 image.png | T:Like it?", 0},
		{"several images, alt kept", tag("a cat", "image/png", p) + " and " + tag("", "image/png", p),
			"I:image/png 3x2 a cat | T:and | I:image/png 3x2 image.png", 0},
		{"webp: no cheap size", tag("image", "image/webp", []byte("RIFFxxxxWEBP")), "I:image/webp 0x0 image.webp", 0},
		{"malformed base64", "Look: ![image](data:image/png;base64,@@@notbase64@@@) nice", "T:Look: " + Unsent + " nice", 1},
		{"non-image data URL", "Doc: ![file](data:application/pdf;base64,JVBERi0=)", "T:Doc: " + Unsent, 1},
		{"leftover MEDIA tag stripped", "Saved it.\nMEDIA:/home/bot/.hermes/out/report.pdf\nDone.", "T:Saved it.\n\nDone.", 1},
		{"quoted MEDIA tag stripped", "See `MEDIA:/tmp/a b.mp4` here", "T:See here", 1},
		{"quoted path stripped", "Clip: MEDIA:\"/tmp/a b.mp4\" ok", "T:Clip: ok", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parts, dropped := Split(tt.in, nil)
			if got := describe(parts); got != tt.want {
				t.Fatalf("got  %q\nwant %q", got, tt.want)
			}
			if len(dropped) != tt.wantDropped {
				t.Fatalf("dropped %q", dropped)
			}
			for _, d := range dropped {
				if strings.Contains(d, "base64,") {
					t.Fatalf("dropped log carries data: %q", d)
				}
			}
		})
	}
}

func TestSplitKeepsImageBytes(t *testing.T) {
	p := pngData(t, 4, 4)
	parts, _ := Split("x "+tag("image", "image/png", p), nil)
	if len(parts) != 2 || !bytes.Equal(parts[1].Image.Data, p) {
		t.Fatalf("parts %+v", parts)
	}
}

func TestTranscriptPlaceholder(t *testing.T) {
	parts, _ := Split("Here:\n"+tag("image", "image/png", pngData(t, 1, 1))+"\nBye", nil)
	if got := Transcript(parts); got != "Here:\n[image]\nBye" {
		t.Fatalf("transcript %q", got)
	}
}

func TestChunks(t *testing.T) {
	para := strings.Repeat("word ", 50) // 250 bytes
	long := strings.Repeat(para+"\n\n", 10)
	for _, c := range Chunks(long, 600) {
		if len(c) > 600 || strings.HasPrefix(c, " ") || c == "" {
			t.Fatalf("bad chunk %d %q", len(c), c[:20])
		}
	}
	if got := Chunks("short", 600); len(got) != 1 || got[0] != "short" {
		t.Fatalf("short %v", got)
	}
	// No separator at all, multi-byte runes: cut on a rune boundary.
	runes := strings.Repeat("é", 1000) // 2000 bytes
	chunks := Chunks(runes, 301)
	if strings.Join(chunks, "") != runes {
		t.Fatal("content lost")
	}
	for _, c := range chunks {
		if len(c) > 301 || !utf8.ValidString(c) {
			t.Fatalf("chunk %d valid=%v", len(c), utf8.ValidString(c))
		}
	}
}
