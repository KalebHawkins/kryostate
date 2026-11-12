package web

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/engine"
	"github.com/KalebHawkins/kryostate/internal/query"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// The stacks page is a table of one row per stack, and each stack has a page
// of its own for its members and what is collected there. Neither draws a
// list that grows with the estate without a cap: the list is filtered on the
// server and capped, members are capped with a link to the inventory, and a
// host is added by typing part of its name.
const (
	// stackListLimit caps the rows of the stacks table.
	stackListLimit = 200
	// memberLimit caps the members table on a stack's page.
	memberLimit = 50
	// roleLimit caps the roles summarised in a row of the stacks table.
	roleLimit = 3
)

// stackURL is the path of a stack's page.
func stackURL(name string) string { return "/stacks/" + url.PathEscape(name) }

// stackInventory is the inventory filtered to a stack's members.
func stackInventory(name string) string {
	return "/hosts?q=" + url.QueryEscape("stack == "+query.Quote(name))
}

// stackBack is where a stacks form returns to. A form may name the list, a
// stack's page or a host's page; anything else, an off-site address above
// all, falls back to def.
func stackBack(back, def string) string {
	if back == "/stacks" {
		return back
	}
	if !strings.HasPrefix(back, "/stacks/") && !strings.HasPrefix(back, "/hosts/") {
		return def
	}
	if strings.Contains(back, "//") || strings.ContainsAny(back, `\?#`) {
		return def
	}
	return back
}

// roleCount is one role in a stack and how many members play it.
type roleCount struct {
	Role  string
	Count int
}

// stackRow is one row of the stacks table.
type stackRow struct {
	View      store.StackView
	Href      string
	Inventory string
	Roles     []roleCount
	MoreRoles int
	// Collectors is how many switched-on catalog collectors run on its
	// members; Here is how many of those are limited to some hosts.
	Collectors int
	Here       int
}

// matchesStack is the stacks page filter: part of a name, environment or
// owner, ignoring case.
func matchesStack(v store.StackView, q string) bool {
	if q == "" {
		return true
	}
	q = strings.ToLower(q)
	for _, s := range []string{v.Stack.Name, v.Stack.Environment, v.Stack.Owner} {
		if strings.Contains(strings.ToLower(s), q) {
			return true
		}
	}
	return false
}

// runsOnStack says whether a collector limited to some hosts runs on a
// stack: when its target names the stack, or matches any live member.
func runsOnStack(tg *engine.Targets, target, stack string, members []string) bool {
	if names, ok := query.ListOf(target, "stack"); ok {
		return slices.Contains(names, stack)
	}
	return slices.ContainsFunc(members, func(id string) bool { return tg.Match(target, id) })
}

