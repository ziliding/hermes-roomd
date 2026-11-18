// Package contextbuild turns the transcript delta a bot has not yet seen into
// the attributed messages sent to its Hermes session:
//
//	<msg from="Alex">(to Ada) what do you think?</msg>
//	<msg from="Cleo">I'd rather go tomorrow.</msg>
//
// The speaker comes from the event sender, named by the display names in
// config. Each message is one element, so a multi-line body can never pass
// for a new speaker: body text is escaped (& < >) and cannot close or open a
// tag. @handles, Matrix ids, matrix.to links and pills are rewritten to
// "(to Name)". The bot's own messages are dropped (Hermes already holds them as
// assistant turns), as are silence markers.
package contextbuild

import (
	"regexp"
	"strings"

	"github.com/ziliding/hermes-roomd/internal/roster"
	"github.com/ziliding/hermes-roomd/internal/silence"
	"github.com/ziliding/hermes-roomd/internal/transcript"
)

// Builder renders deltas for one roster.
type Builder struct {
	r        *roster.Roster
	handles  []handle
	anyMXIDs *regexp.Regexp
}

type handle struct {
	re   *regexp.Regexp
	name string
}

// New precompiles the mention patterns for every participant.
func New(r *roster.Roster) *Builder {
	b := &Builder{
		r: r,
		// Any other Matrix user id, optionally inside a matrix.to link.
		anyMXIDs: regexp.MustCompile(`(^|[^\pL\pN_])(?:https://matrix\.to/#/)?@([A-Za-z0-9._=\-/+]+):[A-Za-z0-9.\-]+(?::\d+)?:?`),
	}
	for _, p := range append([]*roster.Person{r.User()}, r.Bots()...) {
		lp := roster.Localpart(p.MXID)
		alts := []string{regexp.QuoteMeta(p.MXID[1:]), regexp.QuoteMeta(p.Display)}
		if !strings.EqualFold(lp, p.Display) {
			alts = append(alts, regexp.QuoteMeta(lp))
		}
		// "@ada:server", "@Ada", "@ada", "https://matrix.to/#/@ada:server",
		// each optionally followed by an Element-style ":".
		re := regexp.MustCompile(`(?i)(^|[^\pL\pN_])(?:https://matrix\.to/#/)?@(?:` +
			strings.Join(alts, "|") + `):?([^\pL\pN_.\-]|\.(?:\s|$)|$)`)
		b.handles = append(b.handles, handle{re: re, name: p.Display})
	}
	return b
}

// Build renders entries for bot selfKey. It returns "" when nothing remains
// (e.g. the delta held only the bot's own lines or silence).
func (b *Builder) Build(selfKey string, entries []transcript.Entry) string {
	self, _ := b.r.ByKey(selfKey)
	var lines []string
	for _, e := range entries {
		if self != nil && e.Sender == self.MXID {
			continue
		}
		text := b.Rewrite(e.Body, e.FormattedBody)
		if silence.Is(text) {
			continue
		}
		lines = append(lines, Msg(b.r.DisplayFor(e.Sender), text))
	}
	return strings.Join(lines, "\n")
}

// Rewrite replaces mentions in body with "(to Name)" and strips reply fallbacks.
func (b *Builder) Rewrite(body, formattedBody string) string {
	text := stripReplyFallback(body)
	// Pills first: the plain body shows only the pill's visible text
	// (usually the bare display name), so rewrite its first occurrence.
	for _, p := range roster.Pills(formattedBody) {
		visible := strings.TrimPrefix(p.Text, "@")
		if visible == "" {
			continue
		}
		re := regexp.MustCompile(`(?i)(^|[^\pL\pN_])@?` + regexp.QuoteMeta(visible) + `:?`)
		if loc := re.FindStringSubmatchIndex(text); loc != nil {
			text = text[:loc[0]] + text[loc[2]:loc[3]] + "(to " + b.r.DisplayFor(p.MXID) + ")" + text[loc[1]:]
		}
	}
	for _, h := range b.handles {
		text = h.re.ReplaceAllString(text, "${1}(to "+escapeRepl(h.name)+")${2}")
	}
	text = b.anyMXIDs.ReplaceAllString(text, "${1}(to ${2})")
	return strings.TrimSpace(text)
}

func escapeRepl(s string) string { return strings.ReplaceAll(s, "$", "$$") }

// stripReplyFallback removes the "> <@x:y> quoted" block some clients prepend.
func stripReplyFallback(body string) string {
	if !strings.HasPrefix(body, "> ") {
		return body
	}
	lines := strings.Split(body, "\n")
	i := 0
	for i < len(lines) && strings.HasPrefix(lines[i], ">") {
		i++
	}
	if i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		return strings.Join(lines[i+1:], "\n")
	}
	return body
}

// Escape escapes text as Msg does a message's text.
func Escape(text string) string { return textEscaper.Replace(text) }

// Msg renders one message element.
func Msg(from, text string) string {
	return `<msg from="` + attrEscaper.Replace(from) + `">` + textEscaper.Replace(text) + `</msg>`
}

var (
	textEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	attrEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	// Unescape reverses both escapers (for text a model copies back).
	Unescape = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&amp;", "&")
)
