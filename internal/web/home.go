package web

import (
	"net/http"
	"time"

	"github.com/KalebHawkins/kryostate/internal/engine"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// The overview page.

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
