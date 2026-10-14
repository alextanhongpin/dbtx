package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Spec computes the ticks of a schedule.
type Spec interface {
	// Next returns the first tick strictly after t.
	Next(t time.Time) time.Time
}

// Parse accepts a five-field cron expression (minute hour day-of-month month
// day-of-week; ranges, lists and steps; day-of-week 0-7 with 0 and 7 Sunday),
// the descriptors @hourly, @daily, @midnight, @weekly, @monthly, @yearly and
// @annually, or "@every <duration>" (e.g. "@every 15m"). Cron ticks are
// computed in loc.
func Parse(spec string, loc *time.Location) (Spec, error) {
	spec = strings.TrimSpace(spec)
	if loc == nil {
		loc = time.UTC
	}
	if d, ok := strings.CutPrefix(spec, "@every "); ok {
		dur, err := time.ParseDuration(strings.TrimSpace(d))
		if err != nil || dur < time.Second {
			return nil, fmt.Errorf("schedule: @every needs a duration >= 1s: %q", spec)
		}
		return every(dur), nil
	}
	switch spec {
	case "@yearly", "@annually":
		spec = "0 0 1 1 *"
	case "@monthly":
		spec = "0 0 1 * *"
	case "@weekly":
		spec = "0 0 * * 0"
	case "@daily", "@midnight":
		spec = "0 0 * * *"
	case "@hourly":
		spec = "0 * * * *"
	}
	f := strings.Fields(spec)
	if len(f) != 5 {
		return nil, fmt.Errorf("schedule: want 5 cron fields, got %d in %q", len(f), spec)
	}
	c := &cron{loc: loc}
	var err error
	if c.minute, err = field(f[0], 0, 59); err != nil {
		return nil, err
	}
	if c.hour, err = field(f[1], 0, 23); err != nil {
		return nil, err
	}
	if c.dom, err = field(f[2], 1, 31); err != nil {
		return nil, err
	}
	if c.month, err = field(f[3], 1, 12); err != nil {
		return nil, err
	}
	if c.dow, err = field(f[4], 0, 7); err != nil {
		return nil, err
	}
	if c.dow&(1<<7) != 0 {
		c.dow |= 1
	}
	c.domStar, c.dowStar = f[2] == "*" || f[2] == "?", f[4] == "*" || f[4] == "?"
	return c, nil
}

type every time.Duration

func (e every) Next(t time.Time) time.Time { return t.Add(time.Duration(e)) }

type cron struct {
	minute, hour, dom, month, dow uint64
	domStar, dowStar              bool
	loc                           *time.Location
}

func field(s string, lo, hi int) (uint64, error) {
	var bits uint64
	for part := range strings.SplitSeq(s, ",") {
		rng, stepS, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepS)
			if err != nil || n < 1 {
				return 0, fmt.Errorf("schedule: bad step in %q", part)
			}
			step = n
		}
		start, end := lo, hi
		if rng != "*" && rng != "?" {
			a, b, isRange := strings.Cut(rng, "-")
			x, err := strconv.Atoi(a)
			if err != nil {
				return 0, fmt.Errorf("schedule: bad value in %q", part)
			}
			start, end = x, x
			if isRange {
				if end, err = strconv.Atoi(b); err != nil {
					return 0, fmt.Errorf("schedule: bad range in %q", part)
				}
			} else if hasStep {
				end = hi
			}
		}
		if start < lo || end > hi || start > end {
			return 0, fmt.Errorf("schedule: %q out of range %d-%d", part, lo, hi)
		}
		for v := start; v <= end; v += step {
			bits |= 1 << v
		}
	}
	return bits, nil
}

func (c *cron) dayMatches(t time.Time) bool {
	dom := c.dom&(1<<t.Day()) != 0
	dow := c.dow&(1<<int(t.Weekday())) != 0
	if c.domStar || c.dowStar {
		return dom && dow
	}
	return dom || dow // classic cron: either restricted field may match
}

func (c *cron) Next(t time.Time) time.Time {
	t = t.In(c.loc).Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		switch {
		case c.month&(1<<int(t.Month())) == 0:
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, c.loc)
		case !c.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, c.loc)
		case c.hour&(1<<t.Hour()) == 0:
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, c.loc)
		case c.minute&(1<<t.Minute()) == 0:
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{} // no tick within five years (e.g. 30 February)
}
