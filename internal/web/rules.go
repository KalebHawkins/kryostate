package web

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/query"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// Rules track a rollout: a scope query chooses the hosts, an expect query
// says what each should report, and the page sorts them into Done, Not yet
// and Unknown from current values. Both queries are evaluated in Go by package
// query, as the inventory's are, and are checked when saved.

// ruleListCap caps each list of hosts on a rule's page.
const ruleListCap = 50

// rolloutsShown is how many open rules the overview lists.
const rolloutsShown = 5

// ruleRow is one rule in the list, or on the overview.
type ruleRow struct {
	ID    int64
	Name  string
	Href  string
	Scope string // in words
	store.RuleResult
	Spark template.HTML
}

// ruleForm is the new-rule and edit forms, with what was typed and why it
// was refused.
type ruleForm struct {
	ID          int64
	Action      string
	Key         string // unique on the page, for element ids
	Name        string
	Description string
	Owner       string
	Scope       string
	Expect      string
	Err         string
	// ErrField is "scope" or "expect" when a query was refused, and
	// Col and Caret point at the problem.
	ErrField string
	Col      int
	Caret    string
}

type rulesData struct {
	Rows []ruleRow
	Open int
	Form ruleForm
	// FormOpen opens the new-rule form, when there are no rules or it was
	// refused.
	FormOpen bool
	CLI      []string
}

func ruleURL(id int64) string { return "/rules/" + strconv.FormatInt(id, 10) }

// scopeText says which hosts a rule covers in a few words.
func scopeText(scope string) string {
	if strings.TrimSpace(scope) == "" {
		return "Every host"
	}
	if names, ok := query.ListOf(scope, "stack"); ok {
		items, more := capList(names, 3)
		s := "Stack " + strings.Join(items, ", ")
		if len(names) > 1 {
			s = "Stacks " + strings.Join(items, ", ")
		}
		if more > 0 {
			s += " and " + plural(more, "more")
		}
		return s
	}
	if names, ok := query.ListOf(scope, "name"); ok {
		return plural(len(names), "host")
	}
	return short(scope, 60)
}

