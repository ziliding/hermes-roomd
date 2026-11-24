package checkin

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

func at(hhmm string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", "2026-11-24 "+hhmm, time.UTC)
	if err != nil {
		panic(err)
	}
	return t
}

func TestParseWindow(t *testing.T) {
	for _, s := range []string{"", "23:00-07:00", "1:30 - 6:00", "22:00-24:00"} {
		if _, err := ParseWindow(s); err != nil {
			t.Errorf("ParseWindow(%q): %v", s, err)
		}
	}
	for _, s := range []string{"23:00", "23-07", "25:00-07:00", "07:60-08:00", "07:00-07:00", "00:00-24:00", "x:00-07:00"} {
		if _, err := ParseWindow(s); err == nil {
			t.Errorf("ParseWindow(%q) accepted", s)
		}
	}
	if w, _ := ParseWindow("1:30 - 6:00"); w.String() != "01:30-06:00" {
		t.Errorf("String() = %q", w.String())
	}
}

func TestWindowContains(t *testing.T) {
	night, _ := ParseWindow("23:00-07:00")
	day, _ := ParseWindow("09:00-17:00")
	tests := []struct {
		w    Window
		at   string
		want bool
	}{
		{night, "22:59", false}, {night, "23:00", true}, {night, "03:00", true}, {night, "06:59", true}, {night, "07:00", false}, {night, "12:00", false},
		{day, "08:59", false}, {day, "09:00", true}, {day, "16:59", true}, {day, "17:00", false},
		{Window{}, "03:00", false},
	}
	for _, tt := range tests {
		if got := tt.w.Contains(at(tt.at)); got != tt.want {
			t.Errorf("%s contains %s = %v, want %v", tt.w, tt.at, got, tt.want)
		}
	}
}

func TestWindowEndAfter(t *testing.T) {
	night, _ := ParseWindow("23:00-07:00")
	if got, want := night.EndAfter(at("23:30")), at("07:00").AddDate(0, 0, 1); !got.Equal(want) {
		t.Errorf("before midnight: %v, want %v", got, want)
	}
	if got, want := night.EndAfter(at("02:00")), at("07:00"); !got.Equal(want) {
		t.Errorf("after midnight: %v, want %v", got, want)
	}
}

func TestDue(t *testing.T) {
	night, _ := ParseWindow("23:00-07:00")
	s := Schedule{After: 3 * time.Hour, Quiet: night, Loc: time.UTC}
	if got, want := s.Due(at("10:00"), nil), at("13:00"); !got.Equal(want) {
		t.Errorf("plain: %v, want %v", got, want)
	}
	if got, want := s.Due(at("21:00"), nil), at("07:00").AddDate(0, 0, 1); !got.Equal(want) {
		t.Errorf("into quiet hours: %v, want the end of them, %v", got, want)
	}

	s.Jitter = 30 * time.Minute
	r := rand.New(rand.NewPCG(1, 2))
	seen := map[bool]bool{}
	for range 200 {
		got := s.Due(at("10:00"), r)
		if got.Before(at("12:30")) || got.After(at("13:30")) {
			t.Fatalf("jittered %v outside 12:30-13:30", got)
		}
		seen[got.Before(at("13:00"))] = true
		late := s.Due(at("21:00"), r)
		if end := at("07:00").AddDate(0, 0, 1); late.Before(end) || late.After(end.Add(30*time.Minute)) {
			t.Fatalf("after quiet hours %v, want 07:00-07:30", late)
		}
	}
	if !seen[true] || !seen[false] {
		t.Error("jitter never moved the check-in both earlier and later")
	}
}

func TestDueInTimezone(t *testing.T) {
	tokyo := time.FixedZone("JST", 9*3600)
	night, _ := ParseWindow("23:00-07:00")
	s := Schedule{After: time.Hour, Quiet: night, Loc: tokyo}
	// 14:30 UTC is 23:30 in Tokyo: quiet there, so it waits until 07:00 JST (22:00 UTC).
	if got, want := s.Due(at("13:30"), nil), at("22:00"); !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if s.QuietAt(at("12:00")) || !s.QuietAt(at("15:00")) {
		t.Error("quiet hours not read in the schedule's timezone")
	}
}

func TestInstructionAndAgo(t *testing.T) {
	got := Instruction("Alex", 3*time.Hour+10*time.Minute, at("09:14"))
	for _, want := range []string{"for about 3 hours", "Tuesday 09:14", "ask Alex", "[SILENT]"} {
		if !strings.Contains(got, want) {
			t.Errorf("instruction lacks %q: %s", want, got)
		}
	}
	for d, want := range map[time.Duration]string{time.Minute: "1 minute", 45 * time.Minute: "45 minutes", 2 * time.Hour: "about 2 hours", 72 * time.Hour: "about 3 days"} {
		if got := Ago(d); got != want {
			t.Errorf("Ago(%v) = %q, want %q", d, got, want)
		}
	}
}
