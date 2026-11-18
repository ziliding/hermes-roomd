// Package media turns a bot's reply into what can be posted to Matrix.
//
// Images reach a reply in three shapes. Hermes' Runs API returns the bot's
// MEDIA:/path tags as they are, and models told so write bare absolute
// paths instead; Hermes' other endpoints inline images as markdown data
// URLs, ![image](data:image/png;base64,…), far over Matrix's 64 KB event
// limit as text. roomd pulls every image out (to post it as an m.image),
// reading files only from allowed directories, keeps host paths out of the
// posted text, and splits text that is still too long.
package media

import (
	"bytes"
	"encoding/base64"
	"image"
	_ "image/gif" // registers decoders, for width and height
	_ "image/jpeg"
	_ "image/png"
	"regexp"
	"slices"
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

// pathStops are the characters that end an unquoted path in a reply.
const pathStops = "（）「」『』【】《》〈〉，。、；：！？"

var (
	dataURLRe = regexp.MustCompile(`!\[([^\]\n]*)\]\(\s*data:([^;,)\s]*)((?:;[^;,)\s]*)*),([^)]*)\)`)
	// A MEDIA: tag Hermes did not rewrite (not an image, too big, unreadable).
	mediaTagRe = regexp.MustCompile("[*_]{0,3}(?:" +
		"`MEDIA:[^`\\n]+`|\"MEDIA:[^\"\\n]+\"|'MEDIA:[^'\\n]+'|" + // the whole tag quoted
		"MEDIA:\\s*(?:`[^`\\n]+`|\"[^\"\\n]+\"|'[^'\\n]+'|" + // a quoted path
		"(?i:/[^\\s" + pathStops + "]+?\\.(?:png|jpe?g|gif|webp|bmp))|" + // an image path ends at its extension
		"[^\\s" + pathStops + "]+))[*_]{0,3}") // anything else ends at a space or CJK punctuation
	blankRunRe = regexp.MustCompile(`\n{3,}`)
	spaceRunRe = regexp.MustCompile(`[ \t]{2,}`)
)

// Split cuts a reply into text and image parts in their original order.
// Three kinds of image reference are taken out of the text:
//   - inline data URLs, ![alt](data:image/…;base64,…);
//   - MEDIA:/path tags, as Hermes' Runs API returns them;
//   - bare absolute paths ending in an image extension (.png, .jpg, …),
//     which models write when told the Runs API doesn't deliver MEDIA tags.
//
// Files are read through files (nil: no file is ever read). A reference
// that can't become an image is replaced by the Unsent note, except a
// MEDIA: tag for a non-image file, which is just removed; a bare path that
// can't be read is left as text only when files is nil (the feature is
// off). No host path that was turned into an image or note stays in the
// text. dropped lists what was left out and why, for the log (never data).
func Split(text string, files *Files) (parts []Part, dropped []string) {
	var pending strings.Builder // text since the last image
	flush := func() {
		if t := tidy(pending.String()); t != "" {
			parts = append(parts, Part{Text: t})
		}
		pending.Reset()
	}
	pos := 0
	for _, r := range findRefs(text, files != nil) {
		pending.WriteString(text[pos:r.start])
		pos = r.end
		img, why := r.load(files)
		switch {
		case img != nil:
			flush()
			parts = append(parts, Part{Image: img})
		case r.kind == refMedia && !IsImagePath(r.path):
			dropped = append(dropped, "MEDIA tag (not an image): "+r.path)
			pending.WriteString(" ")
		default:
			dropped = append(dropped, r.describe()+": "+why)
			pending.WriteString(" " + Unsent + " ")
		}
	}
	pending.WriteString(text[pos:])
	flush()
	return parts, dropped
}

type refKind int

const (
	refDataURL refKind = iota
	refMedia
	refPath
)

// ref is one image reference found in a reply.
type ref struct {
	kind       refKind
	start, end int
	path       string // refMedia, refPath
	alt        string // refDataURL
	mime       string // refDataURL
	params     string // refDataURL
	data       string // refDataURL
}

func (r ref) describe() string {
	switch r.kind {
	case refDataURL:
		return "inline data URL (" + r.mime + ")"
	case refMedia:
		return "MEDIA tag " + r.path
	}
	return "image path " + r.path
}

func (r ref) load(files *Files) (*Image, string) {
	if r.kind == refDataURL {
		return decode(r.alt, r.mime, r.params, r.data)
	}
	if !IsImagePath(r.path) {
		return nil, "not an image file name"
	}
	img, err := files.Load(r.path)
	if err != nil {
		return nil, err.Error()
	}
	return img, ""
}

var (
	// An absolute path ending in an image extension, shortest first so two
	// paths with no space between them stay apart. Whitespace, quotes,
	// brackets and CJK punctuation end a path.
	barePathRe    = regexp.MustCompile(`(?i)/[^\s"'` + "`" + `<>()\[\]{}` + pathStops + `]+?\.(?:png|jpe?g|gif|webp|bmp)`)
	mediaPrefixRe = regexp.MustCompile(`^[*_]{0,3}[` + "`" + `"']?MEDIA:\s*`)
)

// findRefs returns the image references in text, in order, without overlaps.
// Bare paths are looked for only when paths is true.
func findRefs(text string, paths bool) []ref {
	var refs []ref
	taken := func(a, b int) bool {
		for _, r := range refs {
			if a < r.end && b > r.start {
				return true
			}
		}
		return false
	}
	for _, m := range dataURLRe.FindAllStringSubmatchIndex(text, -1) {
		refs = append(refs, ref{kind: refDataURL, start: m[0], end: m[1], alt: text[m[2]:m[3]],
			mime: strings.ToLower(text[m[4]:m[5]]), params: text[m[6]:m[7]], data: text[m[8]:m[9]]})
	}
	for _, m := range mediaTagRe.FindAllStringIndex(text, -1) {
		if !taken(m[0], m[1]) {
			refs = append(refs, ref{kind: refMedia, start: m[0], end: m[1], path: mediaPath(text[m[0]:m[1]])})
		}
	}
	if paths {
		for _, m := range barePathRe.FindAllStringIndex(text, -1) {
			if taken(m[0], m[1]) || !pathBoundary(text, m[0], m[1]) {
				continue
			}
			refs = append(refs, ref{kind: refPath, start: m[0], end: m[1], path: text[m[0]:m[1]]})
		}
	}
	slices.SortFunc(refs, func(a, b ref) int { return a.start - b.start })
	return refs
}

// mediaPath is the path inside a MEDIA: tag, without quotes, emphasis or
// trailing punctuation.
func mediaPath(tag string) string {
	p := mediaPrefixRe.ReplaceAllString(tag, "")
	p = strings.Trim(p, "`\"'*_ ")
	return strings.TrimRight(p, ".,;:!?)]}，。；：！？）")
}

// pathBoundary rejects a match that is part of something longer: a URL
// (https://host/x.png), a relative path (a/b.png) or a longer file name
// (x.png.bak).
func pathBoundary(text string, start, end int) bool {
	if start > 0 {
		prev, _ := utf8.DecodeLastRuneInString(text[:start])
		if prev < utf8.RuneSelf && (isWordByte(byte(prev)) || strings.ContainsRune(":/.~-", prev)) {
			return false
		}
	}
	if strings.HasPrefix(text[start:], "//") {
		return false
	}
	if end < len(text) {
		next := text[end]
		if isWordByte(next) || next == '/' || next == '-' || (next == '.' && end+1 < len(text) && isWordByte(text[end+1])) {
			return false
		}
	}
	return true
}

func isWordByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
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
