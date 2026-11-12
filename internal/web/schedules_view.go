package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/KalebHawkins/kryostate/internal/engine"
)

// The schedules page reads as a timetable: when things run, which of them
// run close together, and how much that asks of the estate. Clock times come
// first because "02:00" is what a person plans around; "in 9h" is second.

// schedulePresets are the intervals offered in the editor. Anything else is
// still accepted under Custom, within engine.MinEvery and engine.MaxEvery.
var schedulePresets = []struct {
	Value string
	Label string
	Every time.Duration
}{
	{"15m", "Every 15 min", 15 * time.Minute},
	{"30m", "Every 30 min", 30 * time.Minute},
	{"1h", "Every hour", time.Hour},
	{"2h", "Every 2 hours", 2 * time.Hour},
	{"4h", "Every 4 hours", 4 * time.Hour},
	{"6h", "Every 6 hours", 6 * time.Hour},
	{"12h", "Every 12 hours", 12 * time.Hour},
	{"1d", "Daily", 24 * time.Hour},
	{"2d", "Every 2 days", 48 * time.Hour},
	{"7d", "Weekly", 7 * 24 * time.Hour},
}

// spreadOver is how far the storm suggestion spreads a cluster out.
const spreadOver = 2 * time.Hour

type option struct {
	Value    string
	Label    string
	Selected bool
}

// scheduleForm is what the editor posts, before it is checked.
type scheduleForm struct {
	Preset string // a preset value, "default", "manual" or "custom"
	Every  string // the custom interval
	Hour   string // "" for a time picked automatically
	Minute string
}

// parseScheduleForm turns the editor's fields into what to do: "default",
// "manual" or "set" with a schedule. A time of day only counts for a
// whole-day interval; for anything shorter the hour and minute boxes are
// hidden, so whatever they hold was not meant.
func parseScheduleForm(f scheduleForm) (string, engine.Schedule, error) {
	var sc engine.Schedule
	switch f.Preset {
	case "default":
		return "default", sc, nil
	case "manual":
		return "manual", sc, nil
	case "custom":
		every, err := engine.ParseEvery(f.Every)
		if err != nil {
			return "", sc, err
		}
		sc.Every = every
	default:
		found := false
		for _, p := range schedulePresets {
			if p.Value == f.Preset {
				sc.Every, found = p.Every, true
			}
		}
		if !found {
			return "", sc, fmt.Errorf("%q is not one of the choices", f.Preset)
		}
	}
	if sc.Every%(24*time.Hour) == 0 && f.Hour != "" {
		h, err := strconv.Atoi(f.Hour)
		if err != nil || h < 0 || h > 23 {
			return "", sc, fmt.Errorf("%q is not an hour", f.Hour)
		}
		m := 0
		if f.Minute != "" {
			if m, err = strconv.Atoi(f.Minute); err != nil || m < 0 || m > 59 {
				return "", sc, fmt.Errorf("%q is not a minute", f.Minute)
			}
		}
		sc.At = fmt.Sprintf("%02d:%02d", h, m)
	}
	if err := sc.Check(); err != nil {
		return "", sc, err
	}
	return "set", sc, nil
}

// whenText is a clock time a person can plan around: "Today 14:00",
// "Tomorrow 02:00", "Thu 02:00", or a date further out.
func whenText(t, now time.Time, loc *time.Location) string { return engine.ClockText(t, now, loc) }

// aboutText says roughly how long something takes.
func aboutText(d time.Duration) string { return engine.AboutText(d) }

