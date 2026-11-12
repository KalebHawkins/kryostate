package web

import (
	"context"
	"errors"
	"net/http"

	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/engine"
	"github.com/KalebHawkins/kryostate/internal/ingest"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// handleChoose switches a catalog collector on, or changes where it runs.
func (s *Server) handleChoose(w http.ResponseWriter, r *http.Request) {
	back := "/collectors"
	t, ok := collect.Lookup(field(r, "template"))
	if !ok {
		refuse(w, r, back, errors.New("no such collector in the catalog"))
		return
	}
	back = collectorURL(t.ID) + "#where"
	if t.Reveals && !s.AllowRevealing {
		refuse(w, r, back, errors.New(t.Title+" shows what is inside files or variables, so it is switched on from the command line, or the server must be started with -allow-content-collectors"))
		return
	}
	if err := r.ParseForm(); err != nil {
		refuse(w, r, back, err)
		return
	}
	var stacks []string
	if r.FormValue("where") == "stacks" {
		stacks = r.Form["stack"]
		if len(stacks) == 0 {
			refuse(w, r, back, errors.New("choose at least one stack, or All hosts"))
			return
		}
	}
	param := field(r, "param")
	if t.Param != nil && t.Param.Config() {
		if field(r, "key") == "" {
			refuse(w, r, back, errors.New("name the key to read, such as serverName"))
			return
		}
		param = collect.ConfigParam(param, field(r, "section"), field(r, "key"))
	}
	c, err := s.Store.PutChoice(r.Context(), collect.Choice{
		Template: t.ID, Param: param, Attr: field(r, "attr"), Stacks: stacks,
	}, actorOf(r))
	if err != nil {
		refuse(w, r, back, err)
		return
	}
	where := "every host"
	if len(c.Stacks) > 0 {
		where = "hosts in " + strings.Join(c.Stacks, ", ")
	}
	done(w, r, collectorURL(t.ID)+"#where", c.Label()+" is on for "+where+". It is recorded as "+c.Attr+
		" from the next collection. Use Test to try it on one host now.")
}

func (s *Server) handleUnchoose(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(field(r, "id"), 10, 64)
	if err != nil {
		refuse(w, r, "/collectors", errors.New("bad collector id"))
		return
	}
	c, err := s.Store.Choice(r.Context(), id)
	if err != nil {
		refuse(w, r, "/collectors", errors.New("that collector is not switched on"))
		return
	}
	if err := s.Store.DeleteChoice(r.Context(), id, actorOf(r)); err != nil {
		refuse(w, r, "/collectors", err)
		return
	}
	done(w, r, collectorURL(c.Template)+"#where", c.Label()+" is off. Values it already collected are kept.")
}

type tryData struct {
	Host    string
	HostID  string
	Title   string
	Result  engine.TryResult
	Err     string
	Value   string
	Problem string
	Hint    string
	Back    string // the page of the collector tested
}

// collectorURL is the page of one collector.
func collectorURL(id string) string { return "/collectors/" + id }

// specURL is the page of the collector a loaded spec belongs to: the built-in
// collector for its attribute, or the spec itself when it is one of yours.
func specURL(sp collect.Spec) string {
	if builtin, err := collect.BuiltinSpecs(); err == nil {
		for _, b := range builtin.Specs {
			if b.ID == sp.ID {
				return collectorURL(collect.RefBuiltin + sp.Attr)
			}
		}
	}
	return collectorURL(collect.RefSpec + sp.ID)
}

// handleTry runs one collector against one host and shows the result, saving
// nothing. It runs only commands the page could already cause to run with the
// Collect button: catalog entries that are switched on, and specs loaded from
// files on the server.
func (s *Server) handleTry(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	var specs []collect.Spec
	title, back := "", "/collectors"
	// The host page offers both kinds in one list, as choice:ID or spec:ID.
	if v := field(r, "collector"); v != "" {
		kind, id, _ := strings.Cut(v, ":")
		if kind == "choice" || kind == "spec" {
			r.Form.Set(kind, id)
		}
	}
	if v := field(r, "choice"); v != "" {
		id, _ := strconv.ParseInt(v, 10, 64)
		c, err := s.Store.Choice(ctx, id)
		if err != nil {
			refuse(w, r, back, errors.New("that collector is not switched on"))
			return
		}
		back = collectorURL(c.Template)
		if specs, err = c.Specs(); err != nil {
			refuse(w, r, back, err)
			return
		}
		title = c.Label()
	} else if ids := r.Form["spec"]; len(ids) > 0 && s.Engine != nil && s.Engine.Specs != nil {
		// A built-in collector is several specs, one per system; the one
		// for the host's system is the one that runs.
		for _, sp := range s.Engine.Specs.Specs {
			if slices.Contains(ids, sp.ID) {
				specs = append(specs, sp)
			}
		}
		if len(specs) > 0 {
			title, back = specs[0].ID, specURL(specs[0])
			if len(specs) > 1 {
				title = attrLabel(specs[0].Attr)
			}
		}
	}
	if len(specs) == 0 {
		refuse(w, r, back, errors.New("choose a collector to test"))
		return
	}

	hostRef := field(r, "host")
	ciID, err := s.Store.LookupCI(ctx, hostRef)
	if err != nil {
		refuse(w, r, back, err)
		return
	}
	targets, err := s.Store.CollectTargets(ctx, "")
	if err != nil {
		s.fail(w, err)
		return
	}
	var prof *store.ConnProfile
	for i := range targets {
		if targets[i].CIID == ciID {
			prof = &targets[i]
			break
		}
	}
	ci, _ := s.Store.CI(ctx, ciID)
	d := tryData{Host: ci.Label, HostID: ciID, Title: title, Back: back}
	if prof == nil {
		d.Err = ci.Label + " has no enabled connection, so nothing can be run on it."
	} else if res, err := s.Engine.TrySpecs(ctx, *prof, specs); err != nil {
		d.Err = err.Error()
	} else {
		d.Result = res
		switch {
		case res.Reading.Err != nil && !res.Reading.Ran:
			d.Problem = "The command failed: " + res.Reading.Err.Error()
		case res.Reading.Err != nil:
			d.Problem = "The command ran, but the rule found nothing in its output."
		default:
			d.Value = res.Reading.Value
		}
		d.Hint = collect.Explain(res.Reading)
	}
	s.audit(r, "try collector", ciID, title)
	s.render(w, r, "try.html", "Test a collector", "collectors", d)
}

// handleBulkHosts adds a pasted list of hosts to a stack.
func (s *Server) handleBulkHosts(w http.ResponseWriter, r *http.Request) {
	stack := field(r, "stack")
	back := "/stacks#stack-" + slug(stack, map[string]int{})
	if stack == "" {
		refuse(w, r, "/stacks", errors.New("name the stack"))
		return
	}
	msg, err := s.bulkAdd(r, stack)
	if err != nil {
		refuse(w, r, back, err)
		return
	}
	done(w, r, back, msg)
}

// bulkAdd reads the hosts and user fields of a form into a stack.
func (s *Server) bulkAdd(r *http.Request, stack string) (string, error) {
	lines, problems := ingest.ParseHostList(r.FormValue("hosts"))
	if len(lines) == 0 {
		if len(problems) > 0 {
			return "", errors.New(strings.Join(problems, "; "))
		}
		return "", errors.New("list at least one host, one per line")
	}
	user := field(r, "user")
	if user != "" {
		if err := plainToken("The user", user); err != nil {
			return "", err
		}
	}
	res, err := ingest.AddHostsToStack(r.Context(), s.Store, stack, lines,
		ingest.BulkOptions{Create: true, User: user, Application: field(r, "application"), By: actorOf(r)})
	if err != nil {
		return "", err
	}
	res.Problems = append(problems, res.Problems...)
	return res.Summary(stack), nil
}

// fixedEntries lists the catalog entries that need no value, which are the
// ones a single button can switch on.
func fixedEntries() []collect.Template {
	var out []collect.Template
	for _, t := range collect.Catalog {
		if t.Fixed() {
			out = append(out, t)
		}
	}
	return out
}

// enableFor switches on a group, or one fixed catalog entry, for the given
// stacks (none meaning every host). It only widens where things run. It
// returns a sentence for the flash message.
func (s *Server) enableFor(r *http.Request, what string, stacks []string) (string, error) {
	var ids []string
	title := ""
	if g, ok := collect.LookupGroup(what); ok {
		ids, title = g.Entries, g.Title
	} else if t, ok := collect.Lookup(what); ok && t.Fixed() {
		ids, title = []string{t.ID}, t.Title
	} else {
		return "", errors.New("choose a group or a collector")
	}
	var on, already []string
	for _, id := range ids {
		c, changed, err := s.Store.EnableChoice(r.Context(), collect.Choice{Template: id, Stacks: stacks}, actorOf(r))
		if err != nil {
			return "", err
		}
		if changed {
			on = append(on, c.Label())
		} else {
			already = append(already, c.Label())
		}
	}
	where := "every host"
	if len(stacks) > 0 {
		where = strings.Join(stacks, ", ")
	}
	msg := title + ": "
	switch {
	case len(on) > 0 && len(already) > 0:
		msg += strings.Join(on, ", ") + " now on for " + where + "; " + strings.Join(already, ", ") + " already covered it."
	case len(on) > 0:
		msg += "on for " + where + "."
	default:
		msg += "already on for " + where + ", nothing changed."
	}
	if len(on) > 0 {
		msg += " Values arrive with the next collection."
	}
	return msg, nil
}

// handleChooseGroup switches on a group of catalog collectors from the
// collectors page.
func (s *Server) handleChooseGroup(w http.ResponseWriter, r *http.Request) {
	back := "/collectors#groups"
	if err := r.ParseForm(); err != nil {
		refuse(w, r, back, err)
		return
	}
	var stacks []string
	if r.FormValue("where") == "stacks" {
		stacks = r.Form["stack"]
		if len(stacks) == 0 {
			refuse(w, r, back, errors.New("choose at least one stack, or All hosts"))
			return
		}
	}
	msg, err := s.enableFor(r, field(r, "group"), stacks)
	if err != nil {
		refuse(w, r, back, err)
		return
	}
	done(w, r, "/collectors#groups", msg)
}

// handleStackCollectors switches a group or collector on for one stack, from
// that stack's card.
func (s *Server) handleStackCollectors(w http.ResponseWriter, r *http.Request) {
	stack := field(r, "stack")
	back := "/stacks#stack-" + slug(stack, map[string]int{})
	if _, err := s.Store.StackID(r.Context(), stack); err != nil {
		refuse(w, r, "/stacks", errors.New("no such stack"))
		return
	}
	msg, err := s.enableFor(r, field(r, "what"), []string{stack})
	if err != nil {
		refuse(w, r, back, err)
		return
	}
	done(w, r, back, msg)
}

// testOption is one entry in a host page's Test list.
type testOption struct{ Value, Label, Group string }

// attrTitles names the attributes catalog collectors record: the catalog's
// own titles, and a short phrase for each switched-on template, such as
// "serverName in server.conf [general]".
func (s *Server) attrTitles(ctx context.Context) map[string]string {
	out := collect.FixedTitles()
	if choices, err := s.Store.Choices(ctx); err == nil {
		for _, c := range choices {
			out[c.Attr] = c.ShortLabel()
		}
	}
	return out
}
