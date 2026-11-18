package media

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// MaxFileBytes is the largest image file roomd reads and uploads.
const MaxFileBytes = 10 << 20

// imageExts are the image file extensions roomd looks for in a reply (the
// same set Hermes inlines).
var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true}

// IsImagePath reports whether path ends in an image extension.
func IsImagePath(path string) bool { return imageExts[strings.ToLower(filepath.Ext(path))] }

// Files reads image files a bot named in its reply, but only from the
// allowed directories. Hermes' Runs API returns MEDIA:/path tags (or bare
// paths) as they are, and roomd runs on the same host as Hermes.
type Files struct {
	dirs []string // cleaned, symlinks resolved
	max  int64
}

// NewFiles allows reading images from dirs (absolute paths). A directory
// that doesn't exist is an error.
func NewFiles(dirs []string, maxBytes int64) (*Files, error) {
	f := &Files{max: maxBytes}
	for _, d := range dirs {
		if !filepath.IsAbs(d) {
			return nil, fmt.Errorf("media dir %q is not an absolute path", d)
		}
		real, err := filepath.EvalSymlinks(filepath.Clean(d))
		if err != nil {
			return nil, fmt.Errorf("media dir %q: %w", d, err)
		}
		if st, err := os.Stat(real); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("media dir %q is not a directory", d)
		}
		f.dirs = append(f.dirs, real)
	}
	return f, nil
}

// ErrNotAllowed means the path is outside every allowed directory.
var ErrNotAllowed = errors.New("outside media_dirs")

// Load reads one image file. It must resolve (symlinks included) to a
// regular file inside an allowed directory, at most the size limit, with
// an image extension, and its bytes must sniff as an image.
func (f *Files) Load(path string) (*Image, error) {
	if f == nil || len(f.dirs) == 0 {
		return nil, fmt.Errorf("%w (none configured)", ErrNotAllowed)
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("not an absolute path")
	}
	if !IsImagePath(path) {
		return nil, errors.New("not an image file name")
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	if !f.allowed(real) {
		return nil, ErrNotAllowed
	}
	st, err := os.Lstat(real)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if st.Size() > f.max {
		return nil, fmt.Errorf("too big (%d bytes, limit %d)", st.Size(), f.max)
	}
	fh, err := os.Open(real)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	data, err := io.ReadAll(io.LimitReader(fh, f.max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > f.max {
		return nil, fmt.Errorf("too big (limit %d bytes)", f.max)
	}
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		return nil, fmt.Errorf("not an image (%s)", mime)
	}
	img := &Image{Alt: filepath.Base(real), MIME: mime, Data: data}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		img.Width, img.Height = cfg.Width, cfg.Height
	}
	return img, nil
}

func (f *Files) allowed(real string) bool {
	for _, d := range f.dirs {
		rel, err := filepath.Rel(d, real)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}
