/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package schedule holds what AgentSchedule needs that is not Kubernetes: cron
// math, the labels and annotations a scheduled run carries, and placeholder
// substitution. The controller, the validating webhook and the API share it.
package schedule

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// Cron math for AgentSchedule. github.com/robfig/cron/v3 only parses and finds
// the next match; the rules that matter to a business user live here:
//
//   - only five-field expressions and @hourly @daily @weekly @monthly are
//     accepted (no seconds, no @every, no TZ= prefix: the zone is spec.timeZone);
//   - a wall-clock time that does not exist (a daylight-saving gap) never fires;
//   - a wall-clock time that occurs twice (an overlap) fires once, at its first
//     occurrence.
//
// Every function takes the time as an argument, so tests inject the clock.

// scheduleParser accepts what the CRD documents and nothing more.
var scheduleParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// maxTickScan bounds the ticks examined in one call so a very old cursor cannot
// spin the reconciler.
const maxTickScan = 20000

// Cron is a parsed expression in a time zone.
type Cron struct {
	spec cron.Schedule
	loc  *time.Location
	expr string
	zone string
}

// Parse parses expr and zone. An empty zone is UTC.
func Parse(expr, zone string) (*Cron, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, fmt.Errorf("schedule is empty")
	}
	upper := strings.ToUpper(expr)
	if strings.HasPrefix(upper, "TZ=") || strings.HasPrefix(upper, "CRON_TZ=") {
		return nil, fmt.Errorf("schedule must not carry a TZ= prefix: set spec.timeZone")
	}
	if strings.HasPrefix(expr, "@every") {
		return nil, fmt.Errorf("@every is not supported: use a five-field cron expression")
	}
	if strings.HasPrefix(expr, "@") {
		switch expr {
		case "@hourly", "@daily", "@weekly", "@monthly":
		default:
			return nil, fmt.Errorf("unsupported descriptor %q: use @hourly, @daily, @weekly or @monthly", expr)
		}
	} else if n := len(strings.Fields(expr)); n != 5 {
		return nil, fmt.Errorf("schedule needs five fields (minute hour day-of-month month day-of-week), got %d", n)
	}
	sp, err := scheduleParser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid schedule %q: %w", expr, err)
	}
	if zone == "" {
		zone = "UTC"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, fmt.Errorf("invalid time zone %q: %w", zone, err)
	}
	return &Cron{spec: sp, loc: loc, expr: expr, zone: zone}, nil
}

// Next returns the first tick strictly after t, or the zero time when there is
// none within the library's search window (five years).
func (c *Cron) Next(t time.Time) time.Time {
	t = t.In(c.loc)
	for range 64 {
		n := c.spec.Next(t)
		if n.IsZero() {
			return n
		}
		n = n.In(c.loc)
		if c.isRepeatedWallTime(n) {
			// The second occurrence of a wall-clock time in a daylight-saving
			// overlap: the first one already fired.
			t = n
			continue
		}
		return n
	}
	return time.Time{}
}

// isRepeatedWallTime reports whether the same wall-clock reading occurred
// earlier because the clocks were set back (the second pass through an overlap).
func (c *Cron) isRepeatedWallTime(t time.Time) bool {
	_, off := t.Zone()
	_, before := t.Add(-3 * time.Hour).Zone()
	delta := before - off
	if delta <= 0 {
		return false
	}
	e := t.Add(-time.Duration(delta) * time.Second)
	_, eo := e.Zone()
	if eo != before {
		return false
	}
	ey, em, ed := e.Date()
	ty, tm, td := t.Date()
	return ey == ty && em == tm && ed == td && e.Hour() == t.Hour() && e.Minute() == t.Minute()
}

// NextN returns up to n ticks after t.
func (c *Cron) NextN(t time.Time, n int) []time.Time {
	var out []time.Time
	for range n {
		t = c.Next(t)
		if t.IsZero() {
			break
		}
		out = append(out, t)
	}
	return out
}

// MinInterval returns the shortest gap between consecutive ticks in the next
// year, sampled from t. It is how the webhook refuses "* * * * *": two
// consecutive firings are not enough, since "0,1 * * * *" has a long gap too.
func (c *Cron) MinInterval(t time.Time) time.Duration {
	const samples = 400
	min := time.Duration(0)
	prev := c.Next(t)
	for range samples {
		if prev.IsZero() {
			break
		}
		n := c.Next(prev)
		if n.IsZero() {
			break
		}
		if d := n.Sub(prev); min == 0 || d < min {
			min = d
		}
		prev = n
	}
	return min
}

// TickPlan is what the controller does about the ticks since the cursor.
type TickPlan struct {
	// Due is the latest tick in (after, now] that is still inside the deadline;
	// zero when there is none.
	Due time.Time
	// Missed is a tick that fell in (after, now] but is older than the deadline.
	// Zero when none was missed. It is set only when Due is zero.
	Missed time.Time
	// Next is the first tick after now.
	Next time.Time
}

// Plan finds what to do at now given the cursor after (the last tick
// handled, or the time the schedule's spec last changed). Only the latest tick
// is ever started, however many were missed; one older than deadline is
// reported as missed instead.
func (c *Cron) Plan(after, now time.Time, deadline time.Duration) TickPlan {
	var res TickPlan
	// Ticks older than the deadline cannot start, so scanning from just inside
	// it keeps the work bounded however long the cursor has been idle.
	start := after
	if floor := now.Add(-deadline).Add(-time.Nanosecond); floor.After(start) {
		start = floor
	}
	last := time.Time{}
	t := c.Next(start)
	for i := 0; !t.IsZero() && !t.After(now) && i < maxTickScan; i++ {
		last = t
		t = c.Next(t)
	}
	res.Next = t
	if !last.IsZero() {
		res.Due = last
		return res
	}
	// Nothing inside the deadline: was something skipped before it?
	if start.After(after) {
		if m := c.Next(after); !m.IsZero() && !m.After(start) {
			res.Missed = m
		}
	}
	return res
}

// Location is the time zone the expression is read in.
func (c *Cron) Location() *time.Location { return c.loc }
