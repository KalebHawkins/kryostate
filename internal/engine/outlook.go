package engine

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// The outlook is the schedule seen from the estate's side: when collectors
// run, which of them run close together, and how many sessions that adds up
// to. Its point is to make a storm visible before it happens, when many
// collectors fall due in the same few minutes and the scheduler's session
// limit turns them into a long queue against the same hosts.

const (
	// ClusterWindow is how close together runs must start to be counted as
	// one cluster: every run starting within this long of the cluster's
	// first.
	ClusterWindow = 15 * time.Minute
	// StormSessionFactor flags a cluster as busy when its sessions are more
	// than this many times the estate's session limit.
	StormSessionFactor = 10
	// StormCollectors flags a cluster as busy when this many collectors or
	// more run in it, and between them they need more sessions than the
	// estate's limit allows at once.
	StormCollectors = 4
	// DefaultSessionTime is how long one host's session is assumed to take
	// when no scheduled run has finished yet to measure.
	DefaultSessionTime = 15 * time.Second
	// OutlookHours is how far ahead the timeline looks.
	OutlookHours = 24
	// outlookDays is how far ahead later runs of a collector are counted,
	// so that a weekly collector three days out still meets the daily ones
	// running beside it. Runs of collectors more often than hourly are only
	// counted for the first day; they are everywhere anyway.
	outlookDays = 7
)

// Occurrence is one run of one collector.
type Occurrence struct {
	Key   string
	Title string
	At    time.Time
	Every time.Duration
	// First is the collector's next run, rather than a later one.
	First bool
}

