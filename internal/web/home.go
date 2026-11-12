package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/KalebHawkins/kryostate/internal/engine"
	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// The overview page. It is read top to bottom: one sentence saying whether
// the estate is all right, the problems worth acting on (and only those that
// exist), what changed, what runs next, and what the estate is made of. Every
// number links to the inventory filtered to the hosts behind it.

// banner is the headline. Tone is ok, warn, bad or none, and picks the colour;
// the words say the same thing without it.
type banner struct {
	Tone     string
	Headline string
	Detail   string
}

// attentionItem is one non-empty problem bucket.
type attentionItem struct {
	Key   string
	Count int
	Title string
	Note  string
	Href  string
	Tone  string
}

type upcomingRow struct {
	Key   string
	Title string
	// When is a clock time, such as "14:00 today"; In is the same as a
	// distance, such as "in 5h", for the title attribute.
	When  string
	In    string
	Sched string
	Due   bool
}

// glanceRow is one bar in the estate at a glance.
type glanceRow struct {
	Label   string
	Href    string
	Count   int
	Pct     int
	Unknown bool
}

type glanceGroup struct {
	Title    string
	Rows     []glanceRow
	More     int
	MoreHref string
	Empty    string
}

type overviewData struct {
	Banner      banner
	Health      store.Health
	StaleAfter  string
	Attention   []attentionItem
	Failing     []store.FailingHost
	MoreFailing bool
	Changes     []changeRow
	// TotalChanges counts the last day's changes, up to the limit read.
	TotalChanges int
	MoreChanges  bool
	Upcoming     []upcomingRow
	Scheduling   bool
	ManualOnly   bool
	LastRun      *store.RunSummary
	Glance       []glanceGroup
	Rollouts     rollouts
}

// staleAfter is how long since a host's last good collection before the
// overview calls it out of date: twice the shortest built-in schedule, or the
// configured window when nothing built in is scheduled.
func (s *Server) staleAfter(ctx context.Context) time.Duration {
	fallback := s.StaleAfter
	if fallback <= 0 {
		fallback = 14 * 24 * time.Hour
	}
	if s.Scheduler != nil {
		return s.Scheduler.StaleAfter(ctx, fallback)
	}
	return fallback
}

// handleHome answers, in order: is the estate all right, what needs doing,
// what changed, and what runs next. Counts come from collection outcomes, not
// from rows in a table, so a host nobody can reach is counted as that rather
// than as fine.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	staleAfter := s.staleAfter(ctx)
	var d overviewData
	var err error
	if d.Health, err = s.Store.EstateHealth(ctx, staleAfter); err != nil {
		s.fail(w, err)
		return
	}
	attention, err := s.Store.NeedingAttention(ctx, staleAfter)
	if err != nil {
		s.fail(w, err)
		return
	}
	d.StaleAfter = engine.FormatEvery(staleAfter.Round(time.Minute))
	if runs, err := s.Store.Runs(ctx, 1); err == nil && len(runs) > 0 {
		d.LastRun = &runs[0]
	}
	d.Banner = overviewBanner(d.Health, attention, d.LastRun)
	d.Attention = attentionItems(d.Health, d.StaleAfter)

	const failingShown, changesShown = 5, 8
	if d.Health.Failing > 0 {
		failing, err := s.Store.FailingHosts(ctx, failingShown+1)
		if err != nil {
			s.fail(w, err)
			return
		}
		if len(failing) > failingShown {
			failing, d.MoreFailing = failing[:failingShown], true
		}
		d.Failing = failing
	}

	cs, err := s.Store.Changes(ctx, store.ChangeFilter{Since: now.Add(-24 * time.Hour), Limit: 1000})
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
		for _, p := range up {
			row := upcomingRow{Key: p.Key, Title: p.Title, Sched: p.Schedule.Describe(),
				When: clockText(p.Next, now), In: untilText(p.Next, now)}
			row.Due = !p.Next.IsZero() && !p.Next.After(now)
			if p.Running {
				row.When, row.In, row.Due = "running now", "", false
			}
			d.Upcoming = append(d.Upcoming, row)
		}
		d.ManualOnly = len(up) == 0
	}

	d.Glance = s.glance(ctx, d.Health.Hosts)
	d.Rollouts = s.rollouts(ctx)
	s.render(w, r, "home.html", "Overview", "home", d)
}

// hostsWord is "1 host" or "n hosts".
func hostsWord(n int) string {
	if n == 1 {
		return "1 host"
	}
	return fmt.Sprintf("%d hosts", n)
}

