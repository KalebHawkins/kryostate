package web

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/query"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// The inventory is filtered in two steps. The structured parameters that
// links have always used (attr and value, unknown, stack, retired) narrow the
// rows in SQL, with fixed statements. The q parameter is a query in the
// language of package query, and is evaluated in Go over the loaded values:
// nothing a person types is ever turned into SQL.
//
// q that uses none of the query syntax, such as "web01", is a name search, as
// the search box was before there was a language.

type hostsData struct {
	Rows    []store.HostRow
	Attrs   []store.AttrInfo
	Columns []string
	Filter  store.HostFilter
	// Query is the raw query string, so the CSV and JSON links export what
	// is on screen.
	Query template.URL
	// Expr is the q parameter as typed; QErr is set when it does not parse.
	Expr      string
	QErr      *query.Error
	Examples  []queryExample
	Operators []queryOperator
	// HealthLabel describes the health filter, if one is set.
	HealthLabel string
}

type queryExample struct {
	Q    string
	Href string
	Note string
}

var examples = []queryExample{
	{Q: `os.family == "Linux" && !file.exists == "/etc/ssh/sshd_config.d/hardening.conf"`, Note: "Linux hosts without the SSH hardening drop-in"},
	{Q: `stack == "data" && role == "database"`, Note: "The database hosts of the data stack"},
	{Q: `name =~ "^web"`, Note: "Names that start with web"},
	{Q: `name in ("web01", "db01")`, Note: "Two hosts, by name"},
	{Q: `os.version =~ "^Red Hat 7" || os.version == "CentOS 7"`, Note: "Either of two operating systems"},
	{Q: `hw.cpu.cores >= 8 && hw.memory.total_mb < 16000`, Note: "Eight cores or more, under 16 GB of memory"},
	{Q: `!has(os.version)`, Note: "Hosts with no OS version recorded yet"},
}

// queryOperator is one row of the operator table in the inventory's query
// help. Ops are shown as code; Example is a query that uses them.
type queryOperator struct {
	Ops     []string
	Meaning string
	Example string
}

var queryOperators = []queryOperator{
	{[]string{"==", "!="}, "Equal, not equal", `os.family == "Linux"`},
	{[]string{"=~", "!~"}, "Matches a pattern, or does not. Patterns are unanchored: add ^ or $ to anchor", `name =~ "^web"`},
	{[]string{"<", "<=", ">", ">="}, "Order numbers and versions", `hw.cpu.cores >= 8`},
	{[]string{"&&", "||"}, "Both, either", `stack == "web" && role == "app"`},
	{[]string{"!"}, "Not", `!(stack == "web")`},
	{[]string{"( )"}, "Group", `(stack == "web" || stack == "api") && role == "app"`},
	{[]string{"in ( )"}, "Any of a list", `name in ("web01", "db01")`},
	{[]string{"has( )"}, "Has a value at all", `has(hw.serial)`},
	{[]string{"[ ]"}, "A collector that takes a path or a name", `file.exists["/etc/hosts"] == true`},
}

func init() {
	for i := range examples {
		examples[i].Href = "/hosts?q=" + url.QueryEscape(examples[i].Q)
	}
}

// hostFilterFrom reads the structured filters from a URL.
func hostFilterFrom(q url.Values) store.HostFilter {
	f := store.HostFilter{
		Attr:     q.Get("attr"),
		Value:    q.Get("value"),
		Stack:    q.Get("stack"),
		Unknown:  q.Get("unknown"),
		Because:  q.Get("because"),
		Unparsed: q.Get("unparsed"),
		Health:   q.Get("health"),
		Retired:  q.Get("retired") == "1",
	}
	if s := strings.TrimSpace(q.Get("q")); s != "" && !query.LooksLikeQuery(s) {
		f.Search = s
	}
	return f
}

