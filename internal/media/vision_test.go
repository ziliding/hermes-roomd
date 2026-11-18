package media

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"testing"
)

func TestForVision(t *testing.T) {
	small := pngData(t, 20, 10)
	var g bytes.Buffer
	if err := gif.Encode(&g, image.NewPaletted(image.Rect(0, 0, 30, 20), []color.Color{color.Black, color.White}), nil); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		data     []byte
		maxEdge  int
		wantMIME string
		w, h     int
		same     bool // returned unchanged
	}{
		{"small PNG passes through", small, 100, "image/png", 20, 10, true},
		{"wide PNG is scaled to the edge", pngData(t, 400, 100), 100, "image/jpeg", 100, 25, false},
		{"tall PNG is scaled to the edge", pngData(t, 50, 200), 100, "image/jpeg", 25, 100, false},
		{"GIF is re-encoded as JPEG", g.Bytes(), 100, "image/jpeg", 30, 20, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mime, out, err := ForVision(tt.data, tt.maxEdge)
			if err != nil || mime != tt.wantMIME || bytes.Equal(out, tt.data) != tt.same {
				t.Fatalf("mime %q same %v err %v", mime, bytes.Equal(out, tt.data), err)
			}
			cfg, format, err := image.DecodeConfig(bytes.NewReader(out))
			if err != nil || "image/"+format != tt.wantMIME || cfg.Width != tt.w || cfg.Height != tt.h {
				t.Fatalf("got %s %dx%d err %v", format, cfg.Width, cfg.Height, err)
			}
		})
	}
	if _, _, err := ForVision([]byte("plain text"), 100); !errors.Is(err, ErrNotDecodable) {
		t.Fatalf("text: %v", err)
	}
	var j bytes.Buffer
	_ = jpeg.Encode(&j, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil)
	if mime, out, err := ForVision(j.Bytes(), 100); err != nil || mime != "image/jpeg" || !bytes.Equal(out, j.Bytes()) {
		t.Fatalf("small JPEG: %q %v", mime, err)
	}
}
