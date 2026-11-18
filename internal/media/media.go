// Package media turns a bot's reply into what can be posted to Matrix.
//
// The Hermes API server rewrites a reply's MEDIA:/path tags for images into
// inline markdown data URLs, ![image](data:image/png;base64,…), up to 5 MB
// each. Posted as text that is far over Matrix's 64 KB event limit, so
// roomd pulls the images out (to post them as m.image events), removes
// MEDIA: tags Hermes left alone (they would leak server paths), and splits
// text that is still too long.
package media

import (
	"bytes"
	"encoding/base64"
	"image"
	_ "image/gif" // registers decoders, for width and height
	_ "image/jpeg"
	_ "image/png"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Image is one inline image taken out of a reply.
type Image struct {
	Alt    string
	MIME   string
	Data   []byte
	Width  int // 0 when the header can't be read cheaply (webp, bmp)
	Height int
}

// Filename is a name for the image, e.g. image.png.
func (i Image) Filename() string {
	ext := strings.TrimPrefix(i.MIME, "image/")
	switch ext {
	case "jpeg":
		ext = "jpg"
	case "svg+xml":
		ext = "svg"
	}
	return "image." + ext
}

// Body is the m.image body: the alt text, or the filename when the alt is
// empty or Hermes' generic "image".
func (i Image) Body() string {
	if a := strings.TrimSpace(i.Alt); a != "" && !strings.EqualFold(a, "image") {
		return a
	}
	return i.Filename()
}

// Part is one piece of a reply, in order: text or an image.
type Part struct {
	Text  string
	Image *Image
}

// Placeholder stands in for an image in the transcript (never the data).
const Placeholder = "[image]"

// Unsent replaces an inline image that could not be decoded or sent.
const Unsent = "(image could not be sent)"

var (
	dataURLRe = regexp.MustCompile(`!\[([^\]\n]*)\]\(\s*data:([^;,)\s]*)((?:;[^;,)\s]*)*),([^)]*)\)`)
	// A MEDIA: tag Hermes did not rewrite (not an image, too big, unreadable).
	mediaTagRe = regexp.MustCompile("[*_]{0,3}(?:" +
		"`MEDIA:[^`\\n]+`|\"MEDIA:[^\"\\n]+\"|'MEDIA:[^'\\n]+'|" + // the whole tag quoted
		"MEDIA:\\s*(?:`[^`\\n]+`|\"[^\"\\n]+\"|'[^'\\n]+'|\\S+))[*_]{0,3}") // or just the path
	blankRunRe = regexp.MustCompile(`\n{3,}`)
	spaceRunRe = regexp.MustCompile(`[ \t]{2,}`)
)

// Split cuts a reply into text and image parts in their original order.
// An inline data URL that is not a decodable image becomes the Unsent note.
// MEDIA: tags are removed. dropped lists what was removed or not decoded,
// for the log (never the data itself).
func Split(text string) (parts []Part, dropped []string) {
	var pending strings.Builder // text since the last image
	flush := func() {
		if t := tidy(pending.String()); t != "" {
			parts = append(parts, Part{Text: t})
		}
		pending.Reset()
	}
	pos := 0
	for _, m := range dataURLRe.FindAllStringSubmatchIndex(text, -1) {
		pending.WriteString(text[pos:m[0]])
		pos = m[1]
		alt, mime, params, data := text[m[2]:m[3]], strings.ToLower(text[m[4]:m[5]]), text[m[6]:m[7]], text[m[8]:m[9]]
		img, why := decode(alt, mime, params, data)
		if img == nil {
			dropped = append(dropped, "inline data URL ("+mime+"): "+why)
			pending.WriteString(" " + Unsent + " ")
			continue
		}
		flush()
		parts = append(parts, Part{Image: img})
	}
	pending.WriteString(text[pos:])
	flush()
	for i := range parts {
		if parts[i].Image != nil {
			continue
		}
		for _, tag := range mediaTagRe.FindAllString(parts[i].Text, -1) {
			dropped = append(dropped, "MEDIA tag: "+strings.TrimSpace(tag))
		}
		parts[i].Text = tidy(mediaTagRe.ReplaceAllString(parts[i].Text, ""))
	}
	out := parts[:0]
	for _, p := range parts {
		if p.Image != nil || p.Text != "" {
			out = append(out, p)
		}
	}
	return out, dropped
}

func decode(alt, mime, params, data string) (*Image, string) {
	if !strings.HasPrefix(mime, "image/") {
		return nil, "not an image"
	}
	if !strings.Contains(strings.ToLower(params), ";base64") {
		return nil, "not base64"
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(data), ""))
	if err != nil || len(raw) == 0 {
		return nil, "malformed base64"
	}
	img := &Image{Alt: alt, MIME: mime, Data: raw}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(raw)); err == nil {
		img.Width, img.Height = cfg.Width, cfg.Height
	}
	return img, ""
}

func tidy(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(spaceRunRe.ReplaceAllString(l, " "), " \t")
	}
	return strings.TrimSpace(blankRunRe.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// Transcript is the reply as the transcript records it: the text, with
// Placeholder for each image.
func Transcript(parts []Part) string {
	var out []string
	for _, p := range parts {
		if p.Image != nil {
			out = append(out, Placeholder)
		} else {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "\n")
}

// MaxText is the most text roomd puts in one event, in bytes. Matrix caps a
// whole event at 65535 bytes, including JSON escaping and the envelope.
const MaxText = 30000

// Chunks splits text into pieces of at most max bytes, preferring a
// paragraph break, then a line break, then a space, and never cutting a
// UTF-8 character.
func Chunks(text string, max int) []string {
	var out []string
	for len(text) > max {
		cut := max
		for !utf8.RuneStart(text[cut]) {
			cut--
		}
		window := text[:cut]
		for _, sep := range []string{"\n\n", "\n", " "} {
			if i := strings.LastIndex(window, sep); i > max/2 {
				cut = i
				break
			}
		}
		if piece := strings.TrimSpace(text[:cut]); piece != "" {
			out = append(out, piece)
		}
		text = strings.TrimLeft(text[cut:], " \n")
	}
	if text = strings.TrimSpace(text); text != "" {
		out = append(out, text)
	}
	return out
}
