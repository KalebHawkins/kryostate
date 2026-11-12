package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/engine"
	"github.com/KalebHawkins/kryostate/internal/query"
)

// Where a collector runs is a query over the inventory, the way Rundeck node
// filters, Salt compound matchers and Datadog scopes do it: an expression
// over stacks, systems and names, with a live count of what it matches. A
// list of every host, or of every stack, is never drawn; anything that could
// grow with the estate is capped and says how many more there are.
const (
	// pickLimit caps every list of hosts a picker or a preview shows.
	pickLimit = 10
	// chipLimit caps the quick-insert chips for stacks and systems.
	chipLimit = 12
)

// targetView describes where a collector runs, for the page.
type targetView struct {
	Expr string // as stored; blank means every host
	// Kind is how the target reads in words: "all", "stacks" (only stack
	// names), "names" (only host names) or "query".
	Kind  string
	Items []string // the stacks or names, at most chipLimit
	More  int      // how many more stacks or names there are
	// Missing are stacks a stacks target names that no longer exist.
	Missing []string
	Count   int // hosts it matches
	// Skipped is how many of them run a system the collector has no
	// command for, and so are passed over.
	Skipped int
	Href    string // the inventory, filtered to the hosts it matches
	Sample  []string
	// SampleMore is how many matching hosts Sample leaves out.
	SampleMore int
	Err        string
	Col        int
	Caret      string
}

// targetsNow loads every host for evaluating targets on a page.
func (s *Server) targetsNow(ctx context.Context) (*engine.Targets, error) {
	hosts, err := s.Store.QueryHosts(ctx)
	if err != nil {
		return nil, err
	}
	return engine.NewTargets(hosts, s.Store.QueryResolver(ctx)), nil
}

// describeTarget works out what a target means against the hosts there are
// now. systems are the operating system families the collector has commands
// for; stacks are the stacks that exist, to point out ones that do not.
func describeTarget(tg *engine.Targets, expr string, systems []string, stacks map[string]int) targetView {
	v := targetView{Expr: strings.TrimSpace(expr), Kind: "query", Href: "/hosts"}
	switch {
	case v.Expr == "":
		v.Kind = "all"
	default:
		v.Href = "/hosts?q=" + url.QueryEscape(inventoryQuery(v.Expr))
		if names, ok := query.ListOf(v.Expr, "stack"); ok {
			v.Kind = "stacks"
			v.Items, v.More = capList(names, chipLimit)
			for _, n := range names {
				if _, ok := stacks[n]; stacks != nil && !ok {
					v.Missing = append(v.Missing, n)
				}
			}
		} else if names, ok := query.ListOf(v.Expr, "name"); ok {
			v.Kind = "names"
			v.Items, v.More = capList(names, chipLimit)
		}
	}
	if err := tg.Err(v.Expr); err != nil {
		v.Err = err.Error()
		var qe *query.Error
		if errors.As(err, &qe) {
			v.Err, v.Col, v.Caret = qe.Msg, qe.Col, qe.Caret()
		}
		return v
	}
	for _, h := range tg.Hosts(v.Expr) {
		v.Count++
		if fam := h.Values["os.family"]; fam != "" && len(systems) > 0 &&
			!slices.ContainsFunc(systems, func(o string) bool { return strings.EqualFold(o, fam) }) {
			v.Skipped++
			continue
		}
		if len(v.Sample) < pickLimit {
			v.Sample = append(v.Sample, h.Name)
		} else {
			v.SampleMore++
		}
	}
	return v
}

// inventoryQuery is a target as the inventory's q parameter, which reads
// text with no query syntax as a name search: a bare yes/no attribute is
// bracketed so it stays a query.
func inventoryQuery(expr string) string {
	if query.LooksLikeQuery(expr) {
		return expr
	}
	return "(" + expr + ")"
}

func capList(in []string, n int) ([]string, int) {
	if len(in) <= n {
		return in, 0
	}
	return in[:n], len(in) - n
}

// targetChips are the quick-insert buttons of a target editor: some stacks
// and the systems hosts report, each capped.
type targetChips struct {
	Stacks     []string
	MoreStacks int
	Systems    []string
	MoreSys    int
}

func chipsFor(tg *engine.Targets, stacks []string) targetChips {
	var c targetChips
	c.Stacks, c.MoreStacks = capList(stacks, chipLimit)
	seen := map[string]bool{}
	var fams []string
	for _, h := range tg.All() {
		if f := h.Values["os.family"]; f != "" && !seen[f] {
			seen[f] = true
			fams = append(fams, f)
		}
	}
	slices.Sort(fams)
	c.Systems, c.MoreSys = capList(fams, chipLimit)
	return c
}

