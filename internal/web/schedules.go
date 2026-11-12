package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/KalebHawkins/kryostate/internal/engine"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// The schedules page, the changes page and the overview.
//
// A schedule is not a command. The page picks one of the collectors that
// already exist and says how often it runs; nothing typed here reaches a
// host, so it is offered in the browser like any other setting.

type scheduleRow struct {
	Key      string
	Anchor   string
	Kind     string
	Title    string
	Describe string
	Default  string
	Custom   bool
	Manual   bool
	Every    string
	At       string
	LastRun  time.Time
	Outcome  string
	Detail   string
	// Next is how long until the next run ("in 9h"); When is its clock
	// time ("Tomorrow 02:00"), in the server's location.
	Next    string
	When    string
	NextAt  time.Time
	Overdue bool
	Running bool
	// Spread is the time picked for a schedule with none of its own.
	Spread   string
	WholeDay bool
	// Hosts is how many hosts a run logs in to.
	Hosts int
	// Quiet is the least busy time of day, for a whole-day collector.
	Quiet string
	// The editor's choices, with what is in force now selected.
	Options     []option
	CustomEvery string
	HourOpts    []option
	MinuteOpts  []option
}

func untilText(t, now time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := t.Sub(now)
	switch {
	case d <= 0:
		return "due now"
	case d < time.Minute:
		return "in under a minute"
	case d < time.Hour:
		return fmt.Sprintf("in %dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("in %dh", int(d.Hours()))
	default:
		return fmt.Sprintf("in %dd", int(d.Hours()/24))
	}
}

// scheduleRows lists every collector's schedule in plan order, for the
// collector page's Schedule card.
func (s *Server) scheduleRows(ctx context.Context) ([]scheduleRow, error) {
	v, err := s.scheduleView(ctx)
	return v.Rows, err
}

// handleSetSchedule changes one collector's schedule. The collector must be
// one that exists; the form cannot name a command, only pick a collector.
//
// The editor posts a preset (with a custom interval, hour and minute where
// they apply). The older fields, mode with every and at, are still read, so
// a form or script written for them keeps working.
func (s *Server) handleSetSchedule(w http.ResponseWriter, r *http.Request) {
	back := "/schedules"
	if b := field(r, "back"); strings.HasPrefix(b, "/collectors/") && !strings.ContainsAny(b, `\?#`) && !strings.Contains(b, "//") {
		// The collector page sends its own address so the change returns there.
		back = b
	}
	if s.Scheduler == nil {
		refuse(w, r, back, errors.New("scheduling needs a collection engine"))
		return
	}
	ctx := r.Context()
	key := field(r, "collector")
	actor := actorOf(r)
	mode, sc := field(r, "mode"), engine.Schedule{}
	if preset := field(r, "preset"); preset != "" {
		form := scheduleForm{Preset: preset, Every: field(r, "every"), Hour: field(r, "hour"), Minute: field(r, "minute")}
		quiet := mode == "quiet"
		if quiet {
			form.Hour = "" // the quiet time replaces whatever the boxes say
		}
		var err error
		if mode, sc, err = parseScheduleForm(form); err != nil {
			refuse(w, r, back, err)
			return
		}
		if quiet {
			if mode != "set" || sc.Every%(24*time.Hour) != 0 {
				refuse(w, r, back, errors.New("a quiet time goes with a daily (or longer) interval; choose Daily first"))
				return
			}
			o, err := s.Scheduler.Outlook(ctx)
			if err != nil {
				s.fail(w, err)
				return
			}
			c, err := s.Scheduler.Find(ctx, key)
			if err != nil {
				refuse(w, r, back, err)
				return
			}
			sc.At = engine.QuietClock(o.Occurrences, o.Load, c.Key, s.Scheduler.Loc())
		}
	} else if mode != "default" && mode != "manual" {
		every, err := engine.ParseEvery(field(r, "every"))
		if err != nil {
			refuse(w, r, back, err)
			return
		}
		mode, sc = "set", engine.Schedule{Every: every, At: field(r, "at")}
	}
	var (
		c   engine.Collector
		err error
		msg string
	)
	switch mode {
	case "default":
		c, err = s.Scheduler.ResetSchedule(ctx, key, actor)
		msg = "back to its default, " + c.Default.Describe()
	case "manual":
		c, err = s.Scheduler.SetSchedule(ctx, key, engine.Schedule{}, actor)
		msg = "manual only"
	default:
		c, err = s.Scheduler.SetSchedule(ctx, key, sc, actor)
		if at, aerr := engine.NormalizeClock(sc.At); aerr == nil {
			sc.At = at
		}
		msg = sc.Describe()
		if t := engine.SpreadText(c.Key, sc); t != "" && err == nil {
			msg += " (" + t + ", picked for it)"
		}
	}
	if err != nil {
		refuse(w, r, back, err)
		return
	}
	done(w, r, backAnchor(back, c.Key), c.Title+" now runs "+msg+".")
}

// anchorFor turns a collector key into an element id.
func anchorFor(key string) string {
	return "sched-" + strings.NewReplacer(".", "-", ":", "-", " ", "-").Replace(key)
}

type changeRow struct {
	store.Change
	Collector string
}

// changeRows names the collector behind each change, using the titles the
// schedules page uses.
func (s *Server) changeRows(ctx context.Context, cs []store.Change) []changeRow {
	titles := map[string]string{}
	if s.Engine != nil {
		if all, err := s.Engine.Collectors(ctx); err == nil {
			for _, c := range all {
				titles[c.Key] = c.Title
			}
		}
	}
	out := make([]changeRow, 0, len(cs))
	for _, c := range cs {
		key := engine.CollectorKeyFor(c.SpecID, c.SourceName)
		name := titles[key]
		switch {
		case name == "" && key != "":
			name = key
		case name == "":
			name = c.SourceName
		}
		out = append(out, changeRow{Change: c, Collector: name})
	}
	return out
}

var changeWindows = []struct {
	Key   string
	Label string
	Dur   time.Duration
}{
	{"24h", "Last 24 hours", 24 * time.Hour},
	{"7d", "Last 7 days", 7 * 24 * time.Hour},
	{"30d", "Last 30 days", 30 * 24 * time.Hour},
}

func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	win := changeWindows[0]
	for _, cw := range changeWindows {
		if cw.Key == q.Get("window") {
			win = cw
		}
	}
	f := store.ChangeFilter{Since: time.Now().Add(-win.Dur), Attr: q.Get("attr"), Limit: 1000}
	hostLabel := ""
	if id := q.Get("host"); id != "" {
		ci, err := s.Store.CI(ctx, id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		f.CIID, hostLabel = id, ci.Label
	}
	cs, err := s.Store.Changes(ctx, f)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "changes.html", "Changes", "changes", struct {
		Rows      []changeRow
		Window    string
		Windows   any
		Host      string
		HostLabel string
		Attr      string
		Limited   bool
	}{s.changeRows(ctx, cs), win.Key, changeWindows, f.CIID, hostLabel, f.Attr, len(cs) >= f.Limit})
}