func schedCount(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

type clusterView struct {
	ID       string
	When     string
	Until    string // the clock time of its last start, when it spans minutes
	In       string
	Summary  string
	Duration string
	Sessions int
	Storm    bool
	Rows     []scheduleRow
	Also     []string
	// SpreadKeys are its whole-day collectors, which the storm suggestion
	// can move; SpreadStart is where they would start from.
	SpreadKeys  []string
	SpreadStart string
	SpreadOver  string
	Unmovable   int
}

type hourView struct {
	Label  string
	Title  string
	Height template.CSS
	Runs   int
	Storm  bool
	Href   string
}

type scheduleView struct {
	Rows     []scheduleRow // in plan order
	Clusters []clusterView
	Manual   []scheduleRow
	Hours    []hourView
	Session  string
	Measured bool
	Location string
}

// scheduleView works out everything the schedules page and the collector
// page's Schedule card show.
func (s *Server) scheduleView(ctx context.Context) (scheduleView, error) {
	var v scheduleView
	if s.Scheduler == nil {
		return v, nil
	}
	o, err := s.Scheduler.Outlook(ctx)
	if err != nil {
		return v, err
	}
	loc := s.Scheduler.Loc()
	now := o.Now
	v.Session, v.Measured = aboutSession(o.Load.SessionTime), o.Measured
	v.Location = now.In(loc).Format("MST")

	byKey := map[string]int{}
	for _, p := range o.Plan {
		r := newScheduleRow(p, now, loc)
		r.Hosts = len(o.Load.Reach[p.Key])
		// Offered for every row: the editor may be switched to Daily.
		r.Quiet = engine.QuietClock(o.Occurrences, o.Load, p.Key, loc)
		byKey[p.Key] = len(v.Rows)
		v.Rows = append(v.Rows, r)
		if r.Manual {
			v.Manual = append(v.Manual, r)
		}
	}

	shown := map[int]string{} // cluster index -> element id, for the timeline
	for i, c := range o.Clusters {
		if c.Firsts == 0 {
			continue // only later runs: they are listed at their next run
		}
		cv := clusterView{
			ID:   fmt.Sprintf("at-%s", c.Start.In(loc).Format("200601021504")),
			When: whenText(c.Start, now, loc), In: soonText(c.Start, now),
			Duration: aboutText(c.Duration), Sessions: c.Sessions, Storm: c.Storm,
			SpreadOver: engine.FormatEvery(spreadOver),
		}
		if c.End.Sub(c.Start) >= time.Minute {
			cv.Until = c.End.In(loc).Format("15:04")
		}
		parts := []string{schedCount(len(c.Keys), "collector", "collectors")}
		if c.Hosts > 0 {
			parts = append(parts, "~"+schedCount(c.Hosts, "host", "hosts"))
		}
		cv.Summary = strings.Join(parts, " · ")
		listed := map[string]bool{}
		for _, run := range c.Runs {
			if listed[run.Key] {
				continue
			}
			listed[run.Key] = true
			r := v.Rows[byKey[run.Key]]
			if run.First {
				cv.Rows = append(cv.Rows, r)
				if r.WholeDay {
					cv.SpreadKeys = append(cv.SpreadKeys, r.Key)
				} else {
					cv.Unmovable++
				}
			} else {
				cv.Also = append(cv.Also, r.Title+" ("+r.Describe+")")
			}
		}
		t := c.Start.In(loc)
		cv.SpreadStart = fmt.Sprintf("%02d:%02d", t.Hour(), t.Minute()/5*5)
		shown[i] = cv.ID
		v.Clusters = append(v.Clusters, cv)
	}

	busiest := 1
	for _, h := range o.Hours {
		busiest = max(busiest, h.Runs)
	}
	for _, h := range o.Hours {
		hv := hourView{Label: h.Start.In(loc).Format("15"), Runs: h.Runs, Storm: h.Storm}
		hv.Title = h.Start.In(loc).Format("15:04") + ": " + schedCount(h.Runs, "run", "runs")
		if h.Sessions > 0 {
			hv.Title += ", ~" + schedCount(h.Sessions, "session", "sessions")
		}
		if h.Storm {
			hv.Title += ", busy"
		}
		pct := 0
		if h.Runs > 0 {
			pct = 8 + 92*h.Runs/busiest
		}
		hv.Height = template.CSS(fmt.Sprintf("height:%d%%", pct))
		for i, c := range o.Clusters {
			id, ok := shown[i]
			if ok && !c.Start.Before(h.Start) && c.Start.Before(h.Start.Add(time.Hour)) {
				hv.Href = "#" + id
				break
			}
		}
		v.Hours = append(v.Hours, hv)
	}
	return v, nil
}

// soonText is the relative time shown beside a clock time: empty when the
// clock time already says "Now".
func soonText(t, now time.Time) string {
	if !t.IsZero() && !t.After(now) {
		return ""
	}
	return untilText(t, now)
}

func aboutSession(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
	return engine.FormatEvery(d.Round(time.Minute))
}

// newScheduleRow describes one collector's schedule and fills its editor.
func newScheduleRow(p engine.Planned, now time.Time, loc *time.Location) scheduleRow {
	r := scheduleRow{
		Key: p.Key, Anchor: anchorFor(p.Key), Kind: p.Kind, Title: p.Title,
		Describe: p.Schedule.Describe(), Default: p.Default.Describe(),
		Custom: p.Custom, Manual: p.Schedule.Manual(), At: p.Schedule.At,
		LastRun: p.State.LastStarted, Outcome: p.State.Outcome, Detail: p.State.Detail,
		Running: p.Running, NextAt: p.Next,
		Spread:   engine.SpreadText(p.Key, p.Schedule),
		WholeDay: !p.Schedule.Manual() && p.Schedule.Every%(24*time.Hour) == 0,
	}
	if r.LastRun.IsZero() {
		r.LastRun = p.Last
	}
	if p.Running {
		r.Outcome = "running"
	}
	if !r.Manual {
		r.Every = engine.FormatEvery(p.Schedule.Every)
	} else {
		r.Every = engine.FormatEvery(p.Default.Every)
	}
	r.Overdue = !p.Next.IsZero() && !p.Next.After(now)
	switch {
	case r.Manual:
		r.Next, r.When = "only when asked", "Only when asked"
	case p.Running:
		r.Next, r.When = "running now", "Running now"
	default:
		r.When = whenText(p.Next, now, loc)
		r.Next = soonText(p.Next, now)
	}

	// The editor starts on what is in force now.
	preset := "custom"
	switch {
	case !p.Custom:
		preset = "default"
	case r.Manual:
		preset = "manual"
	default:
		for _, pr := range schedulePresets {
			if pr.Every == p.Schedule.Every {
				preset = pr.Value
			}
		}
	}
	r.CustomEvery = r.Every
	r.Options = append(r.Options, option{"default", "Default (" + p.Default.Describe() + ")", preset == "default"})
	r.Options = append(r.Options, option{"manual", "Manual only", preset == "manual"})
	for _, pr := range schedulePresets {
		r.Options = append(r.Options, option{pr.Value, pr.Label, preset == pr.Value})
	}
	r.Options = append(r.Options, option{"custom", "Custom…", preset == "custom"})

	hour, minute := "", "00"
	if hs, ms, ok := strings.Cut(p.Schedule.At, ":"); ok {
		hour, minute = hs, ms
	}
	auto := "Any time (picked for it)"
	if c := engine.SpreadClock(p.Key); c != "" {
		auto = "Picked for it (" + c + ")"
	}
	r.HourOpts = append(r.HourOpts, option{"", auto, hour == ""})
	for h := 0; h < 24; h++ {
		v := fmt.Sprintf("%02d", h)
		r.HourOpts = append(r.HourOpts, option{v, v, v == hour})
	}
	mins := []string{"00", "15", "30", "45"}
	if !contains(mins, minute) {
		mins = append(mins, minute)
	}
	for _, m := range mins {
		r.MinuteOpts = append(r.MinuteOpts, option{m, m, m == minute})
	}
	return r
}

func (s *Server) handleSchedules(w http.ResponseWriter, r *http.Request) {
	v, err := s.scheduleView(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	d := struct {
		scheduleView
		Scheduling  bool
		MinEvery    string
		MaxSessions int
		PerHost     int
		Spread      string
		Backoff     string
		BackoffMax  string
		Window      string
	}{scheduleView: v, MinEvery: engine.FormatEvery(engine.MinEvery), Window: engine.FormatEvery(engine.ClusterWindow)}
	if sc := s.Scheduler; sc != nil {
		d.Scheduling = sc.Running()
		d.MaxSessions, d.PerHost = sc.MaxSessions, sc.PerHost
		d.Spread = engine.FormatEvery(sc.Spread)
		d.Backoff, d.BackoffMax = engine.FormatEvery(sc.BackoffBase), engine.FormatEvery(sc.BackoffMax)
	}
	s.render(w, r, "schedules.html", "Schedules", "schedules", d)
}

// handleSpreadSchedules is the one-click answer to a busy cluster: its
// whole-day collectors are given times of day spread over a couple of hours
// from where the cluster starts. Only collectors that exist can be named,
// and only whole-day ones are moved.
func (s *Server) handleSpreadSchedules(w http.ResponseWriter, r *http.Request) {
	back := "/schedules"
	if s.Scheduler == nil {
		refuse(w, r, back, errors.New("scheduling needs a collection engine"))
		return
	}
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		refuse(w, r, back, err)
		return
	}
	over, err := engine.ParseEvery(field(r, "over"))
	if err != nil || over < 30*time.Minute || over > 12*time.Hour {
		refuse(w, r, back, errors.New("spread them over between 30m and 12h"))
		return
	}
	plan, err := s.Scheduler.Plan(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	current := map[string]engine.Schedule{}
	for _, p := range plan {
		current[p.Key] = p.Schedule
	}
	var keys []string
	seen := map[string]bool{}
	for _, k := range r.PostForm["collector"] {
		sc, ok := current[k]
		if !ok || seen[k] || sc.Manual() || sc.Every%(24*time.Hour) != 0 {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	if len(keys) < 2 {
		refuse(w, r, back, errors.New("there is nothing here to spread out: it needs two or more daily (or longer) collectors"))
		return
	}
	clocks, err := engine.StaggerClocks(field(r, "start"), len(keys), over)
	if err != nil {
		refuse(w, r, back, err)
		return
	}
	actor := actorOf(r)
	for i, k := range keys {
		sc := engine.Schedule{Every: current[k].Every, At: clocks[i]}
		if _, err := s.Scheduler.SetSchedule(ctx, k, sc, actor); err != nil {
			refuse(w, r, back, err)
			return
		}
	}
	done(w, r, back, fmt.Sprintf("Spread %d collectors over %s, from %s to %s.",
		len(keys), engine.FormatEvery(over), clocks[0], clocks[len(clocks)-1]))
}
