// Package roster knows who is in a group room: the user and the bots, their
// Matrix ids and display names. It answers "who is this?" and "who is this
// message addressed to?" for contextbuild and turns.
package roster

import (
	"html"
	"regexp"
	"strings"
)

// UserKey is the participant key of the human user.
const UserKey = "user"

// Person is one participant.
type Person struct {
	Key     string // bot key, or UserKey
	Display string
	MXID    string
	IsBot   bool

	nameRe *regexp.Regexp // display name / localpart / mxid, with optional leading @
}

// Roster is an immutable set of participants.
type Roster struct {
	people []*Person // user first, then bots in config order
	byMXID map[string]*Person
	byKey  map[string]*Person
}

// New builds a roster. bots must be in a stable order.
func New(userMXID, userDisplay string, bots []Person) *Roster {
	r := &Roster{byMXID: map[string]*Person{}, byKey: map[string]*Person{}}
	add := func(p Person) {
		p.nameRe = nameRegexp(p)
		pp := &p
		r.people = append(r.people, pp)
		r.byMXID[p.MXID] = pp
		r.byKey[p.Key] = pp
	}
	add(Person{Key: UserKey, Display: userDisplay, MXID: userMXID})
	for _, b := range bots {
		b.IsBot = true
		add(b)
	}
	return r
}

// ByMXID returns the participant with that Matrix id, if known.
func (r *Roster) ByMXID(mxid string) (*Person, bool) { p, ok := r.byMXID[mxid]; return p, ok }

// ByKey returns the participant with that key, if known.
func (r *Roster) ByKey(key string) (*Person, bool) { p, ok := r.byKey[key]; return p, ok }

// Bots returns the bots in stable order.
func (r *Roster) Bots() []*Person { return r.people[1:] }

// User returns the human user.
func (r *Roster) User() *Person { return r.people[0] }

// DisplayFor names a sender: the roster display name, else the mxid localpart.
func (r *Roster) DisplayFor(mxid string) string {
	if p, ok := r.byMXID[mxid]; ok {
		return p.Display
	}
	return Localpart(mxid)
}

// Localpart returns "ada" for "@ada:server".
func Localpart(mxid string) string {
	s := strings.TrimPrefix(mxid, "@")
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	return s
}

// Addressee returns the key of the single bot a message is addressed to, or
// "" when it names none. At most one addressee is ever counted: the bot whose
// name or handle appears earliest in the body; bots only present in
// m.mentions / pills rank after any named in the text, in list order. The
// sender (exclude) never counts, so a bot naming itself addresses nobody.
func (r *Roster) Addressee(body, formattedBody string, mentions []string, exclude string) string {
	const notInText = int(^uint(0) >> 1)
	best, bestPos := "", notInText
	hinted := map[string]int{} // key -> order among explicit mentions
	for i, id := range append(append([]string{}, mentions...), PillTargets(formattedBody)...) {
		if p, ok := r.byMXID[id]; ok && p.IsBot {
			if _, seen := hinted[p.Key]; !seen {
				hinted[p.Key] = i
			}
		}
	}
	for _, p := range r.Bots() {
		if p.Key == exclude {
			continue
		}
		pos := notInText
		if loc := p.nameRe.FindStringIndex(body); loc != nil {
			pos = loc[0]
		} else if order, ok := hinted[p.Key]; ok {
			pos = notInText - 1000 + order // explicit mention not visible in body text
		}
		if pos < bestPos {
			best, bestPos = p.Key, pos
		}
	}
	return best
}

// NamePos returns the byte offset in body where participant key is first
// named (display name, localpart or mxid, optional "@"), or -1.
func (r *Roster) NamePos(key, body string) int {
	p, ok := r.byKey[key]
	if !ok {
		return -1
	}
	if loc := p.nameRe.FindStringIndex(body); loc != nil {
		return loc[0]
	}
	return -1
}

// Pill is a matrix.to user link found in formatted_body.
type Pill struct {
	MXID string
	Text string // the link's visible text (what the plain body usually shows)
}

var pillRe = regexp.MustCompile(`(?i)<a\s[^>]*href=["']https://matrix\.to/#/(@[^"'?/>]+)[^"']*["'][^>]*>(.*?)</a>`)
var tagRe = regexp.MustCompile(`<[^>]*>`)

// Pills extracts user pills from an HTML formatted_body, in order.
func Pills(formattedBody string) []Pill {
	var out []Pill
	for _, m := range pillRe.FindAllStringSubmatch(formattedBody, -1) {
		id := unescapeMXID(m[1])
		text := strings.TrimSpace(html.UnescapeString(tagRe.ReplaceAllString(m[2], "")))
		out = append(out, Pill{MXID: id, Text: text})
	}
	return out
}

// PillTargets is the mxids of Pills(formattedBody).
func PillTargets(formattedBody string) []string {
	var ids []string
	for _, p := range Pills(formattedBody) {
		ids = append(ids, p.MXID)
	}
	return ids
}

func unescapeMXID(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "%3A", ":"), "%3a", ":")
}

// NameRegexp matches a participant's display name, localpart or full mxid as a
// whole word, with an optional leading "@" (case-insensitive).
func nameRegexp(p Person) *regexp.Regexp {
	alts := []string{regexp.QuoteMeta(p.MXID), "@?" + regexp.QuoteMeta(p.Display)}
	if lp := Localpart(p.MXID); !strings.EqualFold(lp, p.Display) {
		alts = append(alts, "@?"+regexp.QuoteMeta(lp))
	}
	return regexp.MustCompile(`(?i)(?:^|[^\pL\pN_@])(?:` + strings.Join(alts, "|") + `)(?:[^\pL\pN_]|$)`)
}