// handleTargetPreview answers the target editor as a person types: whether
// the query parses, and if so how many hosts it matches and the first few of
// them. It changes nothing.
func (s *Server) handleTargetPreview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tg, err := s.targetsNow(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	var systems []string
	if t, ok := collect.Lookup(r.URL.Query().Get("id")); ok {
		systems = t.Systems()
	}
	_, members, err := s.stackNames(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.fragment(w, "target-preview", describeTarget(tg, r.URL.Query().Get("target"), systems, members))
}

// hostPick is one suggestion of a host picker.
type hostPick struct {
	Into  string // the input the pick goes into, as a CSS id
	Hosts []string
	More  int
	Note  string
	Empty string
	// Add is true for the target editor, where a pick adds the host to a
	// name in (...) list; otherwise a pick fills in the input.
	Add bool
}

var reInto = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// handleTargetHosts suggests hosts by name for adding to a target. It reads
// at most pickLimit names, however many hosts there are.
func (s *Server) handleTargetHosts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	into := q.Get("into")
	if !reInto.MatchString(into) {
		http.Error(w, "bad into", http.StatusBadRequest)
		return
	}
	d := hostPick{Into: into, Add: true}
	text := strings.TrimSpace(q.Get("find"))
	if text == "" {
		s.fragment(w, "host-picks", d)
		return
	}
	hosts, total, err := s.Store.FindHosts(r.Context(), text, pickLimit)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, h := range hosts {
		d.Hosts = append(d.Hosts, h.Name)
	}
	d.More = total - len(hosts)
	if total == 0 {
		d.Empty = "No host's name contains " + strconv.Quote(text) + "."
	}
	s.fragment(w, "host-picks", d)
}

// handleTryHosts suggests hosts to test a collector on: those it targets that
// have a connection, by name. When it targets none of those, every host with
// a connection is offered, and the list says so.
func (s *Server) handleTryHosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	into := q.Get("into")
	if !reInto.MatchString(into) {
		http.Error(w, "bad into", http.StatusBadRequest)
		return
	}
	d := hostPick{Into: into}
	// What the collector targets: a switched-on catalog entry, or specs
	// loaded from files. Unknown ids target every host.
	where := ""
	if v := q.Get("choice"); v != "" {
		id, _ := strconv.ParseInt(v, 10, 64)
		if c, err := s.Store.Choice(ctx, id); err == nil {
			where = c.Target
		}
	} else if ids := q["spec"]; len(ids) > 0 && s.Engine != nil && s.Engine.Specs != nil {
		for _, sp := range s.Engine.Specs.Specs {
			if slices.Contains(ids, sp.ID) {
				where = engine.SpecTarget(sp)
				break
			}
		}
	}
	tg, err := s.targetsNow(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	profiles, err := s.Store.CollectTargets(ctx, "")
	if err != nil {
		s.fail(w, err)
		return
	}
	connected := map[string]bool{}
	for _, p := range profiles {
		connected[p.CIID] = true
	}
	text := strings.ToLower(strings.TrimSpace(q.Get("host")))
	pick := func(only func(id string) bool) (names []string, total int) {
		for _, h := range tg.All() {
			if !connected[h.ID] || !only(h.ID) || !strings.Contains(strings.ToLower(h.Name), text) {
				continue
			}
			total++
			if len(names) < pickLimit {
				names = append(names, h.Name)
			}
		}
		return names, total
	}
	only := func(id string) bool { return tg.Match(where, id) }
	if where != "" && len(connected) > 0 && countWhere(tg, connected, only) == 0 {
		d.Note = "It targets no host with a connection, so these are every host with one."
		only = func(string) bool { return true }
	}
	names, total := pick(only)
	d.Hosts, d.More = names, total-len(names)
	if len(connected) == 0 {
		d.Empty = "No host has a connection yet, so there is nothing to test on."
	} else if total == 0 {
		d.Empty = "No host it runs on has a name containing " + strconv.Quote(text) + "."
	}
	s.fragment(w, "host-picks", d)
}

// countWhere counts the connected hosts a test accepts, whatever was typed.
func countWhere(tg *engine.Targets, connected map[string]bool, only func(string) bool) int {
	n := 0
	for _, h := range tg.All() {
		if connected[h.ID] && only(h.ID) {
			n++
		}
	}
	return n
}

// fragment writes one template on its own, for an HTMX swap.
func (s *Server) fragment(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.fail(w, err)
	}
}

// targetForm reads the target a form submitted. Older forms, and the groups
// and stack cards, send where=stacks with stack fields instead.
func targetForm(r *http.Request) (string, error) {
	if t := field(r, "target"); t != "" || r.FormValue("where") != "stacks" {
		return t, nil
	}
	stacks := r.Form["stack"]
	if len(stacks) == 0 {
		return "", errors.New("choose at least one stack, or All hosts")
	}
	return query.In("stack", stacks), nil
}

// whereText says where a collector runs in a few words, for the list.
func whereText(target string) string {
	if target == "" {
		return "All hosts"
	}
	if names, ok := query.ListOf(target, "stack"); ok {
		items, more := capList(names, 3)
		s := strings.Join(items, ", ")
		if more > 0 {
			s += " and " + plural(more, "more stack")
		}
		return s
	}
	if names, ok := query.ListOf(target, "name"); ok {
		return plural(len(names), "host")
	}
	return "Hosts matching a query"
}
