package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/KalebHawkins/hearsay/internal/collect"
	"github.com/KalebHawkins/hearsay/internal/engine"
	"github.com/KalebHawkins/hearsay/internal/ingest"
	"github.com/KalebHawkins/hearsay/internal/store"
)

// catalogRow is one catalog entry as the collectors page shows it.
type catalogRow struct {
	T collect.Template
	// On lists the choices made from this entry: at most one for a fixed
	// entry, one per value for a template.
	On []collect.Choice
	// Allowed is false for an entry that reveals content when the server
	// was not started with permission to offer those.
	Allowed bool
}

type catalogGroup struct {
	Category string
	Rows     []catalogRow
}

// choiceRow is a switched-on collector with what the page needs to show it.
type choiceRow struct {
	C       collect.Choice
	Title   string
	Missing []string // stacks it is limited to that no longer exist
	Reveals bool
}

func (s *Server) catalogData(ctx context.Context) ([]catalogGroup, []choiceRow, []string, error) {
	choices, err := s.Store.Choices(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	views, err := s.Store.Stacks(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	stackNames := make([]string, 0, len(views))
	exists := map[string]bool{}
	for _, v := range views {
		stackNames = append(stackNames, v.Stack.Name)
		exists[v.Stack.Name] = true
	}

	var groups []catalogGroup
	index := map[string]int{}
	for _, t := range collect.Catalog {
		row := catalogRow{T: t, Allowed: !t.Reveals || s.AllowRevealing}
		for _, c := range choices {
			if c.Template == t.ID {
				row.On = append(row.On, c)
			}
		}
		i, ok := index[t.Category]
		if !ok {
			i = len(groups)
			index[t.Category] = i
			groups = append(groups, catalogGroup{Category: t.Category})
		}
		groups[i].Rows = append(groups[i].Rows, row)
	}

	var on []choiceRow
	for _, c := range choices {
		r := choiceRow{C: c, Title: c.Label()}
		if t, ok := collect.Lookup(c.Template); ok {
			r.Reveals = t.Reveals
		}
		for _, st := range c.Stacks {
			if !exists[st] {
				r.Missing = append(r.Missing, st)
			}
		}
		on = append(on, r)
	}
	return groups, on, stackNames, nil
}

// handleChoose switches a catalog collector on, or changes where it runs.
func (s *Server) handleChoose(w http.ResponseWriter, r *http.Request) {
	back := "/collectors"
	t, ok := collect.Lookup(field(r, "template"))
	if !ok {
		refuse(w, r, back, errors.New("no such collector in the catalog"))
		return
	}
	back += "#cat-" + slug(t.ID, map[string]int{})
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
	c, err := s.Store.PutChoice(r.Context(), collect.Choice{
		Template: t.ID, Param: field(r, "param"), Attr: field(r, "attr"), Stacks: stacks,
	}, actorOf(r))
	if err != nil {
		refuse(w, r, back, err)
		return
	}
	where := "every host"
	if len(c.Stacks) > 0 {
		where = "hosts in " + strings.Join(c.Stacks, ", ")
	}
	done(w, r, "/collectors#on", c.Label()+" is on for "+where+". It is recorded as "+c.Attr+
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
	done(w, r, "/collectors#on", c.Label()+" is off. Values it already collected are kept.")
}

type tryData struct {
	Host    string
	HostID  string
	Title   string
	Result  engine.TryResult
	Err     string
	Value   string
	Problem string
}

// handleTry runs one collector against one host and shows the result, saving
// nothing. It runs only commands the page could already cause to run with the
// Collect button: catalog entries that are switched on, and specs loaded from
// files on the server.
func (s *Server) handleTry(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	var specs []collect.Spec
	title := ""
	if v := field(r, "choice"); v != "" {
		id, _ := strconv.ParseInt(v, 10, 64)
		c, err := s.Store.Choice(ctx, id)
		if err != nil {
			refuse(w, r, "/collectors", errors.New("that collector is not switched on"))
			return
		}
		if specs, err = c.Specs(); err != nil {
			refuse(w, r, "/collectors", err)
			return
		}
		title = c.Label()
	} else if id := field(r, "spec"); id != "" && s.Engine != nil && s.Engine.Specs != nil {
		for _, sp := range s.Engine.Specs.Specs {
			if sp.ID == id {
				specs = append(specs, sp)
				title = sp.ID
			}
		}
	}
	if len(specs) == 0 {
		refuse(w, r, "/collectors", errors.New("choose a collector to test"))
		return
	}

	hostRef := field(r, "host")
	ciID, err := s.Store.LookupCI(ctx, hostRef)
	if err != nil {
		refuse(w, r, "/collectors", err)
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
	d := tryData{Host: ci.Label, HostID: ciID, Title: title}
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
		ingest.BulkOptions{Create: true, User: user, By: actorOf(r)})
	if err != nil {
		return "", err
	}
	res.Problems = append(problems, res.Problems...)
	return res.Summary(stack), nil
}