// hostFilter is hostFilterFrom with the stale cutoff the overview counts
// with, so its "out of date" link lists the hosts it counted.
func (s *Server) hostFilter(ctx context.Context, q url.Values) store.HostFilter {
	f := hostFilterFrom(q)
	if f.Health == store.HealthStale {
		f.StaleBefore = time.Now().Add(-s.staleAfter(ctx))
	}
	return f
}

// healthLabels describe the health filter's buckets for the inventory's chip.
var healthLabels = map[string]string{
	store.HealthFailing: "latest collection failed",
	store.HealthStale:   "out of date",
	store.HealthNever:   "never collected",
	store.HealthNoConn:  "no connection set up",
}

// QueryResolver names the attribute a parameterised collector records,
// honouring a name a person chose when switching it on.
func QueryResolver(ctx context.Context, st *store.Store) query.Resolver {
	return st.QueryResolver(ctx)
}

// FilterHosts lists the hosts matching the structured filter and, when expr
// is a query rather than a plain name, the query. It also returns every
// host's stack memberships, which the query and the JSON export both need.
// A query that does not parse is returned as a *query.Error.
func FilterHosts(ctx context.Context, st *store.Store, f store.HostFilter, expr string) ([]store.HostRow, map[string][]store.Membership, error) {
	var e *query.Expr
	if expr = strings.TrimSpace(expr); expr != "" && query.LooksLikeQuery(expr) {
		var err error
		if e, err = query.Compile(expr, QueryResolver(ctx, st)); err != nil {
			return nil, nil, err
		}
	} else if expr != "" && f.Search == "" {
		f.Search = expr
	}
	rows, err := st.Hosts(ctx, f)
	if err != nil {
		return nil, nil, err
	}
	members, err := st.AllMemberships(ctx)
	if err != nil {
		return nil, nil, err
	}
	if e == nil {
		return rows, members, nil
	}
	kept := rows[:0]
	for _, r := range rows {
		if e.Match(queryHost(r, members[r.CI.ID])) {
			kept = append(kept, r)
		}
	}
	return kept, members, nil
}

func queryHost(r store.HostRow, ms []store.Membership) query.Host {
	h := query.Host{ID: r.CI.ID, Name: r.CI.Label, Values: r.Values}
	for _, m := range ms {
		h.Stacks = append(h.Stacks, m.Stack)
		if m.Role != "" {
			h.Roles = append(h.Roles, m.Role)
		}
	}
	return h
}

// inventoryColumns reads the columns, expanding col=* to every attribute.
func (s *Server) inventoryColumns(ctx context.Context, q url.Values) ([]string, []store.AttrInfo, error) {
	attrs, err := s.Store.Attrs(ctx)
	if err != nil {
		return nil, nil, err
	}
	cols := columnsFrom(q)
	if len(cols) == 1 && cols[0] == "*" {
		cols = cols[:0]
		for _, a := range attrs {
			cols = append(cols, a.Attr)
		}
	}
	return cols, attrs, nil
}

func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	f := s.hostFilter(ctx, q)
	d := hostsData{Filter: f, Expr: q.Get("q"), Query: template.URL(r.URL.RawQuery), Examples: examples, Operators: queryOperators}
	d.HealthLabel = healthLabels[f.Health]
	var err error
	if d.Columns, d.Attrs, err = s.inventoryColumns(ctx, q); err != nil {
		s.fail(w, err)
		return
	}
	d.Rows, _, err = FilterHosts(ctx, s.Store, f, d.Expr)
	var qe *query.Error
	if errors.As(err, &qe) {
		d.QErr = qe
	} else if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "hosts.html", "Inventory", "hosts", d)
}

// exportRows loads what an export writes, answering a query that does not
// parse with 400 and the reason.
func (s *Server) exportRows(w http.ResponseWriter, r *http.Request) ([]store.HostRow, map[string][]store.Membership, []string, bool) {
	ctx := r.Context()
	q := r.URL.Query()
	cols, _, err := s.inventoryColumns(ctx, q)
	if err != nil {
		s.fail(w, err)
		return nil, nil, nil, false
	}
	rows, members, err := FilterHosts(ctx, s.Store, s.hostFilter(ctx, q), q.Get("q"))
	var qe *query.Error
	if errors.As(err, &qe) {
		http.Error(w, "query: "+qe.Error(), http.StatusBadRequest)
		return nil, nil, nil, false
	} else if err != nil {
		s.fail(w, err)
		return nil, nil, nil, false
	}
	return rows, members, cols, true
}

