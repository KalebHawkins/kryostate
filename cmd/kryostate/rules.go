package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/KalebHawkins/kryostate/internal/store"
)

// cmdRules handles "kryostate rules": rules track a rollout, and the counts
// come from what hosts report.
func cmdRules(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rules", flag.ExitOnError)
	db := dbFlag(fs)
	o := rulesOpts{}
	fs.BoolVar(&o.add, "add", false, "create a rule, with -name, -expect and optionally -scope, -description and -owner")
	fs.StringVar(&o.name, "name", "", "with -add: the rule's name")
	fs.StringVar(&o.scope, "scope", "", "with -add: a query choosing the hosts it covers, such as 'stack == \"web\"'; blank for every host")
	fs.StringVar(&o.expect, "expect", "", "with -add: a query each host in scope should match once done, such as 'nginx.version >= \"1.24\"'")
	fs.StringVar(&o.description, "description", "", "with -add: why, a ticket or a deadline")
	fs.StringVar(&o.owner, "owner", "", "with -add: who owns the rollout")
	fs.StringVar(&o.show, "show", "", "list the hosts a rule is waiting on: not yet, and unknown with what is missing")
	fs.StringVar(&o.del, "delete", "", "delete a rule and its history; no host is changed")
	fs.BoolVar(&o.json, "json", false, "write JSON instead of a table")
	fs.Parse(args)
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected %q: put the rule's name after -show, -delete or -name, in quotes if it has spaces", fs.Arg(0))
	}
	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	return runRules(ctx, st, os.Stdout, o)
}

type rulesOpts struct {
	add                                     bool
	name, scope, expect, description, owner string
	show, del                               string
	json                                    bool
}

func runRules(ctx context.Context, st *store.Store, out io.Writer, o rulesOpts) error {
	chosen := 0
	for _, b := range []bool{o.add, o.show != "", o.del != ""} {
		if b {
			chosen++
		}
	}
	if chosen > 1 {
		return errors.New("use only one of -add, -show and -delete")
	}
	if !o.add && (o.name != "" || o.scope != "" || o.expect != "" || o.description != "" || o.owner != "") {
		return errors.New("-name, -scope, -expect, -description and -owner go with -add")
	}
	switch {
	case o.add:
		r := store.Rule{Name: o.name, Scope: o.scope, Expect: o.expect, Description: o.description, Owner: o.owner}
		if _, err := st.RuleByName(ctx, o.name); err == nil {
			return fmt.Errorf("there is already a rule named %q; delete it first, or edit it in the web interface", o.name)
		}
		if _, err := st.PutRule(ctx, r, actor()); err != nil {
			var qe *store.RuleQueryError
			if errors.As(err, &qe) {
				src := r.Scope
				if qe.Field == "expect" {
					src = r.Expect
				}
				return fmt.Errorf("-%s, column %d: %s\n  %s\n  %s", qe.Field, qe.Err.Col, qe.Err.Msg, strings.TrimSpace(src), qe.Err.Caret())
			}
			return err
		}
		fmt.Fprintf(out, "rule %q created.\n", strings.TrimSpace(o.name))
		return showRule(ctx, st, out, o.name, false)
	case o.del != "":
		r, err := st.RuleByName(ctx, o.del)
		if err != nil {
			return ruleErr(err, o.del)
		}
		if err := st.DeleteRule(ctx, r.ID, actor()); err != nil {
			return err
		}
		fmt.Fprintf(out, "rule %q deleted; no host was changed.\n", r.Name)
		return nil
	case o.show != "":
		return showRule(ctx, st, out, o.show, o.json)
	}
	return listRules(ctx, st, out, o.json)
}

func ruleErr(err error, name string) error {
	if errors.Is(err, store.ErrRuleNotFound) {
		return fmt.Errorf("no rule named %q; list them with: kryostate rules", name)
	}
	return err
}

// RuleJSON is one rule in "kryostate rules -json".
type RuleJSON struct {
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
	Complete      bool       `json:"complete"`
	CompleteSince *time.Time `json:"complete_since,omitempty"`
	LastChange    time.Time  `json:"last_change"`
	Error         string     `json:"error,omitempty"`
}

func ruleJSON(r store.RuleResult) RuleJSON {
	j := RuleJSON{ID: r.Rule.ID, Name: r.Rule.Name, Description: r.Rule.Description, Owner: r.Rule.Owner,
		Scope: r.Rule.Scope, Expect: r.Rule.Expect, InScope: r.InScope, Done: r.Done, NotYet: r.NotYet,
		Unknown: r.Unknown, Complete: r.Complete, LastChange: r.LastChange.UTC(), Error: r.Err}
	if !r.CompleteSince.IsZero() {
		t := r.CompleteSince.UTC()
		j.CompleteSince = &t
	}
	return j
}

