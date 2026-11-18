package media

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif" // decoders for image.Decode
	"image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// Vision limits for images sent to a model. Hermes' API server refuses
// request bodies over 10 MB, and vision models downscale large images
// anyway, so roomd sends something small.
const (
	VisionMaxEdge  = 1568       // longest side, pixels
	VisionKeepSize = 1 << 20    // a PNG/JPEG this small and within the edge is sent as is
	maxPixels      = 64_000_000 // refuse to decode anything bigger (decompression bombs)
)

// ErrNotDecodable means the bytes are not an image roomd can read.
var ErrNotDecodable = errors.New("not a readable image")

// ForVision turns an image file's bytes into what a run carries: a PNG or
// JPEG whose longest side is at most maxEdge. A small PNG or JPEG that
// already fits is returned unchanged; anything else (bigger, or GIF, WebP,
// BMP) is decoded, scaled down, flattened onto white and re-encoded as JPEG.
// Only the first frame of an animation is kept.
func ForVision(data []byte, maxEdge int) (mime string, out []byte, err error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrNotDecodable, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxPixels {
		return "", nil, fmt.Errorf("%w: %dx%d pixels", ErrNotDecodable, cfg.Width, cfg.Height)
	}
	fits := cfg.Width <= maxEdge && cfg.Height <= maxEdge
	if fits && len(data) <= VisionKeepSize && (format == "png" || format == "jpeg") {
		return "image/" + format, data, nil
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrNotDecodable, err)
	}
	w, h := cfg.Width, cfg.Height
	if !fits {
		if w >= h {
			w, h = maxEdge, max(1, h*maxEdge/w)
		} else {
			w, h = max(1, w*maxEdge/h), maxEdge
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	var b bytes.Buffer
	if err := jpeg.Encode(&b, dst, &jpeg.Options{Quality: 85}); err != nil {
		return "", nil, err
	}
	return "image/jpeg", b.Bytes(), nil
}
