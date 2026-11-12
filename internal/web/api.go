package web

// The JSON API: everything the pages show, for other tools to read. It is
// read-only and versioned under /api/v1, so a script written against it keeps
// working when the pages change. Shapes are documented in openapi.json, which
// a test checks against the routes registered here.
//
// Conventions, the same on every endpoint:
//   - Times are RFC 3339 in UTC.
//   - A list is {"items": [...], "total": n, "limit": n, "offset": n}, with
//     "next" holding the URL of the following page while there is one.
//   - An error is {"error": {"status": n, "message": "..."}} with the same
//     status on the response.
//   - A host is named by its id, an id prefix, or any name it is known by.
//   - Nothing secret is returned: connections show the user and address, never
//     a credential reference or a key.

import (
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/KalebHawkins/kryostate/internal/engine"
	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/query"
	"github.com/KalebHawkins/kryostate/internal/store"
	"github.com/KalebHawkins/kryostate/internal/version"
)

//go:embed openapi.json
var openAPISpec []byte

const (
	apiPrefix       = "/api/v1"
	apiDefaultLimit = 500
	apiMaxLimit     = 5000
)

// apiRoutes are the API's endpoints, registered by routes and listed by the
// index. The OpenAPI test checks that each is described there.
type apiRoute struct {
	Path    string
	Summary string
	handler func(*Server, http.ResponseWriter, *http.Request)
}

// apiRoutes is filled in init: the index handler lists it, which would
// otherwise make its initialisation refer to itself.
var apiRoutes []apiRoute

func init() {
	apiRoutes = []apiRoute{
		{"/api/v1", "This list", (*Server).apiIndex},
		{"/api/v1/openapi.json", "OpenAPI 3.1 description of every endpoint", (*Server).apiOpenAPI},
		{"/api/v1/status", "Estate health: how many hosts were collected, failed, are stale, or never collected", (*Server).apiStatus},
		{"/api/v1/hosts", "Hosts, filtered by a query, with their values", (*Server).apiHosts},
		{"/api/v1/hosts/{host}", "One host: every value with its source, what is unknown, stacks, identities, reachability", (*Server).apiHost},
		{"/api/v1/hosts/{host}/changes", "What changed on one host", (*Server).apiHostChanges},
		{"/api/v1/stacks", "Stacks with member counts and health", (*Server).apiStacks},
		{"/api/v1/stacks/{stack}", "One stack and its members with their roles", (*Server).apiStack},
		{"/api/v1/attributes", "Every attribute recorded, with how many hosts have it", (*Server).apiAttributes},
		{"/api/v1/attributes/{attr}", "How many hosts hold each value of one attribute, unknowns included", (*Server).apiAttribute},
		{"/api/v1/changes", "Values that changed, newest first", (*Server).apiChanges},
		{"/api/v1/rules", "Rollout rules with done, not yet and unknown counts", (*Server).apiRules},
		{"/api/v1/rules/{rule}", "One rule with the hosts in each bucket and its history", (*Server).apiRule},
		{"/api/v1/collectors", "What is collected, on what schedule, and when it last ran", (*Server).apiCollectors},
		{"/api/v1/runs", "Recent collection runs", (*Server).apiRuns},
	}
}

func (s *Server) apiRoutesOn(mux *http.ServeMux) {
	for _, rt := range apiRoutes {
		h := rt.handler
		pattern := "GET " + rt.Path
		if rt.Path == apiPrefix {
			pattern = "GET " + apiPrefix + "/{$}"
			mux.HandleFunc("GET "+apiPrefix, func(w http.ResponseWriter, r *http.Request) { h(s, w, r) })
		}
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) { h(s, w, r) })
	}
	// Anything else under the prefix is a JSON 404, not the HTML one.
	mux.HandleFunc(apiPrefix+"/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			apiError(w, http.StatusMethodNotAllowed, "the API is read-only")
			return
		}
		apiError(w, http.StatusNotFound, "no such endpoint; GET "+apiPrefix+" lists them")
	})
}

