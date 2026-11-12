package engine

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedules are intervals, not cron expressions. "Every 6 hours" or "every
// day at 02:00" covers what an inventory needs, reads the same on the page as
// in the database, and leaves no expression for somebody to get subtly wrong
// at three in the morning. Cron's extra power (weekdays only, the 1st of the
// month) buys nothing for reading versions off machines.

// MinEvery is the shortest interval a collector may be given. Anything more
// frequent is monitoring, which is a different tool's job, and a mistake here
// opens sessions into every host in the estate.
const MinEvery = 5 * time.Minute

// MaxEvery is the longest. Past a month the value is stale by any budget and
// "manual only" says what is meant more honestly.
const MaxEvery = 31 * 24 * time.Hour

// ParseEvery reads an interval such as 30m, 6h, 1d or 2d12h. Days are
// accepted because "24h" is how nobody says "daily".
func ParseEvery(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, errors.New("an interval is required, such as 30m, 6h or 1d")
	}
	var days int
	if d, rest, ok := strings.Cut(s, "d"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%q is not an interval; use something like 30m, 6h or 1d", s)
		}
		days = n
		s = rest
	}
	var dur time.Duration
	if s != "" {
		var err error
		dur, err = time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("%q is not an interval; use something like 30m, 6h or 1d", s)
		}
	}
	return time.Duration(days)*24*time.Hour + dur, nil
}

// FormatEvery writes an interval the way ParseEvery reads it, in the largest
// whole unit that fits.
func FormatEvery(d time.Duration) string {
	switch {
	case d <= 0:
		return "manual"
	case d%(24*time.Hour) == 0:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	case d%time.Hour == 0:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	default:
		return d.String()
	}
}

// Describe says a schedule in words, for lists.
func (s Schedule) Describe() string {
	if s.Manual() {
		return "manual only"
	}
	if s.Every == 24*time.Hour {
		if s.At != "" {
			return "daily at " + s.At
		}
		return "daily"
	}
	out := "every " + FormatEvery(s.Every)
	if s.At != "" {
		out += " at " + s.At
	}
	return out
}

// Check refuses a schedule the scheduler should not run.
func (s Schedule) Check() error {
	if s.Manual() {
		if s.At != "" {
			return errors.New("a manual-only collector has no time of day")
		}
		return nil
	}
	if s.Every < MinEvery {
		return fmt.Errorf("the shortest interval is %s", FormatEvery(MinEvery))
	}
	if s.Every > MaxEvery {
		return fmt.Errorf("the longest interval is %s; use manual only for anything rarer", FormatEvery(MaxEvery))
	}
	if s.At != "" {
		if s.Every%(24*time.Hour) != 0 {
			return errors.New("a time of day only goes with a whole number of days, such as 1d or 7d")
		}
		if _, _, err := parseClock(s.At); err != nil {
			return err
		}
	}
	return nil
}

func parseClock(at string) (h, m int, err error) {
	hs, ms, ok := strings.Cut(at, ":")
	if ok && len(hs) >= 1 && len(hs) <= 2 && len(ms) == 2 {
		h, e1 := strconv.Atoi(hs)
		m, e2 := strconv.Atoi(ms)
		if e1 == nil && e2 == nil && h >= 0 && h < 24 && m >= 0 && m < 60 {
			return h, m, nil
		}
	}
	return 0, 0, fmt.Errorf("%q is not a time of day; use HH:MM, such as 02:00", at)
}

// NormalizeClock writes a time of day as HH:MM.
func NormalizeClock(at string) (string, error) {
	at = strings.TrimSpace(at)
	if at == "" {
		return "", nil
	}
	h, m, err := parseClock(at)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%02d:%02d", h, m), nil
}

// NextDue is when a collector should next run, given when it last started,
// or the zero time for a manual-only collector.
//
// A collector that never ran is due now, unless it has a time of day, in
// which case it waits for the next one: somebody who chose 02:00 chose it to
// stay out of the working day. After a run the next one is a whole interval
// later. With a time of day it is that time on the day the interval comes
// round, skipping only a slot less than an hour after the last start: a run
// at 02:03 is followed by 02:00 tomorrow rather than 02:03, and a manual run
// in the evening does not cancel the night's run.
//
// A server that was down past a due time runs the collector once when it
// comes back, not once per missed interval.
func NextDue(s Schedule, last, now time.Time, loc *time.Location) time.Time {
	if s.Manual() {
		return time.Time{}
	}
	if loc == nil {
		loc = time.Local
	}
	if s.At == "" {
		if last.IsZero() {
			return now
		}
		return last.Add(s.Every)
	}
	h, m, err := parseClock(s.At)
	if err != nil {
		if last.IsZero() {
			return now
		}
		return last.Add(s.Every)
	}
	from := now
	if !last.IsZero() {
		from = last.Add(s.Every - 24*time.Hour + time.Hour)
	}
	return nextClock(from.In(loc), h, m)
}

// nextClock is the first h:m local time at or after t.
func nextClock(t time.Time, h, m int) time.Time {
	c := time.Date(t.Year(), t.Month(), t.Day(), h, m, 0, 0, t.Location())
	if c.Before(t) {
		c = time.Date(t.Year(), t.Month(), t.Day()+1, h, m, 0, 0, t.Location())
	}
	return c
}
