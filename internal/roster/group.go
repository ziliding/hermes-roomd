package roster

import (
	"regexp"
	"strings"
	"unicode"
)

// DefaultGroupWords are the words that address every character at once.
var DefaultGroupWords = []string{"你们", "大家", "各位", "everyone", "everybody", "all of you", "you all", "y'all", "you guys"}

// GroupMatcher recognises a message addressed to the whole group ("你们觉得呢?",
// "what do you all think?"). A nil matcher matches nothing.
type GroupMatcher struct{ re *regexp.Regexp }

// NewGroupMatcher compiles words, case-insensitively. A word written in
// Latin script must stand alone ("all of you" does not match inside
// "tall of you"); any other word (Chinese, Japanese) matches anywhere.
func NewGroupMatcher(words []string) *GroupMatcher {
	var alts []string
	for _, w := range words {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		q := regexp.QuoteMeta(w)
		if isLatin(w) {
			q = `(?:^|[^\pL\pN_])` + q + `(?:[^\pL\pN_]|$)`
		}
		alts = append(alts, q)
	}
	if len(alts) == 0 {
		return nil
	}
	return &GroupMatcher{re: regexp.MustCompile(`(?i)(?:` + strings.Join(alts, "|") + `)`)}
}

// Match reports whether body addresses the whole group.
func (g *GroupMatcher) Match(body string) bool { return g != nil && g.re.MatchString(body) }

func isLatin(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) && !unicode.Is(unicode.Latin, r) {
			return false
		}
	}
	return true
}
