package web

import (
	"fmt"
	"strings"
	"time"
)

// ParseAt reads the moment an "as of" view is asked for, in local time. A
// date alone means the end of that day, so "values on 1 Mar" includes what
// was reported during 1 Mar; a date and time (2026-03-01T15:04, or with
// seconds) means that minute.
func ParseAt(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if d, err := time.ParseInLocation(time.DateOnly, s, time.Local); err == nil {
		return d.AddDate(0, 0, 1).Add(-time.Nanosecond), nil
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02 15:04", time.RFC3339} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a date: use 2026-03-01 or 2026-03-01T15:04", s)
}

// asOf is what the host page needs to show its Facts as they were.
type asOf struct {
	// Input is what the date box holds: the date asked about, or empty.
	Input string
	// Label names the moment in the banner, such as "1 Mar 2026".
	Label string
}

func asOfFrom(q string, at time.Time) asOf {
	label := at.Format("2 Jan 2006")
	if strings.Contains(q, "T") || strings.Contains(q, " ") {
		label = at.Format("2 Jan 2006 15:04")
	}
	return asOf{Input: at.Format(time.DateOnly), Label: label}
}
