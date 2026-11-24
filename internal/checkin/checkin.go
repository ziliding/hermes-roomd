// Package checkin decides when a quiet room gets a check-in: after the room
// has been quiet for a while, one bot is asked to start something, so the
// group chat doesn't go dead while the user is away. It is pure scheduling
// (no I/O); the orchestrator (package room) does the asking.
package checkin

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
)

// Window is a daily span of local time, such as 23:00-07:00, when no
// check-in starts. It may wrap past midnight. The zero Window is empty.
type Window struct {
	start, end int // minutes since midnight; equal: empty
}

// ParseWindow parses "HH:MM-HH:MM". "" is the empty window.
func ParseWindow(s string) (Window, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Window{}, nil
	}
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return Window{}, fmt.Errorf("%q is not HH:MM-HH:MM", s)
	}
	start, err := clock(a)
	if err != nil {
		return Window{}, err
	}
	end, err := clock(b)
	if err != nil {
		return Window{}, err
	}
	if start == end {
		return Window{}, fmt.Errorf("%q starts and ends at the same time", s)
	}
	return Window{start: start, end: end}, nil
}

func clock(s string) (int, error) {
	s = strings.TrimSpace(s)
	h, m, ok := strings.Cut(s, ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || hh < 0 || hh > 24 || mm < 0 || mm > 59 || (hh == 24 && mm != 0) {
		return 0, fmt.Errorf("%q is not a time of day (HH:MM)", s)
	}
	return (hh*60 + mm) % (24 * 60), nil
}

// Empty reports whether the window never applies.
func (w Window) Empty() bool { return w.start == w.end }

// String renders the window as HH:MM-HH:MM ("" when empty).
func (w Window) String() string {
	if w.Empty() {
		return ""
	}
	return fmt.Sprintf("%02d:%02d-%02d:%02d", w.start/60, w.start%60, w.end/60, w.end%60)
}

// Contains reports whether t's wall-clock time (in t's location) is inside
// the window. The start is inside, the end is not.
func (w Window) Contains(t time.Time) bool {
	if w.Empty() {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	if w.start < w.end {
		return m >= w.start && m < w.end
	}
	return m >= w.start || m < w.end
}

// EndAfter is the first end of the window at or after t, in t's location.
func (w Window) EndAfter(t time.Time) time.Time {
	y, mo, d := t.Date()
	end := time.Date(y, mo, d, w.end/60, w.end%60, 0, 0, t.Location())
	if end.Before(t) {
		end = time.Date(y, mo, d+1, w.end/60, w.end%60, 0, 0, t.Location())
	}
	return end
}

// Schedule is when check-ins happen.
type Schedule struct {
	After  time.Duration // how long the room must be quiet
	Jitter time.Duration // After is moved by a random amount in [-Jitter, +Jitter]
	Quiet  Window        // no check-in starts inside it
	Loc    *time.Location
}

// Due is when a check-in falls due for a room quiet since anchor: After
// later, moved by a random amount within Jitter. A time inside the quiet
// hours moves to their end, plus a random delay of at most Jitter, so the
// bots don't all start right on the hour.
func (s Schedule) Due(anchor time.Time, r *rand.Rand) time.Time {
	t := anchor.Add(s.After + spread(r, -s.Jitter, s.Jitter))
	if s.QuietAt(t) {
		t = s.Quiet.EndAfter(t.In(s.loc())).Add(spread(r, 0, s.Jitter))
	}
	return t
}

// QuietAt reports whether t falls in the quiet hours.
func (s Schedule) QuietAt(t time.Time) bool { return s.Quiet.Contains(t.In(s.loc())) }

func (s Schedule) loc() *time.Location {
	if s.Loc == nil {
		return time.Local
	}
	return s.Loc
}

// spread is a random duration in [lo, hi], to the second.
func spread(r *rand.Rand, lo, hi time.Duration) time.Duration {
	n := int64((hi - lo) / time.Second)
	if n <= 0 || r == nil {
		return lo
	}
	return lo + time.Duration(r.Int64N(n+1))*time.Second
}

// Instruction is what the opening bot is told, after any new messages. It
// goes in the user message, never the system prompt, so the prompt cache
// stays warm. quiet is how long the room has been quiet; now is the local
// time, so the bot can say good morning rather than good night.
func Instruction(user string, quiet time.Duration, now time.Time) string {
	return "Nobody has written in the group chat for " + Ago(quiet) + ". It is now " + now.Format("Monday 15:04") + ". " +
		"If you feel like it, get the chat going again the way a friend would in a group chat: share a thought or something " +
		"you've been up to, or ask " + user + " or the others something. Keep it short and natural, and don't mention that " +
		"the chat was quiet or that you were asked to write. If nothing worth saying comes to mind, reply with [SILENT]."
}

// Ago renders a quiet spell in plain words: "about 3 hours", "45 minutes".
func Ago(d time.Duration) string {
	switch {
	case d < 90*time.Minute:
		m := max(1, int(d.Round(time.Minute)/time.Minute))
		if m == 1 {
			return "1 minute"
		}
		return strconv.Itoa(m) + " minutes"
	case d < 48*time.Hour:
		return "about " + strconv.Itoa(int(d.Round(time.Hour)/time.Hour)) + " hours"
	default:
		return "about " + strconv.Itoa(int(d.Round(24*time.Hour)/(24*time.Hour))) + " days"
	}
}