// overviewBanner says in one line whether the estate is all right.
// attention counts hosts in any problem bucket, each once.
func overviewBanner(h store.Health, attention int, last *store.RunSummary) banner {
	var b banner
	switch {
	case h.Hosts == 0:
		b.Tone, b.Headline = "none", "No hosts yet"
	case h.Failing > 0:
		b.Tone = "bad"
		b.Headline = fmt.Sprintf("%d of %s failing", h.Failing, hostsWord(h.Hosts))
		if more := attention - h.Failing; more > 0 {
			b.Headline += fmt.Sprintf(" · %d more need attention", more)
		}
	case attention > 0:
		b.Tone = "warn"
		b.Headline = fmt.Sprintf("%d of %s need attention", attention, hostsWord(h.Hosts))
	case h.Hosts == 1:
		b.Tone, b.Headline = "ok", "The only host collected successfully"
	default:
		b.Tone, b.Headline = "ok", fmt.Sprintf("All %d hosts collected successfully", h.Hosts)
	}
	switch {
	case last != nil && last.Finished.IsZero():
		b.Detail = "a run is in progress, started " + ago(last.Started)
	case last != nil:
		b.Detail = fmt.Sprintf("last run %s: %d succeeded, %d failed", ago(last.Started), last.Succeeded, last.Failed)
	case h.Hosts > 0:
		b.Detail = "nothing collected yet"
	default:
		b.Detail = "add one from the inventory"
	}
	return b
}

// attentionItems lists the problem buckets that are not empty, worst first.
func attentionItems(h store.Health, staleAfter string) []attentionItem {
	all := []attentionItem{
		{Key: store.HealthFailing, Count: h.Failing, Tone: "bad", Title: "failing",
			Note: "Their latest collection did not succeed."},
		{Key: store.HealthNoConn, Count: h.NoConnection, Tone: "warn", Title: "with no connection",
			Note: "KryoState has no way to log in to them, so nothing will be collected."},
		{Key: store.HealthStale, Count: h.Stale, Tone: "warn", Title: "out of date",
			Note: "Last collected successfully more than " + staleAfter + " ago."},
		{Key: store.HealthNever, Count: h.Never, Tone: "warn", Title: "never collected",
			Note: "Nothing has been collected from them yet."},
	}
	var out []attentionItem
	for _, a := range all {
		if a.Count > 0 {
			a.Href = "/hosts?health=" + a.Key
			out = append(out, a)
		}
	}
	return out
}

// clockText says when a run is due as a clock time a person can plan around:
// "14:00 today", "09:30 tomorrow", "Mon 14:00", or a date further out.
func clockText(t, now time.Time) string {
	if t.IsZero() {
		return "—"
	}
	if !t.After(now) {
		return "due now"
	}
	t, now = t.Local(), now.Local()
	day := func(x time.Time) time.Time { y, m, d := x.Date(); return time.Date(y, m, d, 0, 0, 0, 0, x.Location()) }
	days := int(day(t).Sub(day(now)).Hours()/24 + 0.5)
	switch {
	case days == 0:
		return t.Format("15:04") + " today"
	case days == 1:
		return t.Format("15:04") + " tomorrow"
	case days < 7:
		return t.Format("Mon 15:04")
	default:
		return t.Format("2 Jan 15:04")
	}
}

// glanceShown is how many bars each group shows before "and n more".
const glanceShown = 6

// glance is what the estate is made of: hosts by operating system family, by
// stack and by application, each a short list of proportional bars.
func (s *Server) glance(ctx context.Context, hosts int) []glanceGroup {
	fam := glanceGroup{Title: "Operating system", MoreHref: "/charts", Empty: "No host has reported one yet."}
	if bs, err := s.Store.Distribution(ctx, "os.family"); err == nil {
		fam.Rows, fam.More = glanceRows("os.family", bs, hosts, "not reported")
	}
	stacks := glanceGroup{Title: "Stack", MoreHref: "/stacks", Empty: "No stacks yet."}
	if bs, err := s.Store.StackSizes(ctx); err == nil {
		for i, b := range bs {
			if i == glanceShown {
				stacks.More = len(bs) - glanceShown
				break
			}
			stacks.Rows = append(stacks.Rows, glanceRow{Label: b.Label, Count: b.Count,
				Pct: pct(b.Count, hosts), Href: "/hosts?stack=" + url.QueryEscape(b.Key)})
		}
	}
	apps := glanceGroup{Title: "Application", MoreHref: "/charts", Empty: "No applications recorded yet."}
	if bs, err := s.Store.Distribution(ctx, model.AttrApplication); err == nil {
		apps.Rows, apps.More = glanceRows(model.AttrApplication, bs, hosts, "not recorded")
	}
	return []glanceGroup{fam, stacks, apps}
}

// glanceRows turns a distribution into bars. The causes of a missing value
// are folded into one bar at the end: the overview says how many have none,
// and the inventory it links to says why.
func glanceRows(attr string, bs []store.Bucket, hosts int, none string) ([]glanceRow, int) {
	var rows []glanceRow
	missing, more := 0, 0
	for _, b := range bs {
		if b.Count <= 0 {
			continue
		}
		if b.Unknown && b.Key != string(model.UnknownUnparsed) {
			missing += b.Count
			continue
		}
		if len(rows) == glanceShown {
			more++
			continue
		}
		rows = append(rows, glanceRow{Label: b.Label, Count: b.Count, Pct: pct(b.Count, hosts),
			Href: bucketURL(attr, b), Unknown: b.Unknown || b.Key == varies})
	}
	if len(rows) == 0 {
		// Nothing reported at all says less than the empty message does.
		return nil, more
	}
	if missing > 0 {
		rows = append(rows, glanceRow{Label: none, Count: missing, Pct: pct(missing, hosts), Unknown: true,
			Href: "/hosts?unknown=" + url.QueryEscape(attr)})
	}
	return rows, more
}