// hostChanges is the recent change, if any, behind each value on a host
// page, keyed by the assertion that holds the new value.
func (s *Server) hostChanges(ctx context.Context, ciID string) map[int64]*store.Change {
	cs, err := s.Store.Changes(ctx, store.ChangeFilter{CIID: ciID, Since: time.Now().Add(-7 * 24 * time.Hour)})
	if err != nil {
		if s.Log != nil {
			s.Log.Warn("host changes", "err", err)
		}
		return nil
	}
	out := map[int64]*store.Change{}
	for i := range cs {
		if _, ok := out[cs[i].AssertionID]; !ok {
			out[cs[i].AssertionID] = &cs[i]
		}
	}
	return out
}

type upcomingRow struct {
	Key   string
	Title string
	When  string
	Sched string
}

type overviewData struct {
	Health       store.Health
	StaleAfter   string
	Failing      []store.FailingHost
	MoreFailing  bool
	Changes      []changeRow
	MoreChanges  bool
	Upcoming     []upcomingRow
	Scheduling   bool
	Families     []store.Bucket
	LastRun      *store.RunSummary
	ManualOnly   bool
	TotalChanges int
}

// handleHome answers two questions: is the estate all right, and what
// changed. Counts come from collection outcomes, not from rows in a table, so
// a host nobody can reach is counted as that rather than as fine.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	fallback := s.StaleAfter
	if fallback <= 0 {
		fallback = 14 * 24 * time.Hour
	}
	staleAfter := fallback
	if s.Scheduler != nil {
		staleAfter = s.Scheduler.StaleAfter(ctx, fallback)
	}
	var d overviewData
	var err error
	if d.Health, err = s.Store.EstateHealth(ctx, staleAfter); err != nil {
		s.fail(w, err)
		return
	}
	d.StaleAfter = engine.FormatEvery(staleAfter.Round(time.Minute))

	const failingShown, changesShown = 10, 12
	failing, err := s.Store.FailingHosts(ctx, failingShown+1)
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(failing) > failingShown {
		failing, d.MoreFailing = failing[:failingShown], true
	}
	d.Failing = failing

	cs, err := s.Store.Changes(ctx, store.ChangeFilter{Since: time.Now().Add(-24 * time.Hour), Limit: 1000})
	if err != nil {
		s.fail(w, err)
		return
	}
	d.TotalChanges = len(cs)
	if len(cs) > changesShown {
		cs, d.MoreChanges = cs[:changesShown], true
	}
	d.Changes = s.changeRows(ctx, cs)

	if s.Scheduler != nil {
		d.Scheduling = s.Scheduler.Running()
		up, err := s.Scheduler.Upcoming(ctx, 6)
		if err != nil {
			s.fail(w, err)
			return
		}
		now := time.Now()
		for _, p := range up {
			when := untilText(p.Next, now)
			if p.Running {
				when = "running now"
			}
			d.Upcoming = append(d.Upcoming, upcomingRow{Key: p.Key, Title: p.Title, When: when, Sched: p.Schedule.Describe()})
		}
		d.ManualOnly = len(up) == 0
	}

	if fams, err := s.Store.Distribution(ctx, "os.family"); err == nil {
		for _, b := range fams {
			if b.Count > 0 {
				d.Families = append(d.Families, b)
			}
		}
	}
	if runs, err := s.Store.Runs(ctx, 1); err == nil && len(runs) > 0 {
		d.LastRun = &runs[0]
	}
	s.render(w, r, "home.html", "Overview", "home", d)
}

