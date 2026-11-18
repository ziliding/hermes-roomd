// Package silence recognises replies that mean "I have nothing to say".
// A silent reply posts nothing, does not count as a turn, and is never fed
// into another bot's context.
package silence

import (
	"strings"
	"unicode"
)

// tokens are compared after lower-casing, trimming edge punctuation/space and
// collapsing "_" to " ". "pass" is deliberately absent: a character may say it.
var tokens = map[string]bool{
	"silent":   true,
	"silence":  true,
	"no reply": true,
	"noreply":  true,
}

// Is reports whether text is only a silence marker: [SILENT], SILENT,
// NO_REPLY, NO REPLY (any case, edge punctuation), narration such as
// "(silent)" / "*stays silent*", 🔇, or bare punctuation like "." / "…".
func Is(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return true
	}
	// Strip emoji mute markers.
	t = strings.TrimSpace(strings.ReplaceAll(t, "🔇", ""))
	if t == "" {
		return true
	}
	if strings.Trim(t, ".… ") == "" {
		return true // ".", "...", "…" — but not "?", "!" or emoji, which are real replies
	}
	core := strings.TrimFunc(t, isEdge)
	if core == "" {
		return false
	}
	norm := strings.ToLower(strings.ReplaceAll(core, "_", " "))
	norm = strings.Join(strings.Fields(norm), " ")
	if tokens[norm] {
		return true
	}
	// Narration of silence: "(stays silent)", "*remains silent*", "[silent]".
	if wrapped(t) {
		switch norm {
		case "stays silent", "remains silent", "says nothing", "silently listens", "listens silently", "no response":
			return true
		}
	}
	return false
}

func isEdge(r rune) bool {
	return unicode.IsPunct(r) || unicode.IsSpace(r)
}

func wrapped(t string) bool {
	if len(t) < 2 {
		return false
	}
	pairs := map[byte]byte{'(': ')', '[': ']', '*': '*', '_': '_'}
	closer, ok := pairs[t[0]]
	return ok && t[len(t)-1] == closer
}
