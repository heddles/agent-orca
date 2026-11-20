package schedule

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, expr, zone string) *Cron {
	t.Helper()
	c, err := Parse(expr, zone)
	if err != nil {
		t.Fatalf("Parse(%q, %q): %v", expr, zone, err)
	}
	return c
}

func at(t *testing.T, zone, v string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	ts, err := time.ParseInLocation(stamp, v, loc)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

const stamp = "2006-01-02 15:04"

func TestParseRejects(t *testing.T) {
	for _, tc := range []struct{ expr, zone, want string }{
		{"", "UTC", "empty"},
		{"* * * *", "UTC", "five fields"},
		{"0 * * * * *", "UTC", "five fields"},
		{"@every 1m", "UTC", "@every"},
		{"@yearly", "UTC", "unsupported descriptor"},
		{"TZ=Europe/Berlin 0 8 * * *", "UTC", "TZ="},
		{"cron_tz=UTC 0 8 * * *", "UTC", "TZ="},
		{"60 * * * *", "UTC", "invalid schedule"},
		{"0 8 * * *", "Mars/Olympus", "time zone"},
	} {
		_, err := Parse(tc.expr, tc.zone)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Parse(%q, %q) = %v, want an error containing %q", tc.expr, tc.zone, err, tc.want)
		}
	}
}

func TestParseAccepts(t *testing.T) {
	for _, expr := range []string{"*/5 * * * *", "0 8 * * 1-5", "@hourly", "@daily", "@weekly", "@monthly", "30 2 29 2 *"} {
		if _, err := Parse(expr, ""); err != nil {
			t.Errorf("Parse(%q): %v", expr, err)
		}
	}
}

func TestNextTable(t *testing.T) {
	for _, tc := range []struct {
		name, expr, zone, after, want string
	}{
		{"weekday 8am skips the weekend", "0 8 * * 1-5", "Europe/Berlin", "2026-11-13 09:00", "2026-11-16 08:00"}, // Fri -> Mon
		{"@weekly is Sunday midnight", "@weekly", "UTC", "2026-11-18 12:00", "2026-11-22 00:00"},
		{"@monthly is the first", "@monthly", "UTC", "2026-11-18 12:00", "2026-12-01 00:00"},
		{"day 31 skips short months", "0 0 31 * *", "UTC", "2026-11-01 00:00", "2026-12-31 00:00"},
		{"february 29 waits for the leap year", "0 0 29 2 *", "UTC", "2026-03-01 00:00", "2028-02-29 00:00"},
		{"year end", "59 23 31 12 *", "UTC", "2026-12-31 23:59", "2027-12-31 23:59"},
		{"same minute is not after", "30 9 * * *", "UTC", "2026-11-18 09:30", "2026-11-19 09:30"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := mustParse(t, tc.expr, tc.zone)
			got := c.Next(at(t, tc.zone, tc.after))
			if want := at(t, tc.zone, tc.want); !got.Equal(want) {
				t.Fatalf("Next = %s, want %s", got.In(c.Location()), want)
			}
		})
	}
}

func TestZoneChangesTheInstant(t *testing.T) {
	after := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	utc := mustParse(t, "0 8 * * *", "UTC").Next(after)
	tokyo := mustParse(t, "0 8 * * *", "Asia/Tokyo").Next(after)
	if utc.Equal(tokyo) || tokyo.Sub(utc) != -9*time.Hour {
		t.Fatalf("08:00 UTC = %s, 08:00 Tokyo = %s", utc, tokyo)
	}
}

func TestDaylightSavingGapIsSkipped(t *testing.T) {
	// New York, 8 March 2026: 02:00 jumps to 03:00, so 02:30 does not exist.
	c := mustParse(t, "30 2 * * *", "America/New_York")
	got := c.NextN(at(t, "America/New_York", "2026-03-07 00:00"), 3)
	want := []time.Time{
		at(t, "America/New_York", "2026-03-07 02:30"),
		at(t, "America/New_York", "2026-03-09 02:30"), // the 8th has no 02:30
		at(t, "America/New_York", "2026-03-10 02:30"),
	}
	for i := range want {
		if i >= len(got) || !got[i].Equal(want[i]) {
			t.Fatalf("ticks = %v, want %v", got, want)
		}
	}
	// Berlin, 29 March 2026: 02:00 jumps to 03:00.
	b := mustParse(t, "30 2 * * *", "Europe/Berlin")
	got = b.NextN(at(t, "Europe/Berlin", "2026-03-28 03:00"), 1)
	if want := at(t, "Europe/Berlin", "2026-03-30 02:30"); len(got) != 1 || !got[0].Equal(want) {
		t.Fatalf("Berlin ticks = %v, want %v", got, want)
	}
}