// ---- Responses ----

func apiJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func apiOK(w http.ResponseWriter, v any) { apiJSON(w, http.StatusOK, v) }

type apiErr struct {
	Status  int    `json:"status"`
	Message string `json:"message"`
}

func apiError(w http.ResponseWriter, status int, msg string) {
	apiJSON(w, status, map[string]apiErr{"error": {Status: status, Message: msg}})
}

func (s *Server) apiFail(w http.ResponseWriter, err error) {
	var qe *query.Error
	switch {
	case errors.As(err, &qe):
		apiError(w, http.StatusBadRequest, "query: "+qe.Error())
	case errors.Is(err, errAPIBadRequest):
		apiError(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), errAPIBadRequest.Error()+": "))
	default:
		if s.Log != nil {
			s.Log.Error("api", "err", err)
		}
		apiError(w, http.StatusInternalServerError, "something went wrong reading the inventory")
	}
}

var errAPIBadRequest = errors.New("bad request")

func badRequest(msg string) error { return errors.Join(errAPIBadRequest, errors.New(msg)) }

// apiList is the shape of every list.
type apiList[T any] struct {
	Items  []T    `json:"items"`
	Total  int    `json:"total"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
	Next   string `json:"next,omitempty"`
}

// page cuts one page out of items according to limit and offset in the URL.
func page[T any](r *http.Request, items []T) (apiList[T], error) {
	q := r.URL.Query()
	limit, offset := apiDefaultLimit, 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > apiMaxLimit {
			return apiList[T]{}, badRequest("limit must be a number from 1 to " + strconv.Itoa(apiMaxLimit))
		}
		limit = n
	}
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return apiList[T]{}, badRequest("offset must be a number, 0 or more")
		}
		offset = n
	}
	out := apiList[T]{Total: len(items), Limit: limit, Offset: offset, Items: []T{}}
	if offset < len(items) {
		end := min(len(items), offset+limit)
		out.Items = items[offset:end]
		if end < len(items) {
			nq := r.URL.Query()
			nq.Set("offset", strconv.Itoa(end))
			nq.Set("limit", strconv.Itoa(limit))
			out.Next = r.URL.Path + "?" + nq.Encode()
		}
	}
	return out, nil
}

func utc(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// sinceParam reads a time from a URL: a date, an RFC 3339 time, or a
// duration back from now such as 24h or 7d.
func sinceParam(v string, def time.Time) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return def, nil
	}
	if strings.HasSuffix(v, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(v, "d")); err == nil && n >= 0 {
			return time.Now().AddDate(0, 0, -n), nil
		}
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return time.Now().Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation(time.DateOnly, v, time.Local); err == nil {
		return t, nil
	}
	return time.Time{}, badRequest("since takes a date (2026-11-01), a time (2026-11-01T08:00:00Z) or an age (24h, 7d)")
}

func (s *Server) apiHostRef(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, err := s.Store.LookupCI(r.Context(), r.PathValue("host"))
	if err != nil {
		apiError(w, http.StatusNotFound, err.Error())
		return "", false
	}
	return id, true
}

// ---- Index and description ----

func (s *Server) apiIndex(w http.ResponseWriter, r *http.Request) {
	type ep struct {
		Path    string `json:"path"`
		Summary string `json:"summary"`
	}
	var eps []ep
	for _, rt := range apiRoutes {
		eps = append(eps, ep{rt.Path, rt.Summary})
	}
	apiOK(w, map[string]any{
		"name": "KryoState", "api": "v1", "version": version.Short(),
		"read_only": true, "endpoints": eps,
		"docs": "/guide/api", "openapi": apiPrefix + "/openapi.json",
	})
}

func (s *Server) apiOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(openAPISpec)
}

// ---- Status ----

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stale := s.staleAfter(ctx)
	h, err := s.Store.EstateHealth(ctx, stale)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	out := map[string]any{
		"hosts": h.Hosts, "ok": h.OK, "failing": h.Failing, "stale": h.Stale,
		"never_collected": h.Never, "no_connection": h.NoConnection,
		"stale_after_hours": int(stale.Hours()),
		"healthy":           h.Failing == 0 && h.Stale == 0 && h.Never == 0 && h.NoConnection == 0,
	}
	if runs, err := s.Store.Runs(ctx, 1); err == nil && len(runs) > 0 {
		out["last_run"] = apiRunOf(runs[0])
	}
	apiOK(w, out)
}

// ---- Hosts ----

func (s *Server) apiHosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	if v := q.Get("retired"); v == "true" {
		q.Set("retired", "1")
	}
	rows, members, err := FilterHosts(ctx, s.Store, s.hostFilter(ctx, q), q.Get("q"))
	if err != nil {
		s.apiFail(w, err)
		return
	}
	// fields chooses the values returned; without it every value is.
	var cols []string
	if f := q.Get("fields"); f != "" {
		for _, c := range strings.Split(f, ",") {
			if c = strings.TrimSpace(c); c != "" {
				cols = append(cols, c)
			}
		}
	}
	list, err := page(r, HostsJSON(rows, members, cols))
	if err != nil {
		s.apiFail(w, err)
		return
	}
	apiOK(w, list)
}

type apiFact struct {
	Attr        string     `json:"attr"`
	Scope       string     `json:"scope,omitempty"`
	Value       string     `json:"value"`
	Raw         string     `json:"raw"`
	Source      string     `json:"source"`
	SourceKind  string     `json:"source_kind"`
	ObservedAt  *time.Time `json:"observed_at"`
	ConfirmedAt *time.Time `json:"confirmed_at"`
}

func apiFacts(fs []model.Fact) []apiFact {
	out := make([]apiFact, 0, len(fs))
	for _, f := range fs {
		v := f.Norm
		if v == "" {
			v = f.Raw
		}
		out = append(out, apiFact{Attr: f.Attr, Scope: f.Scope, Value: v, Raw: f.Raw, Source: f.SourceName,
			SourceKind: string(f.SourceKind), ObservedAt: utc(f.ObservedAt), ConfirmedAt: utc(f.ConfirmedAt)})
	}
	return out
}

type apiUnknown struct {
	Attr   string     `json:"attr"`
	Scope  string     `json:"scope,omitempty"`
	Since  *time.Time `json:"since"`
	LastAt *time.Time `json:"last_tried"`
	Error  string     `json:"error"`
}

func (s *Server) apiHost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := s.apiHostRef(w, r)
	if !ok {
		return
	}
	ci, err := s.Store.CI(ctx, id)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	var facts []model.Fact
	var at *time.Time
	if v := r.URL.Query().Get("at"); v != "" {
		t, err := ParseAt(v)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		at = utc(t)
		facts, err = s.Store.FactsAt(ctx, id, t)
		if err != nil {
			s.apiFail(w, err)
			return
		}
	} else if facts, err = s.Store.Facts(ctx, id); err != nil {
		s.apiFail(w, err)
		return
	}
	out := map[string]any{
		"id": ci.ID, "name": ci.Label, "created_at": utc(ci.CreatedAt),
		"retired_at": nil, "facts": apiFacts(facts), "url": "/hosts/" + ci.ID,
	}
	if ci.RetiredAt != nil {
		out["retired_at"] = utc(*ci.RetiredAt)
	}
	if at != nil {
		out["as_of"] = at
	} else {
		unknown := []apiUnknown{}
		if fs, err := s.Store.ReadFailures(ctx, id); err == nil {
			for _, f := range fs {
				unknown = append(unknown, apiUnknown{Attr: f.Attr, Scope: f.Scope, Since: utc(f.Since), LastAt: utc(f.LastAt), Error: f.Error})
			}
		}
		out["unknown"] = unknown
	}
	stacks := []StackRoleJSON{}
	if ms, err := s.Store.MembershipsOf(ctx, id); err == nil {
		for _, m := range ms {
			stacks = append(stacks, StackRoleJSON{Stack: m.Stack, Role: m.Role})
		}
	}
	out["stacks"] = stacks
	type ident struct {
		Scheme   string     `json:"scheme"`
		Value    string     `json:"value"`
		LastSeen *time.Time `json:"last_seen"`
	}
	idents := []ident{}
	if is, err := s.Store.Identities(ctx, id); err == nil {
		for _, i := range is {
			idents = append(idents, ident{string(i.Scheme), i.Value, utc(i.LastSeen)})
		}
	}
	out["identities"] = idents
	type probe struct {
		Outcome string     `json:"outcome"`
		At      *time.Time `json:"at"`
		Detail  string     `json:"detail,omitempty"`
	}
	reach := map[string]probe{}
	if rs, err := s.Store.Reachability(ctx, id); err == nil {
		for k, p := range rs {
			reach[string(k)] = probe{string(p.Outcome), utc(p.At), p.Detail}
		}
	}
	out["reachability"] = reach
	// Where it is collected from, without the credential reference or keys.
	type conn struct {
		Transport string `json:"transport"`
		Address   string `json:"address"`
		Port      int    `json:"port"`
		User      string `json:"user"`
		Enabled   bool   `json:"enabled"`
	}
	conns := []conn{}
	if ps, err := s.Store.ConnProfiles(ctx, id); err == nil {
		for _, p := range ps {
			conns = append(conns, conn{p.Transport, p.Address, p.Port, p.Username, p.Enabled})
		}
	}
	out["connections"] = conns
	apiOK(w, out)
}

type apiChange struct {
	HostID   string     `json:"host_id"`
	Host     string     `json:"host"`
	Attr     string     `json:"attr"`
	Scope    string     `json:"scope,omitempty"`
	Old      string     `json:"old"`
	New      string     `json:"new"`
	OldAt    *time.Time `json:"old_at"`
	At       *time.Time `json:"at"`
	Source   string     `json:"source"`
	Trigger  string     `json:"trigger,omitempty"`
	RunID    string     `json:"run_id,omitempty"`
	HostLink string     `json:"url"`
}

func apiChanges(cs []store.Change) []apiChange {
	out := make([]apiChange, 0, len(cs))
	for _, c := range cs {
		out = append(out, apiChange{HostID: c.CIID, Host: c.Host, Attr: c.Attr, Scope: c.Scope, Old: c.Old, New: c.New,
			OldAt: utc(c.OldAt), At: utc(c.At), Source: c.SourceName, Trigger: c.Trigger, RunID: c.RunID,
			HostLink: "/hosts/" + c.CIID})
	}
	return out
}

func (s *Server) changesFor(w http.ResponseWriter, r *http.Request, ciID string) {
	q := r.URL.Query()
	since, err := sinceParam(q.Get("since"), time.Now().AddDate(0, 0, -7))
	if err != nil {
		s.apiFail(w, err)
		return
	}
	cs, err := s.Store.Changes(r.Context(), store.ChangeFilter{Since: since, CIID: ciID, Attr: q.Get("attr"), Limit: apiMaxLimit})
	if err != nil {
		s.apiFail(w, err)
		return
	}
	list, err := page(r, apiChanges(cs))
	if err != nil {
		s.apiFail(w, err)
		return
	}
	apiOK(w, list)
}

func (s *Server) apiHostChanges(w http.ResponseWriter, r *http.Request) {
	id, ok := s.apiHostRef(w, r)
	if !ok {
		return
	}
	s.changesFor(w, r, id)
}

func (s *Server) apiChanges(w http.ResponseWriter, r *http.Request) {
	ciID := ""
	if h := r.URL.Query().Get("host"); h != "" {
		id, err := s.Store.LookupCI(r.Context(), h)
		if err != nil {
			apiError(w, http.StatusNotFound, err.Error())
			return
		}
		ciID = id
	}
	s.changesFor(w, r, ciID)
}

// ---- Stacks ----

type apiStackSummary struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Owner       string `json:"owner,omitempty"`
	Environment string `json:"environment,omitempty"`
	Members     int    `json:"members"`
	Collected   int    `json:"collected"`
	Failed      int    `json:"failed"`
	NeverSeen   int    `json:"never_collected"`
	URL         string `json:"url"`
}

func apiStackOf(v store.StackView) apiStackSummary {
	return apiStackSummary{Name: v.Stack.Name, Description: v.Stack.Description, Owner: v.Stack.Owner,
		Environment: v.Stack.Environment, Members: v.Health.Members, Collected: v.Health.Collected,
		Failed: v.Health.Failed, NeverSeen: v.Health.NeverSeen, URL: "/stacks/" + url.PathEscape(v.Stack.Name)}
}

func (s *Server) apiStacks(w http.ResponseWriter, r *http.Request) {
	vs, err := s.Store.Stacks(r.Context())
	if err != nil {
		s.apiFail(w, err)
		return
	}
	out := make([]apiStackSummary, 0, len(vs))
	for _, v := range vs {
		out = append(out, apiStackOf(v))
	}
	list, err := page(r, out)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	apiOK(w, list)
}

func (s *Server) apiStack(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("stack")
	v, err := s.Store.Stack(ctx, name)
	if err != nil {
		apiError(w, http.StatusNotFound, "no stack named "+strconv.Quote(name))
		return
	}
	rows, err := s.Store.StackMembers(ctx, name)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	members, err := s.Store.AllMemberships(ctx)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	roles, _ := s.Store.StackRoles(ctx, name)
	type member struct {
		HostJSON
		Role string `json:"role"`
	}
	ms := make([]member, 0, len(rows))
	for _, h := range HostsJSON(rows, members, nil) {
		ms = append(ms, member{HostJSON: h, Role: roles[h.ID]})
	}
	apiOK(w, map[string]any{"stack": apiStackOf(v), "members": ms})
}

// ---- Attributes ----

func (s *Server) apiAttributes(w http.ResponseWriter, r *http.Request) {
	attrs, err := s.Store.Attrs(r.Context())
	if err != nil {
		s.apiFail(w, err)
		return
	}
	type attr struct {
		Attr      string `json:"attr"`
		Label     string `json:"label"`
		Hosts     int    `json:"hosts"`
		Distinct  int    `json:"distinct_values"`
		Chartable bool   `json:"chartable"`
	}
	out := make([]attr, 0, len(attrs))
	for _, a := range attrs {
		label := a.Label
		if label == "" {
			label = a.Attr
		}
		out = append(out, attr{a.Attr, label, a.CICount, a.Distinct, a.Chartable})
	}
	list, err := page(r, out)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	apiOK(w, list)
}

func (s *Server) apiAttribute(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("attr")
	bs, err := s.Store.Distribution(r.Context(), name)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	type bucket struct {
		Value    string `json:"value"`
		Label    string `json:"label"`
		Hosts    int    `json:"hosts"`
		Unknown  bool   `json:"unknown"`
		HostsURL string `json:"hosts_url"`
	}
	out := make([]bucket, 0, len(bs))
	total := 0
	for _, b := range bs {
		v := url.Values{}
		if b.Unknown {
			v.Set("unknown", name)
			if b.Key != "" {
				v.Set("because", b.Key)
			}
		} else {
			v.Set("attr", name)
			v.Set("value", b.Key)
		}
		val := b.Key
		if b.Unknown {
			val = ""
		}
		out = append(out, bucket{Value: val, Label: b.Label, Hosts: b.Count, Unknown: b.Unknown,
			HostsURL: apiPrefix + "/hosts?" + v.Encode()})
		total += b.Count
	}
	apiOK(w, map[string]any{"attr": name, "hosts": total, "values": out})
}

// ---- Rules ----

type apiRuleSummary struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"`
	Description   string     `json:"description,omitempty"`
	Owner         string     `json:"owner,omitempty"`
	Scope         string     `json:"scope"`
	Expect        string     `json:"expect"`
	InScope       int        `json:"in_scope"`
	Done          int        `json:"done"`
	NotYet        int        `json:"not_yet"`
	Unknown       int        `json:"unknown"`
	Percent       int        `json:"percent_done"`
	Complete      bool       `json:"complete"`
	CompleteSince *time.Time `json:"complete_since,omitempty"`
	LastChange    *time.Time `json:"last_change,omitempty"`
	Error         string     `json:"error,omitempty"`
	URL           string     `json:"url"`
}