func listRules(ctx context.Context, st *store.Store, out io.Writer, asJSON bool) error {
	rs, err := st.RuleStatus(ctx)
	if err != nil {
		return err
	}
	if asJSON {
		js := make([]RuleJSON, 0, len(rs))
		for _, r := range rs {
			js = append(js, ruleJSON(r))
		}
		return encodeJSON(out, js)
	}
	if len(rs) == 0 {
		fmt.Fprintln(out, `no rules; create one with: kryostate rules -add -name 'nginx 1.24' -scope 'stack == "web"' -expect 'nginx.version >= "1.24"'`)
		return nil
	}
	tw := newTable(out)
	fmt.Fprintln(tw, "RULE\tDONE\tNOT YET\tUNKNOWN\tIN SCOPE\t%\tSTATUS\tLAST CHANGE")
	for _, r := range rs {
		status := "open"
		switch {
		case r.Err != "":
			status = "error: " + r.Err
		case r.Complete && !r.CompleteSince.IsZero():
			status = "complete since " + r.CompleteSince.Local().Format("2006-01-02")
		case r.Complete:
			status = "complete"
		case r.InScope == 0:
			status = "no host in scope"
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d%%\t%s\t%s\n", r.Rule.Name, r.Done, r.NotYet, r.Unknown,
			r.InScope, r.Pct(), status, r.LastChange.Local().Format("2006-01-02 15:04"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(out, "\nsee the hosts one is waiting on with: kryostate rules -show <name>")
	return nil
}

// RuleHostJSON is one host in "kryostate rules -show <name> -json".
type RuleHostJSON struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Missing []string          `json:"missing,omitempty"`
	Values  map[string]string `json:"values,omitempty"`
}

// RuleDetailJSON is "kryostate rules -show <name> -json": the rule's counts
// and every host in scope, by bucket.
type RuleDetailJSON struct {
	RuleJSON
	Reads      []string       `json:"reads"`
	NotYetList []RuleHostJSON `json:"not_yet_hosts"`
	Unknowns   []RuleHostJSON `json:"unknown_hosts"`
	DoneList   []RuleHostJSON `json:"done_hosts"`
}

func hostsJSON(hs []store.RuleHost) []RuleHostJSON {
	out := make([]RuleHostJSON, 0, len(hs))
	for _, h := range hs {
		j := RuleHostJSON{ID: h.ID, Name: h.Name, Missing: h.Missing}
		if len(h.Values) > 0 {
			j.Values = h.Values
		}
		out = append(out, j)
	}
	return out
}

func showRule(ctx context.Context, st *store.Store, out io.Writer, name string, asJSON bool) error {
	r, err := st.RuleByName(ctx, name)
	if err != nil {
		return ruleErr(err, name)
	}
	ev, err := st.EvaluateRuleNow(ctx, r)
	if err != nil {
		return err
	}
	hist, err := st.RuleHistory(ctx, r.ID)
	if err != nil {
		return err
	}
	res := store.ResultOf(ev, hist)
	if asJSON {
		reads := ev.Attrs
		if reads == nil {
			reads = []string{}
		}
		return encodeJSON(out, RuleDetailJSON{RuleJSON: ruleJSON(res), Reads: reads,
			NotYetList: hostsJSON(ev.NotYet), Unknowns: hostsJSON(ev.Unknown), DoneList: hostsJSON(ev.Done)})
	}
	scope := r.Scope
	if scope == "" {
		scope = "(every host)"
	}
	fmt.Fprintf(out, "%s\n", r.Name)
	if r.Description != "" {
		fmt.Fprintf(out, "  %s\n", r.Description)
	}
	fmt.Fprintf(out, "  scope:  %s\n  expect: %s\n", scope, r.Expect)
	if ev.Err != nil {
		return fmt.Errorf("the rule cannot be evaluated: %v", ev.Err)
	}
	fmt.Fprintf(out, "  %d%% done: %d done, %d not yet, %d unknown, of %d hosts", res.Pct(), res.Done, res.NotYet, res.Unknown, res.InScope)
	if res.Complete {
		fmt.Fprint(out, " (complete)")
	}
	fmt.Fprintln(out)
	if len(ev.NotYet) > 0 {
		fmt.Fprintf(out, "\nNOT YET (%d)\n", len(ev.NotYet))
		tw := newTable(out)
		for _, h := range ev.NotYet {
			var vals []string
			for a, v := range h.Values {
				vals = append(vals, a+" = "+v)
			}
			sort.Strings(vals)
			fmt.Fprintf(tw, "  %s\t%s\n", h.Name, dash(strings.Join(vals, ", ")))
		}
		tw.Flush()
	}
	if len(ev.Unknown) > 0 {
		fmt.Fprintf(out, "\nUNKNOWN (%d)\n", len(ev.Unknown))
		tw := newTable(out)
		for _, h := range ev.Unknown {
			fmt.Fprintf(tw, "  %s\tno value for %s yet\n", h.Name, strings.Join(h.Missing, ", "))
		}
		tw.Flush()
	}
	if len(ev.Done) > 0 {
		fmt.Fprintf(out, "\n%d done; list them with -json.\n", len(ev.Done))
	}
	return nil
}

func encodeJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
