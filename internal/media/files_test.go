package media

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupDirs makes an allowed dir (out) with a real PNG, plus a secret dir
// outside it, and returns the allowed dir, the secret dir and Files.
func setupDirs(t *testing.T) (out, secret string, f *Files) {
	t.Helper()
	root := t.TempDir()
	out, secret = filepath.Join(root, "out"), filepath.Join(root, "secret")
	for _, d := range []string{out, secret} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path string, data []byte) {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(out, "cat.png"), pngData(t, 5, 4))
	write(filepath.Join(out, "fake.png"), []byte("this is plain text, not an image"))
	write(filepath.Join(out, "big.png"), append(pngData(t, 1, 1), make([]byte, 2048)...))
	write(filepath.Join(secret, "key.png"), pngData(t, 1, 1))
	if err := os.Symlink(filepath.Join(secret, "key.png"), filepath.Join(out, "escape.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(out, "cat.png"), filepath.Join(out, "alias.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(out, "dir.png"), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := NewFiles([]string{out}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return out, secret, f
}

func TestFilesLoad(t *testing.T) {
	out, secret, f := setupDirs(t)
	tests := []struct {
		name    string
		path    string
		wantErr string // "" = loads
	}{
		{"image in the allowed dir", filepath.Join(out, "cat.png"), ""},
		{"symlink inside the dir", filepath.Join(out, "alias.png"), ""},
		{"symlink escaping the dir", filepath.Join(out, "escape.png"), "outside media_dirs"},
		{".. traversal", out + "/../secret/key.png", "outside media_dirs"},
		{"outside the dir", filepath.Join(secret, "key.png"), "outside media_dirs"},
		{"missing file", filepath.Join(out, "nope.png"), "no such file"},
		{"too big", filepath.Join(out, "big.png"), "too big"},
		{"text bytes with a .png name", filepath.Join(out, "fake.png"), "not an image"},
		{"directory", filepath.Join(out, "dir.png"), "not a regular file"},
		{"not an image extension", filepath.Join(out, "notes.txt"), "not an image file name"},
		{"relative path", "out/cat.png", "not an absolute path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			img, err := f.Load(tt.path)
			if tt.wantErr == "" {
				if err != nil || img.MIME != "image/png" || img.Width != 5 || img.Height != 4 || img.Alt != "cat.png" {
					t.Fatalf("img %+v err %v", img, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
	var none *Files
	if _, err := none.Load(filepath.Join(out, "cat.png")); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("nil Files: %v", err)
	}
	if _, err := NewFiles([]string{filepath.Join(out, "missing")}, 1); err == nil {
		t.Fatal("a missing media dir must be an error")
	}
	if _, err := NewFiles([]string{"relative/dir"}, 1); err == nil {
		t.Fatal("a relative media dir must be an error")
	}
}

func TestSplitFileReferences(t *testing.T) {
	out, secret, f := setupDirs(t)
	cat := filepath.Join(out, "cat.png")
	img := "I:image/png 5x4 cat.png"
	tests := []struct {
		name        string
		in          string
		files       *Files
		want        string
		wantDropped int
	}{
		{"MEDIA tag", "Done!\nMEDIA:" + cat, f, "T:Done! | " + img, 0},
		{"quoted MEDIA tag", "Here `MEDIA:" + cat + "` it is", f, "T:Here | " + img + " | T:it is", 0},
		{"bare path, removed from the text", "Saved to " + cat + " for you.", f, "T:Saved to | " + img + " | T:for you.", 0},
		{"bare path in Chinese text", "画好了：" + cat + "，你看看", f, "T:画好了： | " + img + " | T:，你看看", 0},
		{"MEDIA tag glued to Chinese text", "图在这里MEDIA:" + cat + "里面", f, "T:图在这里 | " + img + " | T:里面", 0},
		{"two paths", cat + " and " + cat, f, img + " | T:and | " + img, 0},
		{"path outside the dirs: note, no path", "Look: " + filepath.Join(secret, "key.png"), f, "T:Look: " + Unsent, 1},
		{"symlink escape: note", "Look: " + filepath.Join(out, "escape.png"), f, "T:Look: " + Unsent, 1},
		{"missing file: note", "MEDIA:" + filepath.Join(out, "nope.png"), f, "T:" + Unsent, 1},
		{"non-image bytes: note", "MEDIA:" + filepath.Join(out, "fake.png"), f, "T:" + Unsent, 1},
		{"non-image MEDIA tag: removed, no note", "Report: MEDIA:" + filepath.Join(out, "r.pdf") + " ok", f, "T:Report: ok", 1},
		{"feature off: MEDIA image tag becomes a note", "MEDIA:" + cat, nil, "T:" + Unsent, 1},
		{"feature off: bare path stays text", "It's at " + cat, nil, "T:It's at " + cat, 0},
		{"URLs are not paths", "See https://example.com/a.png", f, "T:See https://example.com/a.png", 0},
		{"relative paths are not paths", "See out/cat.png", f, "T:See out/cat.png", 0},
		{"longer names are not image paths", "See " + cat + ".bak", f, "T:See " + cat + ".bak", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parts, dropped := Split(tt.in, tt.files)
			if got := describe(parts); got != tt.want {
				t.Fatalf("got  %q\nwant %q\ndropped %q", got, tt.want, dropped)
			}
			if len(dropped) != tt.wantDropped {
				t.Fatalf("dropped %q", dropped)
			}
		})
	}
}