// backAnchor is where a schedule change returns to: the row on the schedules
// page, or the schedule card on a collector's own page.
func backAnchor(back, key string) string {
	if back == "/schedules" {
		return back + "#" + anchorFor(key)
	}
	return back + "#schedule"
}

// schedulesFor picks the schedules that belong to one collector on the
// collectors page. Its ids and the scheduler's keys differ: a catalog entry
// switched on twice with different values has two schedules, and the built-in
// commands are scheduled by fact group rather than one attribute at a time.
func schedulesFor(rows []scheduleRow, id, source string) []scheduleRow {
	var want func(key string) bool
	switch {
	case source == srcCatalog:
		want = func(k string) bool {
			rest, ok := strings.CutPrefix(k, "catalog."+id)
			if !ok {
				return false
			}
			if rest == "" {
				return true
			}
			n, ok := strings.CutPrefix(rest, ".")
			return ok && n != "" && strings.Trim(n, "0123456789") == ""
		}
	case strings.HasPrefix(id, "builtin."):
		group, _, _ := strings.Cut(strings.TrimPrefix(id, "builtin."), ".")
		want = func(k string) bool { return k == "builtin."+group }
	default:
		want = func(k string) bool { return k == id }
	}
	var out []scheduleRow
	for _, r := range rows {
		if want(r.Key) {
			out = append(out, r)
		}
	}
	return out
}