// ruleRows evaluates every rule for the list and the overview.
func (s *Server) ruleRows(ctx context.Context) ([]ruleRow, error) {
	evals, err := s.Store.EvaluateRules(ctx)
	if err != nil || len(evals) == 0 {
		return nil, err
	}
	hist, err := s.Store.RuleHistories(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	rows := make([]ruleRow, 0, len(evals))
	for _, ev := range evals {
		h := hist[ev.Rule.ID]
		row := ruleRow{ID: ev.Rule.ID, Name: ev.Rule.Name, Href: ruleURL(ev.Rule.ID),
			Scope: scopeText(ev.Rule.Scope), RuleResult: store.ResultOf(ev, h)}
		if ev.Err == nil {
			row.Spark = sparkline(h, ev.Counts(), now)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	s.renderRules(w, r, ruleForm{}, http.StatusOK)
}

func (s *Server) renderRules(w http.ResponseWriter, r *http.Request, f ruleForm, status int) {
	rows, err := s.ruleRows(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	f.Action, f.Key = "/rules", "new"
	d := rulesData{Rows: rows, Form: f, FormOpen: len(rows) == 0 || f.Err != ""}
	for _, row := range rows {
		if !row.Complete {
			d.Open++
		}
	}
	d.CLI = []string{
		"kryostate rules",
		`kryostate rules -add -name 'nginx 1.24' -scope 'stack == "web"' -expect 'nginx.version >= "1.24"'`,
	}
	if len(rows) > 0 {
		d.CLI = append(d.CLI, "kryostate rules -show "+shellQuote(rows[0].Name))
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.render(w, r, "rules.html", "Rules", "rules", d)
}

// formRule reads a rule from a submitted form.
func formRule(r *http.Request) store.Rule {
	return store.Rule{
		Name:        field(r, "name"),
		Description: field(r, "description"),
		Owner:       field(r, "owner"),
		Scope:       field(r, "scope"),
		Expect:      field(r, "expect"),
	}
}

// refusedForm keeps what was typed and says why it was refused.
func refusedForm(rule store.Rule, err error) ruleForm {
	f := ruleForm{ID: rule.ID, Name: rule.Name, Description: rule.Description, Owner: rule.Owner,
		Scope: rule.Scope, Expect: rule.Expect, Err: err.Error()}
	var qe *store.RuleQueryError
	if errors.As(err, &qe) {
		f.ErrField, f.Err, f.Col, f.Caret = qe.Field, qe.Err.Msg, qe.Err.Col, qe.Err.Caret()
	}
	return f
}

func (s *Server) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	rule := formRule(r)
	id, err := s.Store.PutRule(r.Context(), rule, actorOf(r))
	if err != nil {
		s.renderRules(w, r, refusedForm(rule, err), http.StatusUnprocessableEntity)
		return
	}
	done(w, r, ruleURL(id), "Created the rule "+rule.Name+".")
}

// ruleHostRow is one host in a rule's lists.
type ruleHostRow struct {
	ID      string
	Name    string
	Missing []string
	Values  []attrValue
}

type attrValue struct{ Attr, Value string }

// ruleList is one of a rule's three lists of hosts.
type ruleList struct {
	Key   string
	Title string
	Note  string
	Count int
	Hosts []ruleHostRow
	More  int
	Href  string
	Empty string
}

// attrNote explains an attribute no host has a value for.
type attrNote struct {
	Attr string
	// Recorded is true when a switched-on or built-in collector records it,
	// so it is waiting for a run rather than for a collector.
	Recorded bool
}

type ruleData struct {
	Rule   store.Rule
	Result store.RuleResult
	Err    string
	Attrs  []string
	Lists  []ruleList
	Notes  []attrNote
	Trend  template.HTML
	Since  time.Time // when the history starts
	Points int
	Edit   ruleForm
	// EditOpen opens the edit form, when a change was refused.
	EditOpen  bool
	ScopeHref string
	CLI       []string
}

func (s *Server) ruleFromPath(w http.ResponseWriter, r *http.Request) (store.Rule, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return store.Rule{}, false
	}
	rule, err := s.Store.Rule(r.Context(), id)
	if errors.Is(err, store.ErrRuleNotFound) {
		http.NotFound(w, r)
		return rule, false
	} else if err != nil {
		s.fail(w, err)
		return rule, false
	}
	return rule, true
}

func (s *Server) handleRule(w http.ResponseWriter, r *http.Request) {
	rule, ok := s.ruleFromPath(w, r)
	if !ok {
		return
	}
	s.renderRule(w, r, rule, ruleForm{}, http.StatusOK)
}

// ruleQueries are the inventory queries behind a rule's three lists. A host
// is Unknown when its answer hangs on a value it has not reported; the
// inventory reads a missing value as empty, so the lists are told apart there
// by which of the attributes the expect query reads are present.
func ruleQueries(r store.Rule, attrs []string) (done, notYet, unknown string) {
	not := "!(" + r.Expect + ")"
	done = query.And(r.Scope, r.Expect)
	var have, lack []string
	for _, a := range attrs {
		have = append(have, "has("+a+")")
		lack = append(lack, "!has("+a+")")
	}
	notYet = query.And(append([]string{r.Scope, not}, have...)...)
	unknown = query.And(r.Scope, not, query.Or(lack...))
	return
}

func (s *Server) renderRule(w http.ResponseWriter, r *http.Request, rule store.Rule, edit ruleForm, status int) {
	ctx := r.Context()
	ev, err := s.Store.EvaluateRuleNow(ctx, rule)
	if err != nil {
		s.fail(w, err)
		return
	}
	hist, err := s.Store.RuleHistory(ctx, rule.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	d := ruleData{Rule: rule, Result: store.ResultOf(ev, hist), Attrs: ev.Attrs, Points: len(hist)}
	d.ScopeHref = "/hosts"
	if rule.Scope != "" {
		d.ScopeHref = "/hosts?q=" + url.QueryEscape(inventoryQuery(rule.Scope))
	}
	if ev.Err != nil {
		d.Err = ev.Err.Error()
	} else {
		dq, nq, uq := ruleQueries(rule, ev.Attrs)
		d.Lists = []ruleList{
			s.ruleList("notyet", "Not yet", "Their values say they are not done.", ev.NotYet, nq, "None: no host in scope is waiting on a change."),
			s.ruleList("unknown", "Unknown", "They have not reported a value the rule needs.", ev.Unknown, uq, "None: every host in scope has reported what the rule reads."),
			s.ruleList("done", "Done", "They match.", ev.Done, dq, "None yet."),
		}
		d.Notes = s.attrNotes(ctx, ev)
		d.Trend = trendChart(hist, ev.Counts(), time.Now())
		if len(hist) > 0 {
			d.Since = hist[0].At
		}
	}
	if edit.Err == "" {
		edit = ruleForm{ID: rule.ID, Name: rule.Name, Description: rule.Description, Owner: rule.Owner,
			Scope: rule.Scope, Expect: rule.Expect}
	} else {
		d.EditOpen = true
	}
	edit.Action, edit.Key = ruleURL(rule.ID), "edit"
	d.Edit = edit
	n := shellQuote(rule.Name)
	d.CLI = []string{"kryostate rules -show " + n, "kryostate rules -show " + n + " -json"}
	if len(ev.NotYet) > 0 {
		_, nq, _ := ruleQueries(rule, ev.Attrs)
		d.CLI = append(d.CLI, "kryostate hosts -q "+shellQuote(nq))
	}
	d.CLI = append(d.CLI, "kryostate rules -delete "+n)
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.render(w, r, "rule.html", rule.Name, "rules", d)
}

func (s *Server) ruleList(key, title, note string, hosts []store.RuleHost, q, empty string) ruleList {
	l := ruleList{Key: key, Title: title, Note: note, Count: len(hosts), Empty: empty,
		Href: "/hosts?q=" + url.QueryEscape(inventoryQuery(q))}
	for i, h := range hosts {
		if i == ruleListCap {
			l.More = len(hosts) - ruleListCap
			break
		}
		row := ruleHostRow{ID: h.ID, Name: h.Name, Missing: h.Missing}
		for a, v := range h.Values {
			row.Values = append(row.Values, attrValue{a, v})
		}
		sort.Slice(row.Values, func(i, j int) bool { return row.Values[i].Attr < row.Values[j].Attr })
		l.Hosts = append(l.Hosts, row)
	}
	return l
}

// attrNotes points out attributes the rule reads that no host in scope has a
// value for, saying whether anything collects them.
func (s *Server) attrNotes(ctx context.Context, ev store.RuleEval) []attrNote {
	if len(ev.Unknown) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for _, list := range [][]store.RuleHost{ev.Done, ev.NotYet} {
		for _, h := range list {
			for a := range h.Values {
				seen[a] = true
			}
		}
	}
	recorded := map[string]bool{}
	if choices, err := s.Store.Choices(ctx); err == nil {
		for _, c := range choices {
			attr := c.Attr
			if attr == "" {
				if t, ok := collect.Lookup(c.Template); ok {
					attr = collect.DefaultAttr(t, c.Param)
				}
			}
			recorded[attr] = true
		}
	}
	if s.Engine != nil && s.Engine.Specs != nil {
		for _, sp := range s.Engine.Specs.Specs {
			recorded[sp.Attr] = true
		}
	}
	var out []attrNote
	for _, a := range ev.Attrs {
		if !seen[a] {
			out = append(out, attrNote{Attr: a, Recorded: recorded[a]})
		}
	}
	return out
}

func (s *Server) handleUpdateRule(w http.ResponseWriter, r *http.Request) {
	old, ok := s.ruleFromPath(w, r)
	if !ok {
		return
	}
	rule := formRule(r)
	rule.ID = old.ID
	if _, err := s.Store.PutRule(r.Context(), rule, actorOf(r)); err != nil {
		s.renderRule(w, r, old, refusedForm(rule, err), http.StatusUnprocessableEntity)
		return
	}
	msg := "Saved the rule " + rule.Name + "."
	if rule.Scope != old.Scope || rule.Expect != old.Expect {
		msg += " Its queries changed, so its history starts again."
	}
	done(w, r, ruleURL(old.ID), msg)
}

func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	rule, ok := s.ruleFromPath(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeleteRule(r.Context(), rule.ID, actorOf(r)); err != nil {
		refuse(w, r, ruleURL(rule.ID), err)
		return
	}
	done(w, r, "/rules", "Deleted the rule "+rule.Name+". No host was changed.")
}

// rulePreview is the live answer under a rule's expect box.
type rulePreview struct {
	Expr  string
	Err   string
	Col   int
	Caret string
	// ScopeErr is set when the scope does not parse, so nothing can be
	// counted.
	ScopeErr bool
	store.RuleCounts
	Attrs []string
}

// handleRulePreview answers a rule form as a person types: for the scope,
// how many hosts it covers; for the expect query, how those split into Done,
// Not yet and Unknown; for the name, whether it is taken. It changes nothing.
func (s *Server) handleRulePreview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	scope, expect := strings.TrimSpace(q.Get("scope")), strings.TrimSpace(q.Get("expect"))
	switch q.Get("which") {
	case "name":
		name := strings.TrimSpace(q.Get("name"))
		taken := ""
		if other, err := s.Store.RuleByName(ctx, name); err == nil && name != "" && strconv.FormatInt(other.ID, 10) != q.Get("id") {
			taken = other.Name
		}
		s.fragment(w, "rule-name-preview", taken)
		return
	case "scope":
		tg, err := s.targetsNow(ctx)
		if err != nil {
			s.fail(w, err)
			return
		}
		_, members, err := s.stackNames(ctx)
		if err != nil {
			s.fail(w, err)
			return
		}
		s.fragment(w, "target-preview", describeTarget(tg, scope, nil, members))
		return
	}
	d := rulePreview{Expr: expect}
	if expect == "" {
		s.fragment(w, "rule-preview", d)
		return
	}
	resolve := s.Store.QueryResolver(ctx)
	e, err := query.Compile(expect, resolve)
	if err != nil {
		var qe *query.Error
		if !errors.As(err, &qe) {
			s.fail(w, err)
			return
		}
		d.Err, d.Col, d.Caret = qe.Msg, qe.Col, qe.Caret()
		s.fragment(w, "rule-preview", d)
		return
	}
	d.Attrs = e.Attrs()
	if scope != "" {
		if _, err := query.Compile(scope, resolve); err != nil {
			d.ScopeErr = true
			s.fragment(w, "rule-preview", d)
			return
		}
	}
	hosts, err := s.Store.QueryHosts(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	d.RuleCounts = store.EvaluateRule(store.Rule{Scope: scope, Expect: expect}, hosts, resolve).Counts()
	s.fragment(w, "rule-preview", d)
}

// rollouts are the open rules for the overview's card, by name, with how
// many more there are and how many rules in all.
type rollouts struct {
	Rows  []ruleRow
	More  int
	Total int
}

func (s *Server) rollouts(ctx context.Context) rollouts {
	rows, err := s.ruleRows(ctx)
	if err != nil {
		s.Log.Error("rollouts", "err", err)
		return rollouts{}
	}
	out := rollouts{Total: len(rows)}
	for _, row := range rows {
		if row.Complete {
			continue
		}
		if len(out.Rows) == rolloutsShown {
			out.More++
			continue
		}
		out.Rows = append(out.Rows, row)
	}
	return out
}