func (s *Server) handleStacks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	views, err := s.Store.Stacks(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	choices, err := s.Store.Choices(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	tg, err := s.targetsNow(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	all, err := s.Store.AllMemberships(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	// One pass over every membership gives each stack its live members and
	// the roles they play, instead of a query per stack.
	live := map[string]bool{}
	for _, h := range tg.All() {
		live[h.ID] = true
	}
	members := map[string][]string{}
	roles := map[string]map[string]int{}
	for id, ms := range all {
		if !live[id] {
			continue
		}
		for _, m := range ms {
			members[m.Stack] = append(members[m.Stack], id)
			if m.Role == "" {
				continue
			}
			if roles[m.Stack] == nil {
				roles[m.Stack] = map[string]int{}
			}
			roles[m.Stack][m.Role]++
		}
	}
	everywhere := 0
	for _, c := range choices {
		if c.Target == "" {
			everywhere++
		}
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	var rows []stackRow
	shown := 0
	for _, v := range views {
		if !matchesStack(v, q) {
			continue
		}
		shown++
		if len(rows) == stackListLimit {
			continue
		}
		name := v.Stack.Name
		row := stackRow{View: v, Href: stackURL(name), Inventory: stackInventory(name)}
		for role, n := range roles[name] {
			row.Roles = append(row.Roles, roleCount{role, n})
		}
		slices.SortFunc(row.Roles, func(a, b roleCount) int {
			if c := cmp.Compare(b.Count, a.Count); c != 0 {
				return c
			}
			return cmp.Compare(a.Role, b.Role)
		})
		if len(row.Roles) > roleLimit {
			row.Roles, row.MoreRoles = row.Roles[:roleLimit], len(row.Roles)-roleLimit
		}
		for _, c := range choices {
			if c.Target != "" && runsOnStack(tg, c.Target, name, members[name]) {
				row.Here++
			}
		}
		row.Collectors = row.Here + everywhere
		rows = append(rows, row)
	}
	s.render(w, r, "stacks.html", "Stacks", "stacks", struct {
		Rows  []stackRow
		Q     string
		Shown int // stacks the filter matches
		Total int
		More  int // matched stacks beyond the cap
	}{rows, q, shown, len(views), shown - len(rows)})
}

// memberRow is one member on a stack's page.
type memberRow struct {
	Host store.HostRow
	Role string
}

func (s *Server) handleStack(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	view, err := s.Store.Stack(ctx, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	hosts, err := s.Store.StackMembers(ctx, name)
	if err != nil {
		s.fail(w, err)
		return
	}
	roles, err := s.Store.StackRoles(ctx, name)
	if err != nil {
		s.fail(w, err)
		return
	}
	choices, err := s.Store.Choices(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	tg, err := s.targetsNow(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	ids := make([]string, 0, len(hosts))
	for _, h := range hosts {
		ids = append(ids, h.CI.ID)
	}
	var here, everywhere []collect.Choice
	for _, c := range choices {
		switch {
		case c.Target == "":
			everywhere = append(everywhere, c)
		case runsOnStack(tg, c.Target, name, ids):
			here = append(here, c)
		}
	}
	var members []memberRow
	for _, h := range hosts {
		if len(members) == memberLimit {
			break
		}
		members = append(members, memberRow{Host: h, Role: roles[h.CI.ID]})
	}
	s.render(w, r, "stack.html", name, "stacks", struct {
		View        store.StackView
		Href        string
		Inventory   string
		Members     []memberRow
		MoreMembers int
		Collectors  []collect.Choice // limited to some hosts, and run here
		Everywhere  []collect.Choice // on for every host
		Groups      []collect.Group
		Fixed       []collect.Template // catalog entries that need no value
	}{view, stackURL(name), stackInventory(name), members, len(hosts) - len(members),
		here, everywhere, collect.Groups, fixedEntries()})
}

// handleStackHosts suggests hosts by name for adding to a stack. It reads
// at most pickLimit names, however many hosts there are, and changes
// nothing.
func (s *Server) handleStackHosts(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Store.StackID(r.Context(), r.PathValue("name")); err != nil {
		http.NotFound(w, r)
		return
	}
	d := hostPick{Into: "stack-host"}
	text := strings.TrimSpace(r.URL.Query().Get("host"))
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
		d.Empty = "No host's name contains " + strconv.Quote(text) + ". Add several hosts below to record new ones."
	}
	s.fragment(w, "host-picks", d)
}

// shellWord quotes a value for a POSIX shell when it needs it, for the
// command-line hints.
func shellWord(v string) string {
	if v != "" && strings.Trim(v, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.:,/@+=") == "" {
		return v
	}
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// QueryAdd is what adding the hosts a query matches to a stack would do, or
// did.
type QueryAdd struct {
	Added   []store.HostRow // matching hosts not yet members
	Already int             // matching hosts already members
}

// MatchForStack works out which hosts a query adds to a stack, with the same
// meaning the inventory's search box gives it: a query, or text without any
// query syntax searched for in host names. A blank query is refused, since
// it would match every host. Nothing is changed.
func MatchForStack(ctx context.Context, st *store.Store, stack, expr string) (QueryAdd, error) {
	var out QueryAdd
	if strings.TrimSpace(expr) == "" {
		return out, errors.New("write a query: a blank one would add every host")
	}
	rows, _, err := FilterHosts(ctx, st, store.HostFilter{}, expr)
	if err != nil {
		return out, err
	}
	roles, err := st.StackRoles(ctx, stack)
	if err != nil {
		return out, err
	}
	for _, h := range rows {
		if _, ok := roles[h.CI.ID]; ok {
			out.Already++
			continue
		}
		out.Added = append(out.Added, h)
	}
	return out, nil
}

// AddMatchingToStack adds every host a query matches to an existing stack,
// in one role, once: the stack does not follow the query afterwards. Hosts
// already in the stack keep the role they have.
func AddMatchingToStack(ctx context.Context, st *store.Store, stack, expr, role string) (QueryAdd, error) {
	id, err := st.StackID(ctx, stack)
	if err != nil {
		return QueryAdd{}, err
	}
	res, err := MatchForStack(ctx, st, stack, expr)
	if err != nil {
		return res, err
	}
	for _, h := range res.Added {
		if err := st.AddToStack(ctx, id, h.CI.ID, role); err != nil {
			return res, err
		}
	}
	return res, nil
}

// Summary is the sentence a person is shown after adding by query.
func (q QueryAdd) Summary(stack string) string {
	msg := "Added " + plural(len(q.Added), "host") + " to " + stack
	if q.Already > 0 {
		msg += " (" + strconv.Itoa(q.Already) + " already members)"
	}
	return msg + "."
}

// queryPreview is the live answer of the add-by-query box.
type queryPreview struct {
	Expr       string
	Err        string
	Col        int
	Caret      string
	Count      int // hosts it would add
	Already    int
	Sample     []string
	SampleMore int
	Href       string // the inventory, filtered by the same query
	// OOB swaps the submit button too, so it says how many it adds.
	OOB bool
}

// handleStackQueryPreview answers the add-by-query box as a person types:
// whether the query parses, how many hosts it would add and the first few of
// them. It changes nothing.
func (s *Server) handleStackQueryPreview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	if _, err := s.Store.StackID(ctx, name); err != nil {
		http.NotFound(w, r)
		return
	}
	d := queryPreview{Expr: strings.TrimSpace(r.URL.Query().Get("query")), OOB: true}
	if d.Expr == "" {
		s.fragment(w, "stack-query-preview", d)
		return
	}
	d.Href = "/hosts?q=" + url.QueryEscape(d.Expr)
	res, err := MatchForStack(ctx, s.Store, name, d.Expr)
	var qe *query.Error
	switch {
	case errors.As(err, &qe):
		d.Err, d.Col, d.Caret = qe.Msg, qe.Col, qe.Caret()
	case err != nil:
		s.fail(w, err)
		return
	}
	d.Count, d.Already = len(res.Added), res.Already
	for _, h := range res.Added {
		if len(d.Sample) == pickLimit {
			d.SampleMore = len(res.Added) - pickLimit
			break
		}
		d.Sample = append(d.Sample, h.CI.Label)
	}
	s.fragment(w, "stack-query-preview", d)
}

// handleStackQuery adds every host a query matches to a stack.
func (s *Server) handleStackQuery(w http.ResponseWriter, r *http.Request) {
	stack := field(r, "stack")
	back := stackBack(field(r, "back"), stackURL(stack))
	if _, err := s.Store.StackID(r.Context(), stack); err != nil {
		refuse(w, r, "/stacks", errors.New("no such stack"))
		return
	}
	expr, role := field(r, "query"), field(r, "role")
	res, err := AddMatchingToStack(r.Context(), s.Store, stack, expr, role)
	if err != nil {
		refuse(w, r, back, err)
		return
	}
	if len(res.Added) == 0 && res.Already == 0 {
		refuse(w, r, back, errors.New("the query matches no host"))
		return
	}
	s.audit(r, "add to stack by query", stack, expr)
	done(w, r, back, res.Summary(stack))
}
