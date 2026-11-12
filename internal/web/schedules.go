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
	Next     string
	Overdue  bool
	Running  bool
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

func (s *Server) scheduleRows(ctx context.Context) ([]scheduleRow, error) {
	if s.Scheduler == nil {
		return nil, nil
	}
	plan, err := s.Scheduler.Plan(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]scheduleRow, 0, len(plan))
	for _, p := range plan {
		r := scheduleRow{
			Key: p.Key, Anchor: anchorFor(p.Key), Kind: p.Kind, Title: p.Title,
			Describe: p.Schedule.Describe(), Default: p.Default.Describe(),
			Custom: p.Custom, Manual: p.Schedule.Manual(), At: p.Schedule.At,
			LastRun: p.State.LastStarted, Outcome: p.State.Outcome, Detail: p.State.Detail,
			Running: p.Running,
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
		r.Next = untilText(p.Next, now)
		r.Overdue = !p.Next.IsZero() && !p.Next.After(now)
		if r.Manual {
			r.Next = "only when asked"
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *Server) handleSchedules(w http.ResponseWriter, r *http.Request) {
	rows, err := s.scheduleRows(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	d := struct {
		Rows        []scheduleRow
		Scheduling  bool
		MinEvery    string
		MaxSessions int
		PerHost     int
		Spread      string
		Backoff     string
		BackoffMax  string
	}{Rows: rows, MinEvery: engine.FormatEvery(engine.MinEvery)}
	if sc := s.Scheduler; sc != nil {
		d.Scheduling = sc.Running()
		d.MaxSessions, d.PerHost = sc.MaxSessions, sc.PerHost
		d.Spread = engine.FormatEvery(sc.Spread)
		d.Backoff, d.BackoffMax = engine.FormatEvery(sc.BackoffBase), engine.FormatEvery(sc.BackoffMax)
	}
	s.render(w, r, "schedules.html", "Schedules", "schedules", d)
}

// handleSetSchedule changes one collector's schedule. The collector must be
// one that exists; the form cannot name a command, only pick a collector.
func (s *Server) handleSetSchedule(w http.ResponseWriter, r *http.Request) {
	const back = "/schedules"
	if s.Scheduler == nil {
		refuse(w, r, back, errors.New("scheduling needs a collection engine"))
		return
	}
	ctx := r.Context()
	key := field(r, "collector")
	actor := actorOf(r)
	var (
		c   engine.Collector
		err error
		msg string
	)
	switch field(r, "mode") {
	case "default":
		c, err = s.Scheduler.ResetSchedule(ctx, key, actor)
		msg = "back to its default, " + c.Default.Describe()
	case "manual":
		c, err = s.Scheduler.SetSchedule(ctx, key, engine.Schedule{}, actor)
		msg = "manual only"
	default:
		var every time.Duration
		every, err = engine.ParseEvery(field(r, "every"))
		if err == nil {
			sc := engine.Schedule{Every: every, At: field(r, "at")}
			c, err = s.Scheduler.SetSchedule(ctx, key, sc, actor)
			if at, aerr := engine.NormalizeClock(sc.At); aerr == nil {
				sc.At = at
			}
			msg = sc.Describe()
		}
	}
	if err != nil {
		refuse(w, r, back, err)
		return
	}
	done(w, r, back+"#"+anchorFor(c.Key), c.Title+" now runs "+msg+".")
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