func percent(done, of int) int {
	if of == 0 {
		return 0
	}
	return done * 100 / of
}

func (s *Server) apiRules(w http.ResponseWriter, r *http.Request) {
	rs, err := s.Store.RuleStatus(r.Context())
	if err != nil {
		s.apiFail(w, err)
		return
	}
	out := make([]apiRuleSummary, 0, len(rs))
	for _, x := range rs {
		out = append(out, apiRuleSummary{ID: x.Rule.ID, Name: x.Rule.Name, Description: x.Rule.Description,
			Owner: x.Rule.Owner, Scope: x.Rule.Scope, Expect: x.Rule.Expect, InScope: x.InScope, Done: x.Done,
			NotYet: x.NotYet, Unknown: x.Unknown, Percent: percent(x.Done, x.InScope), Complete: x.Complete,
			CompleteSince: utc(x.CompleteSince), LastChange: utc(x.LastChange), Error: x.Err,
			URL: "/rules/" + strconv.FormatInt(x.Rule.ID, 10)})
	}
	list, err := page(r, out)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	apiOK(w, list)
}

func (s *Server) apiRule(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ref := r.PathValue("rule")
	var rule store.Rule
	var err error
	if id, perr := strconv.ParseInt(ref, 10, 64); perr == nil {
		rule, err = s.Store.Rule(ctx, id)
	} else {
		rule, err = s.Store.RuleByName(ctx, ref)
	}
	if err != nil {
		apiError(w, http.StatusNotFound, "no rule "+strconv.Quote(ref))
		return
	}
	ev, err := s.Store.EvaluateRuleNow(ctx, rule)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	type host struct {
		ID      string            `json:"id"`
		Name    string            `json:"name"`
		Values  map[string]string `json:"values"`
		Missing []string          `json:"missing,omitempty"`
	}
	hosts := func(hs []store.RuleHost) []host {
		out := make([]host, 0, len(hs))
		for _, h := range hs {
			v := h.Values
			if v == nil {
				v = map[string]string{}
			}
			out = append(out, host{h.ID, h.Name, v, h.Missing})
		}
		return out
	}
	type point struct {
		At      *time.Time `json:"at"`
		InScope int        `json:"in_scope"`
		Done    int        `json:"done"`
		NotYet  int        `json:"not_yet"`
		Unknown int        `json:"unknown"`
	}
	hist := []point{}
	if snaps, err := s.Store.RuleHistory(ctx, rule.ID); err == nil {
		for _, p := range snaps {
			hist = append(hist, point{utc(p.At), p.InScope, p.Done, p.NotYet, p.Unknown})
		}
	}
	c := ev.Counts()
	out := map[string]any{
		"id": rule.ID, "name": rule.Name, "description": rule.Description, "owner": rule.Owner,
		"scope": rule.Scope, "expect": rule.Expect, "attrs": ev.Attrs,
		"in_scope": c.InScope, "done_count": c.Done, "not_yet_count": c.NotYet, "unknown_count": c.Unknown,
		"percent_done": percent(c.Done, c.InScope),
		"done":         hosts(ev.Done), "not_yet": hosts(ev.NotYet), "unknown": hosts(ev.Unknown),
		"history": hist, "url": "/rules/" + strconv.FormatInt(rule.ID, 10),
	}
	if ev.Err != nil {
		out["error"] = ev.Err.Error()
	}
	apiOK(w, out)
}

