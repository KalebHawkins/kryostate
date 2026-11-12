package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/KalebHawkins/kryostate/internal/store"
)

// BackoffText says why the schedule is leaving a host alone, such as
// "skipped by the schedule until 14:20 after 3 failed attempts: connection
// refused", or "" once the backoff has passed. A collection started by hand
// is not held back by it.
func BackoffText(b store.HostBackoff, now time.Time, loc *time.Location) string {
	if b.Fails == 0 || !now.Before(b.Until) {
		return ""
	}
	if loc == nil {
		loc = time.Local
	}
	until := b.Until.In(loc)
	when := until.Format("15:04")
	if now.In(loc).Format(time.DateOnly) != until.Format(time.DateOnly) {
		when = until.Format("Mon 2 Jan 15:04")
	}
	attempts := "attempts"
	if b.Fails == 1 {
		attempts = "attempt"
	}
	s := fmt.Sprintf("skipped by the schedule until %s after %d failed %s", when, b.Fails, attempts)
	if e := strings.Join(strings.Fields(b.LastError), " "); e != "" {
		s += ": " + e
	}
	return s
}