// handleHostsCSV writes the inventory view that the same query string shows
// on screen, for the spreadsheet somebody will inevitably want.
func (s *Server) handleHostsCSV(w http.ResponseWriter, r *http.Request) {
	rows, _, cols, ok := s.exportRows(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="kryostate-inventory.csv"`)
	cw := csv.NewWriter(w)
	header := append([]string{"host", "id"}, cols...)
	header = append(header, "stacks", "last_collected", "collect_ok")
	_ = cw.Write(header)
	for _, h := range rows {
		rec := []string{csvSafe(h.CI.Label), h.CI.ID}
		for _, c := range cols {
			rec = append(rec, csvSafe(h.Values[c]))
		}
		last := ""
		if h.HasCollected {
			last = h.LastCollect.UTC().Format(time.RFC3339)
		}
		rec = append(rec, csvSafe(strings.Join(h.Stacks, ";")), last,
			strconv.FormatBool(h.CollectOK))
		_ = cw.Write(rec)
	}
	cw.Flush()
}

// HostJSON is one host in the JSON export, /hosts.json and
// "kryostate hosts -json":
//
//	{
//	  "id": "01J…",
//	  "name": "web01",
//	  "stacks": [{"stack": "web", "role": "web"}],
//	  "application": "nginx",
//	  "values": {"os.version": "RHEL 8.6", "env": null},
//	  "last_collected": "2026-11-12T08:00:00Z",
//	  "collect_ok": true
//	}
//
// values holds the chosen columns, with null for a column the host has no
// value for; col=* chooses every attribute. stacks is always an array, empty
// when the host is in none, and role is "" when none was given. application
// is the application attribute, omitted when there is none; last_collected is
// omitted for a host never collected.
type HostJSON struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Stacks        []StackRoleJSON `json:"stacks"`
	Application   string          `json:"application,omitempty"`
	Values        map[string]any  `json:"values"`
	LastCollected *time.Time      `json:"last_collected,omitempty"`
	CollectOK     bool            `json:"collect_ok"`
}

// StackRoleJSON is a host's membership of one stack.
type StackRoleJSON struct {
	Stack string `json:"stack"`
	Role  string `json:"role"`
}

// HostsJSON shapes rows for export. With cols nil, values holds every value
// the host has and no nulls.
func HostsJSON(rows []store.HostRow, members map[string][]store.Membership, cols []string) []HostJSON {
	out := make([]HostJSON, 0, len(rows))
	for _, r := range rows {
		h := HostJSON{ID: r.CI.ID, Name: r.CI.Label, Stacks: []StackRoleJSON{},
			Application: r.Values[model.AttrApplication], Values: map[string]any{}, CollectOK: r.CollectOK}
		for _, m := range members[r.CI.ID] {
			h.Stacks = append(h.Stacks, StackRoleJSON{Stack: m.Stack, Role: m.Role})
		}
		if cols == nil {
			for k, v := range r.Values {
				h.Values[k] = v
			}
		}
		for _, c := range cols {
			if v, ok := r.Values[c]; ok && v != "" {
				h.Values[c] = v
			} else {
				h.Values[c] = nil
			}
		}
		if r.HasCollected {
			t := r.LastCollect.UTC()
			h.LastCollected = &t
		}
		out = append(out, h)
	}
	return out
}

// handleHostsJSON writes the same view as JSON. See HostJSON for the shape.
func (s *Server) handleHostsJSON(w http.ResponseWriter, r *http.Request) {
	rows, members, cols, ok := s.exportRows(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="kryostate-inventory.json"`)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(HostsJSON(rows, members, cols))
}