func TestDaylightSavingOverlapFiresOnce(t *testing.T) {
	// New York, 1 November 2026: 02:00 EDT falls back to 01:00 EST, so 01:30
	// happens twice (05:30Z and 06:30Z). Only the first fires.
	c := mustParse(t, "30 1 * * *", "America/New_York")
	got := c.NextN(time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), 3)
	want := []time.Time{
		time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC),
		time.Date(2026, 11, 2, 6, 30, 0, 0, time.UTC), // 01:30 EST on the 2nd
		time.Date(2026, 11, 3, 6, 30, 0, 0, time.UTC),
	}
	if len(got) != 3 {
		t.Fatalf("ticks = %v", got)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("tick %d = %s, want %s (all: %v)", i, got[i].UTC(), want[i], got)
		}
	}
	// Berlin, 25 October 2026: 03:00 CEST falls back to 02:00 CET; 02:30 happens twice.
	b := mustParse(t, "30 2 * * *", "Europe/Berlin")
	bt := b.NextN(time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC), 2)
	if want := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC); len(bt) != 2 || !bt[0].Equal(want) {
		t.Fatalf("Berlin ticks = %v", bt)
	}
	if want := time.Date(2026, 10, 26, 1, 30, 0, 0, time.UTC); !bt[1].Equal(want) {
		t.Fatalf("Berlin second tick = %s, want %s", bt[1].UTC(), want)
	}
}

func TestNonOverlappingWallTimesAreUntouchedByTheFallBack(t *testing.T) {
	// 03:30 happens once on the day clocks go back; it must still fire.
	c := mustParse(t, "30 3 * * *", "America/New_York")
	got := c.Next(time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC))
	if want := at(t, "America/New_York", "2026-11-01 03:30"); !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got, want)
	}
}

func TestMinInterval(t *testing.T) {
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		expr string
		want time.Duration
	}{
		{"* * * * *", time.Minute},
		{"*/5 * * * *", 5 * time.Minute},
		{"*/2 * * * *", 2 * time.Minute},
		{"0,1 * * * *", time.Minute},
		{"0 * * * *", time.Hour},
		{"0 8 * * 1-5", 24 * time.Hour},
		{"@weekly", 7 * 24 * time.Hour},
	} {
		if got := mustParse(t, tc.expr, "UTC").MinInterval(now); got != tc.want {
			t.Errorf("MinInterval(%q) = %s, want %s", tc.expr, got, tc.want)
		}
	}
}

func TestPlan(t *testing.T) {
	c := mustParse(t, "0 * * * *", "UTC")
	base := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	hour := time.Hour
	for _, tc := range []struct {
		name          string
		after, now    time.Time
		deadline      time.Duration
		due, miss, nx time.Time
	}{
		{
			name: "nothing due yet", after: base, now: base.Add(30 * time.Minute), deadline: hour,
			nx: base.Add(hour),
		},
		{
			name: "a tick is due", after: base, now: base.Add(hour + 10*time.Second), deadline: hour,
			due: base.Add(hour), nx: base.Add(2 * hour),
		},
		{
			name: "several missed: only the latest, within the deadline", after: base, now: base.Add(3*hour + time.Minute), deadline: 2 * hour,
			due: base.Add(3 * hour), nx: base.Add(4 * hour),
		},
		{
			name: "exactly at the deadline still starts", after: base, now: base.Add(2 * hour), deadline: hour,
			due: base.Add(2 * hour), nx: base.Add(3 * hour),
		},
		{
			name: "too late is a missed tick", after: base, now: base.Add(hour + 30*time.Minute), deadline: 10 * time.Minute,
			miss: base.Add(hour), nx: base.Add(2 * hour),
		},
		{
			name: "a very old cursor does not scan the past", after: base.Add(-24 * 365 * hour), now: base.Add(10 * time.Minute), deadline: hour,
			due: base, nx: base.Add(hour),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := c.Plan(tc.after, tc.now, tc.deadline)
			if !p.Due.Equal(tc.due) || !p.Missed.Equal(tc.miss) || !p.Next.Equal(tc.nx) {
				t.Fatalf("Plan = {due %v, missed %v, next %v}, want {%v, %v, %v}", p.Due, p.Missed, p.Next, tc.due, tc.miss, tc.nx)
			}
		})
	}
}

func TestSubstituteIsSinglePass(t *testing.T) {
	tick := time.Date(2026, 11, 16, 8, 0, 0, 0, time.FixedZone("CET", 3600))
	got := Substitute("Digest for {{schedule.date}} at {{ schedule.time }} by {{schedule.name}}; {{params.x}} {{steps.a.output}}", "weekday-digest", tick)
	want := "Digest for 2026-11-16 at 2026-11-16T08:00:00+01:00 by weekday-digest; {{params.x}} {{steps.a.output}}"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// A schedule name cannot smuggle in another placeholder.
	if got := Substitute("{{schedule.name}}", "{{schedule.date}}", tick); got != "{{schedule.date}}" {
		t.Fatalf("substitution recursed: %q", got)
	}
}

func TestRunNameIsStablePerTick(t *testing.T) {
	tick := time.Date(2026, 11, 16, 7, 0, 0, 0, time.UTC)
	a, b := RunName("digest", tick), RunName("digest", tick.In(time.FixedZone("x", 7200)))
	if a != b || a != fmt.Sprintf("digest-%d", tick.Unix()) {
		t.Fatalf("RunName = %q / %q", a, b)
	}
}
