package config

import (
	"strings"
	"testing"
	"time"
)

func TestCheckInDefaultsOff(t *testing.T) {
	c, err := Parse([]byte(threeBots+"rooms: [\"!a:example.com\"]\n"), env(threeEnv))
	if err != nil {
		t.Fatal(err)
	}
	if c.Rooms[0].CheckInConfig.Enabled {
		t.Fatal("check-ins are on without check_in in the config")
	}
}

func TestCheckInTopLevelAndRoomOverrides(t *testing.T) {
	y := threeBots + `
check_in:
  enabled: true
  after: 3h
  jitter: 30m
  quiet_hours: "23:00-07:00"
  timezone: Asia/Shanghai
rooms:
  - "!plain:example.com"
  - id: "!lounge:example.com"
    check_in: { after: 90m, max_unanswered: 0 }
  - id: "!work:example.com"
    check_in: { enabled: false }
`
	c, err := Parse([]byte(y), env(threeEnv))
	if err != nil {
		t.Fatal(err)
	}
	plain, lounge, work := c.Rooms[0].CheckInConfig, c.Rooms[1].CheckInConfig, c.Rooms[2].CheckInConfig
	if !plain.Enabled || plain.Schedule.After != 3*time.Hour || plain.Schedule.Jitter != 30*time.Minute ||
		plain.Schedule.Quiet.String() != "23:00-07:00" || plain.Schedule.Loc.String() != "Asia/Shanghai" ||
		plain.MaxUnanswered != DefaultCheckInMaxUnanswered {
		t.Fatalf("plain room: %+v", plain)
	}
	if !lounge.Enabled || lounge.Schedule.After != 90*time.Minute || lounge.Schedule.Jitter != 30*time.Minute ||
		lounge.Schedule.Quiet.String() != "23:00-07:00" || lounge.MaxUnanswered != 0 {
		t.Fatalf("lounge overrides after and max_unanswered, inherits the rest: %+v", lounge)
	}
	if work.Enabled {
		t.Fatalf("work room turned check-ins off: %+v", work)
	}
}

func TestCheckInOnlyInOneRoom(t *testing.T) {
	y := threeBots + `
rooms:
  - "!plain:example.com"
  - id: "!lounge:example.com"
    check_in: { enabled: true }
`
	c, err := Parse([]byte(y), env(threeEnv))
	if err != nil {
		t.Fatal(err)
	}
	if c.Rooms[0].CheckInConfig.Enabled {
		t.Fatal("plain room inherited check-ins it never enabled")
	}
	if l := c.Rooms[1].CheckInConfig; !l.Enabled || l.Schedule.After != DefaultCheckInAfter || !l.Schedule.Quiet.Empty() || l.Schedule.Loc != time.Local {
		t.Fatalf("lounge defaults: %+v", l)
	}
}

func TestCheckInRejects(t *testing.T) {
	tests := []struct{ name, yaml, wantErr string }{
		{"too short", "check_in: { enabled: true, after: 3m }", "check_in.after must be at least"},
		{"jitter too big", "check_in: { enabled: true, after: 1h, jitter: 1h }", "check_in.jitter"},
		{"bad quiet hours", `check_in: { enabled: true, quiet_hours: "11pm-7am" }`, "check_in.quiet_hours"},
		{"bad timezone", "check_in: { enabled: true, timezone: Mars/Olympus }", "check_in.timezone"},
		{"negative max", "check_in: { enabled: true, max_unanswered: -1 }", "check_in.max_unanswered"},
		{"unknown key", "check_in: { enabled: true, every: 1h }", "field every not found"},
		{"room override", "rooms:\n  - id: \"!b:example.com\"\n    check_in: { enabled: true, after: 1m }", "room !b:example.com: check_in.after"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			y := threeBots + tt.yaml + "\n"
			if !strings.Contains(tt.yaml, "rooms:") {
				y += "rooms: [\"!a:example.com\"]\n"
			}
			_, err := Parse([]byte(y), env(threeEnv))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
	// Off: nothing is checked.
	if _, err := Parse([]byte(threeBots+"check_in: { enabled: false, after: 1m }\nrooms: [\"!a:example.com\"]\n"), env(threeEnv)); err != nil {
		t.Fatalf("disabled check_in was validated: %v", err)
	}
}