// Occurrences lists when each scheduled collector in plan runs from now: its
// next run, wherever that falls, and the runs after it for some days ahead.
// A collector that is overdue is counted as running now.
func Occurrences(plan []Planned, now time.Time, loc *time.Location) []Occurrence {
	var out []Occurrence
	for _, p := range plan {
		if p.Schedule.Manual() || p.Next.IsZero() {
			continue
		}
		end := now.Add(outlookDays * 24 * time.Hour)
		if p.Schedule.Every < time.Hour {
			end = now.Add(OutlookHours * time.Hour)
		}
		t := p.Next
		if t.Before(now) {
			t = now
		}
		o := Occurrence{Key: p.Key, Title: p.Title, At: t, Every: p.Schedule.Every, First: true}
		out = append(out, o)
		o.First = false
		for range 400 {
			t = NextDue(p.Key, p.Schedule, t, t, loc)
			if !t.Before(end) {
				break
			}
			o.At = t
			out = append(out, o)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Load is what a cluster's cost is worked out from.
type Load struct {
	// MaxSessions is the estate's limit on sessions open at once.
	MaxSessions int
	// SessionTime is how long one host's session typically takes.
	SessionTime time.Duration
	// Reach is, for each collector key, the hosts it logs in to.
	Reach map[string]map[string]bool
}

func (l Load) sessions() int {
	if l.MaxSessions <= 0 {
		return DefaultMaxSessions
	}
	return l.MaxSessions
}

// Cluster is runs starting within ClusterWindow of each other.
type Cluster struct {
	Start, End time.Time
	Runs       []Occurrence
	// Keys are the distinct collectors in it, in the order they run.
	Keys []string
	// Firsts counts collectors whose next run is in this cluster.
	Firsts int
	// Hosts is how many different hosts it reaches.
	Hosts int
	// Sessions is how many sessions it opens. Collectors due in the same
	// minute share one session per host; ones a few minutes apart do not.
	Sessions int
	// Duration is roughly how long those sessions take, queued behind the
	// estate's session limit.
	Duration time.Duration
	// Storm says the cluster is busy enough to be worth spreading out.
	Storm bool
}

// ClusterRuns groups occurrences, which must be in time order, into
// clusters: each takes every run starting within window of its first.
func ClusterRuns(occ []Occurrence, window time.Duration, l Load) []Cluster {
	var out []Cluster
	for _, o := range occ {
		if n := len(out); n == 0 || o.At.Sub(out[n-1].Start) >= window {
			out = append(out, Cluster{Start: o.At})
		}
		c := &out[len(out)-1]
		c.Runs = append(c.Runs, o)
		c.End = o.At
	}
	for i := range out {
		out[i].measure(l)
	}
	return out
}

func (c *Cluster) measure(l Load) {
	seen := map[string]bool{}
	hosts := map[string]bool{}
	byMinute := map[time.Time]map[string]bool{}
	for _, o := range c.Runs {
		if !seen[o.Key] {
			seen[o.Key] = true
			c.Keys = append(c.Keys, o.Key)
		}
		if o.First {
			c.Firsts++
		}
		m := o.At.Truncate(time.Minute)
		if byMinute[m] == nil {
			byMinute[m] = map[string]bool{}
		}
		for h := range l.Reach[o.Key] {
			hosts[h] = true
			byMinute[m][h] = true
		}
	}
	c.Hosts = len(hosts)
	for _, hs := range byMinute {
		c.Sessions += len(hs)
	}
	limit := l.sessions()
	st := l.SessionTime
	if st <= 0 {
		st = DefaultSessionTime
	}
	rounds := (c.Sessions + limit - 1) / limit
	c.Duration = time.Duration(rounds) * st
	c.Storm = c.Sessions > limit*StormSessionFactor ||
		(len(c.Keys) >= StormCollectors && c.Sessions > limit)
}

// Hour is one hour of the timeline.
type Hour struct {
	Start    time.Time
	Runs     int
	Sessions int
	Storm    bool
}

// Hours counts runs and sessions in each of the next OutlookHours hours,
// starting with the hour now is in.
func Hours(occ []Occurrence, clusters []Cluster, now time.Time, l Load, loc *time.Location) []Hour {
	if loc == nil {
		loc = time.Local
	}
	n := now.In(loc)
	first := time.Date(n.Year(), n.Month(), n.Day(), n.Hour(), 0, 0, 0, loc)
	out := make([]Hour, OutlookHours)
	for i := range out {
		out[i].Start = first.Add(time.Duration(i) * time.Hour)
	}
	index := func(t time.Time) int {
		d := t.Sub(first)
		if d < 0 {
			return -1
		}
		i := int(d / time.Hour)
		if i >= OutlookHours {
			return -1
		}
		return i
	}
	for _, o := range occ {
		if i := index(o.At); i >= 0 {
			out[i].Runs++
			out[i].Sessions += len(l.Reach[o.Key])
		}
	}
	for _, c := range clusters {
		if i := index(c.Start); i >= 0 && c.Storm {
			out[i].Storm = true
		}
	}
	return out
}

// QuietClock suggests a time of day, on a quarter hour, for a whole-day
// collector to run at: the one with the fewest sessions starting in the
// ClusterWindow after it, and of those the furthest from any other run.
// Runs of the collector named except are left out, so that it does not
// avoid itself.
func QuietClock(occ []Occurrence, l Load, except string, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	const day = 24 * 60
	win := int(ClusterWindow / time.Minute)
	type run struct{ min, weight int }
	var runs []run
	for _, o := range occ {
		if o.Key == except {
			continue
		}
		t := o.At.In(loc)
		w := len(l.Reach[o.Key])
		if w == 0 {
			w = 1
		}
		runs = append(runs, run{t.Hour()*60 + t.Minute(), w})
	}
	best, bestLoad, bestDist := 0, -1, -1
	for slot := 0; slot < day; slot += 15 {
		load, dist := 0, day
		for _, r := range runs {
			after := (r.min - slot + day) % day
			if after < win {
				load += r.weight
			}
			d := min(after, day-after)
			dist = min(dist, d)
		}
		if bestLoad < 0 || load < bestLoad || (load == bestLoad && dist > bestDist) {
			best, bestLoad, bestDist = slot, load, dist
		}
	}
	return fmt.Sprintf("%02d:%02d", best/60, best%60)
}

// StaggerClocks spreads n whole-day collectors out over span from start (a
// time of day, HH:MM): the first at start, the rest at even steps after it,
// on five-minute marks.
func StaggerClocks(start string, n int, span time.Duration) ([]string, error) {
	h, m, err := parseClock(start)
	if err != nil {
		return nil, err
	}
	if n <= 0 {
		return nil, nil
	}
	step := (span / time.Duration(n)).Truncate(5 * time.Minute)
	if step < 5*time.Minute {
		step = 5 * time.Minute
	}
	base := time.Duration(h)*time.Hour + time.Duration(m)*time.Minute
	out := make([]string, n)
	for i := range out {
		t := (base + time.Duration(i)*step) % (24 * time.Hour)
		out[i] = fmt.Sprintf("%02d:%02d", int(t/time.Hour), int(t%time.Hour/time.Minute))
	}
	return out, nil
}

// Reach works out which hosts each scheduled collector in plan logs in to,
// the same way a scheduled run picks them, without contacting any.
func (s *Scheduler) Reach(ctx context.Context, plan []Planned) (map[string]map[string]bool, error) {
	var scheduled []Planned
	for _, p := range plan {
		if !p.Schedule.Manual() {
			scheduled = append(scheduled, p)
		}
	}
	out := map[string]map[string]bool{}
	if len(scheduled) == 0 {
		return out, nil
	}
	jobs, err := s.jobsFor(ctx, scheduled)
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		for k := range j.keys {
			if out[k] == nil {
				out[k] = map[string]bool{}
			}
			out[k][j.ciID] = true
		}
	}
	return out, nil
}

// SessionTime estimates how long one host's session takes, from recent
// scheduled runs: each run's length, less the random spread its hosts were
// started over, divided by the rounds of sessions the limit made it take.
// The middle of those is used. It reports false, and DefaultSessionTime,
// when no scheduled run has finished to measure.
func (s *Scheduler) SessionTime(ctx context.Context) (time.Duration, bool) {
	runs, err := s.Engine.Store.Runs(ctx, 100)
	if err != nil {
		return DefaultSessionTime, false
	}
	limit := s.MaxSessions
	if limit <= 0 {
		limit = DefaultMaxSessions
	}
	var per []time.Duration
	for _, r := range runs {
		if r.Trigger != "schedule" || r.Status != "complete" || r.Targets <= 0 || r.Finished.IsZero() {
			continue
		}
		d := r.Finished.Sub(r.Started) - s.Spread
		rounds := (r.Targets + limit - 1) / limit
		if d <= 0 {
			continue
		}
		per = append(per, d/time.Duration(rounds))
		if len(per) == 20 {
			break
		}
	}
	if len(per) == 0 {
		return DefaultSessionTime, false
	}
	sort.Slice(per, func(i, j int) bool { return per[i] < per[j] })
	t := per[len(per)/2].Round(time.Second)
	if t < time.Second {
		t = time.Second
	}
	return t, true
}

// ClockText is a run time a person can plan around, in loc: "Today 14:00",
// "Tomorrow 02:00", "Thu 02:00", or a date further out. A time already
// past is "Now": the collector is due.
func ClockText(t, now time.Time, loc *time.Location) string {
	if t.IsZero() {
		return "—"
	}
	if !t.After(now) {
		return "Now"
	}
	if loc == nil {
		loc = time.Local
	}
	t, now = t.In(loc), now.In(loc)
	day := func(x time.Time) time.Time { return time.Date(x.Year(), x.Month(), x.Day(), 0, 0, 0, 0, loc) }
	days := int(day(t).Sub(day(now)).Round(24*time.Hour) / (24 * time.Hour))
	clock := t.Format("15:04")
	switch {
	case days == 0:
		return "Today " + clock
	case days == 1:
		return "Tomorrow " + clock
	case days < 7:
		return t.Format("Mon") + " " + clock
	default:
		return t.Format("Mon 2 Jan") + " " + clock
	}
}

// AboutText says roughly how long something takes.
func AboutText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("about %d min", int(d.Round(time.Minute)/time.Minute))
	default:
		d = d.Round(10 * time.Minute)
		if d%time.Hour == 0 {
			return fmt.Sprintf("about %dh", int(d/time.Hour))
		}
		return fmt.Sprintf("about %dh %dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
}

// Outlook is the plan with when everything runs and how busy that is.
type Outlook struct {
	Now         time.Time
	Plan        []Planned
	Occurrences []Occurrence
	Clusters    []Cluster
	Hours       []Hour
	Load        Load
	// Measured says SessionTime came from recent runs rather than a guess.
	Measured bool
}

// Outlook works out the plan, the runs ahead, their clusters and the
// timeline.
func (s *Scheduler) Outlook(ctx context.Context) (Outlook, error) {
	plan, err := s.Plan(ctx)
	if err != nil {
		return Outlook{}, err
	}
	reach, err := s.Reach(ctx, plan)
	if err != nil {
		return Outlook{}, err
	}
	st, measured := s.SessionTime(ctx)
	o := Outlook{Now: s.now(), Plan: plan, Measured: measured,
		Load: Load{MaxSessions: s.MaxSessions, SessionTime: st, Reach: reach}}
	o.Occurrences = Occurrences(plan, o.Now, s.loc())
	o.Clusters = ClusterRuns(o.Occurrences, ClusterWindow, o.Load)
	o.Hours = Hours(o.Occurrences, o.Clusters, o.Now, o.Load, s.loc())
	return o, nil
}