// ---- Collectors and runs ----

func (s *Server) apiCollectors(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.Engine == nil {
		apiOK(w, apiList[any]{Items: []any{}})
		return
	}
	var plan []engine.Planned
	var err error
	if s.Scheduler != nil {
		plan, err = s.Scheduler.Plan(ctx)
	} else {
		var cs []engine.Collector
		if cs, err = s.Engine.Collectors(ctx); err == nil {
			plan = engine.DefaultPlan(cs)
		}
	}
	if err != nil {
		s.apiFail(w, err)
		return
	}
	targets := map[string]string{}
	if choices, err := s.Store.Choices(ctx); err == nil {
		for _, c := range choices {
			targets[engine.ChoiceKey(c)] = c.Target
		}
	}
	type coll struct {
		Key         string     `json:"key"`
		Kind        string     `json:"kind"`
		Title       string     `json:"title"`
		Attrs       []string   `json:"attrs"`
		Target      string     `json:"target"`
		Schedule    string     `json:"schedule"`
		EverySecs   int64      `json:"every_seconds"`
		At          string     `json:"at,omitempty"`
		Custom      bool       `json:"custom_schedule"`
		LastStarted *time.Time `json:"last_started,omitempty"`
		LastOutcome string     `json:"last_outcome,omitempty"`
		Next        *time.Time `json:"next,omitempty"`
		Running     bool       `json:"running"`
	}
	out := make([]coll, 0, len(plan))
	for _, p := range plan {
		attrs := []string{}
		seen := map[string]bool{}
		for _, sp := range p.Specs {
			if !seen[sp.Attr] {
				seen[sp.Attr] = true
				attrs = append(attrs, sp.Attr)
			}
		}
		sort.Strings(attrs)
		c := coll{Key: p.Key, Kind: p.Kind, Title: p.Title, Attrs: attrs, Target: targets[p.Key],
			Schedule: scheduleText(p.Key, p.Schedule), EverySecs: int64(p.Schedule.Every.Seconds()), At: p.Schedule.At,
			Custom: p.Custom, LastStarted: utc(p.Last), LastOutcome: p.State.Outcome, Running: p.Running}
		if !p.Schedule.Manual() {
			c.Next = utc(p.Next)
		}
		out = append(out, c)
	}
	list, err := page(r, out)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	apiOK(w, list)
}

type apiRun struct {
	ID        string     `json:"id"`
	Collector string     `json:"collector"`
	Trigger   string     `json:"trigger,omitempty"`
	Status    string     `json:"status"`
	Started   *time.Time `json:"started_at"`
	Finished  *time.Time `json:"finished_at,omitempty"`
	Targets   int        `json:"targets"`
	Succeeded int        `json:"succeeded"`
	Failed    int        `json:"failed"`
	Note      string     `json:"note,omitempty"`
}

func apiRunOf(r store.RunSummary) apiRun {
	return apiRun{ID: r.ID, Collector: r.Collector, Trigger: r.Trigger, Status: r.Status, Started: utc(r.Started),
		Finished: utc(r.Finished), Targets: r.Targets, Succeeded: r.Succeeded, Failed: r.Failed, Note: r.Note}
}

func (s *Server) apiRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.Store.Runs(r.Context(), apiMaxLimit)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	out := make([]apiRun, 0, len(runs))
	for _, x := range runs {
		out = append(out, apiRunOf(x))
	}
	list, err := page(r, out)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	apiOK(w, list)
}
