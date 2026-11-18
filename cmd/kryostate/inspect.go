package main

// The commands in this file read and settle the inventory from a terminal:
// hosts, show, set and refresh. Everything here was already
// possible through the web interface or by chaining other commands; these
// exist so the common loop does not need a browser or five invocations.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/KalebHawkins/kryostate/internal/engine"
	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/query"
	"github.com/KalebHawkins/kryostate/internal/store"
	"github.com/KalebHawkins/kryostate/internal/web"
)

// shortID is the display form of an opaque id. Any prefix of six or more
// characters is accepted back wherever a host is named.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (a *app) hostList(ctx context.Context, o hostListOpts) error {
	st, err := openStore(a.db)
	if err != nil {
		return err
	}
	defer st.Close()
	rows, members, err := web.FilterHosts(ctx, st, store.HostFilter{Stack: o.stack}, o.query)
	var qe *query.Error
	if errors.As(err, &qe) {
		return fmt.Errorf("query: %s\n  %s\n  %s", qe.Msg, o.query, qe.Caret())
	} else if err != nil {
		return err
	}

	if o.json {
		var jc []string
		if o.colsSet {
			jc = splitIDs(o.cols)
		}
		return writeJSON(web.HostsJSON(rows, members, jc))
	}

	if len(rows) == 0 {
		if o.query != "" || o.stack != "" {
			fmt.Println("no hosts match")
			return nil
		}
		fmt.Println("no hosts yet; add one with: kryostate host add <name>")
		return nil
	}
	columns := splitIDs(o.cols)
	tw := newTable(os.Stdout)
	fmt.Fprint(tw, "NAME\tID")
	for _, c := range columns {
		fmt.Fprintf(tw, "\t%s", strings.ToUpper(c))
	}
	fmt.Fprintln(tw, "\tSTATUS\tCOLLECTED\tSTACKS")
	disabled := 0
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s", r.CI.Label, shortID(r.CI.ID))
		for _, c := range columns {
			v := r.Values[c]
			if v == "" {
				v = "-"
			}
			fmt.Fprintf(tw, "\t%s", v)
		}
		collected := "never"
		if r.HasCollected {
			collected = agoText(r.LastCollect)
			if !r.CollectOK {
				collected += " FAILED"
			}
		}
		status := "enabled"
		if r.CI.DisabledAt != nil {
			status = "disabled"
			disabled++
		}
		fmt.Fprintf(tw, "\t%s\t%s\t%s\n", status, collected, strings.Join(r.Stacks, ","))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if disabled > 0 {
		fmt.Printf("%d hosts, %d of them disabled (not contacted).\n", len(rows), disabled)
		return nil
	}
	fmt.Printf("%d hosts.\n", len(rows))
	return nil
}

func (a *app) hostShow(ctx context.Context, host string, asJSON bool, at string) error {
	if at != "" {
		return showAt(ctx, a.db, host, at, asJSON)
	}

	st, err := openStore(a.db)
	if err != nil {
		return err
	}
	defer st.Close()
	id, err := st.LookupCI(ctx, host)
	if err != nil {
		return err
	}
	ci, err := st.CI(ctx, id)
	if err != nil {
		return err
	}
	facts, err := st.Facts(ctx, id)
	if err != nil {
		return err
	}
	idents, err := st.Identities(ctx, id)
	if err != nil {
		return err
	}
	reach, err := st.Reachability(ctx, id)
	if err != nil {
		return err
	}
	profiles, err := st.ConnProfiles(ctx, id)
	if err != nil {
		return err
	}
	unread, err := st.ReadFailures(ctx, id)
	if err != nil {
		return err
	}

	if asJSON {
		type factJSON struct {
			Attr     string    `json:"attr"`
			Scope    string    `json:"scope,omitempty"`
			Value    string    `json:"value"`
			Raw      string    `json:"raw"`
			Source   string    `json:"source"`
			Kind     string    `json:"source_kind"`
			Observed time.Time `json:"observed_at"`
		}
		var fj []factJSON
		for _, f := range facts {
			fj = append(fj, factJSON{Attr: f.Attr, Scope: f.Scope, Value: f.Norm, Raw: f.Raw,
				Source: f.SourceName, Kind: string(f.SourceKind), Observed: f.ObservedAt})
		}
		return writeJSON(map[string]any{
			"id": ci.ID, "name": ci.Label, "created_at": ci.CreatedAt, "disabled": ci.DisabledAt != nil, "disabled_at": ci.DisabledAt,
			"identities": idents, "facts": fj, "reachability": reach, "unknown": unreadJSON(unread),
		})
	}

	lipgloss.Printf("%s  %s\n", render(os.Stdout, styleTitle, ci.Label), render(os.Stdout, styleFaint, "(id "+ci.ID+")"))
	if ci.DisabledAt != nil {
		state := fmt.Sprintf("DISABLED since %s", ci.DisabledAt.Local().Format("2006-01-02 15:04"))
		if ci.DisabledBy != "" {
			state += " by " + ci.DisabledBy
		}
		if ci.DisabledReason != "" {
			state += ": " + ci.DisabledReason
		}
		field("status", render(os.Stdout, styleWarn, state))
		fmt.Printf("        not contacted; enable with: kryostate host enable %s\n", ci.Label)
	} else {
		field("status", render(os.Stdout, styleGood, "enabled"))
	}
	if len(idents) > 0 {
		var parts []string
		for _, i := range idents {
			parts = append(parts, render(os.Stdout, styleLabel, string(i.Scheme)+"=")+i.Value)
		}
		field("identity", strings.Join(parts, "  "))
	}

	fmt.Println()
	if len(facts) == 0 {
		fmt.Println("nothing asserted about this host yet")
	} else {
		tw := newTable(os.Stdout)
		fmt.Fprintln(tw, "ATTRIBUTE\tVALUE\tSOURCE\tOBSERVED")
		for _, f := range facts {
			attr := f.Attr
			if f.Scope != "" {
				attr += " [" + f.Scope + "]"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s:%s\t%s\n", attr, factValue(f), f.SourceKind, f.SourceName, agoText(f.ObservedAt))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if len(unread) > 0 {
		// The host was asked and could not say, so these are unknown.
		fmt.Println()
		tw := newTable(os.Stdout)
		fmt.Fprintln(tw, "UNKNOWN\tSINCE\tWHY")
		for _, u := range unread {
			attr := u.Attr
			if u.Scope != "" {
				attr += " [" + u.Scope + "]"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", attr, agoText(u.Since), oneLine(u.Error, 70))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if len(reach) > 0 {
		fmt.Println()
		kinds := make([]string, 0, len(reach))
		for k := range reach {
			kinds = append(kinds, string(k))
		}
		sort.Strings(kinds)
		tw := newTable(os.Stdout)
		fmt.Fprintln(tw, "PROBE\tOUTCOME\tWHEN\tDETAIL")
		for _, k := range kinds {
			p := reach[model.ProbeKind(k)]
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", k, p.Outcome, agoText(p.At), oneLine(p.Detail, 60))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if len(profiles) > 0 {
		fmt.Println()
		for _, p := range profiles {
			state := ""
			if !p.Enabled {
				state = " " + render(os.Stdout, styleWarn, "(disabled)")
			}
			field("target", fmt.Sprintf("%s %s@%s%s %s%s", p.Transport, p.Username, p.Address, portSuffix(p.Port),
				render(os.Stdout, styleFaint, p.CredentialRef), state))
		}
	} else {
		fmt.Printf("\nno connection profile; add one with: kryostate target add %s --user <user> [--address <addr>]\n", ci.Label)
	}
	if b, ok, err := st.HostBackoffFor(ctx, id); err != nil {
		return err
	} else if text := engine.BackoffText(b, time.Now(), time.Local); ok && text != "" {
		fmt.Printf("%s (kryostate refresh --only %s still tries it)\n", text, ci.Label)
	}

	return nil
}

// factValue prefers the normalised form and falls back to what the source
// said, so a free-text attribute is shown rather than blanked. When the
// normalised form drops detail, such as a version kept as 9.4 for grouping
// from 9.4.1, the original follows in brackets.
func factValue(f model.Fact) string {
	if f.Norm != "" {
		raw := strings.TrimSpace(f.Raw)
		if raw != "" && raw != f.Norm && len(raw) <= 40 && !strings.ContainsAny(raw, "\r\n") {
			return f.Norm + " (" + raw + ")"
		}
		return f.Norm
	}
	return oneLine(f.Raw, 60)
}

// hostUpdate records what a person may state about a host: the application it
// runs, and the stack it belongs to with its role there. Everything else
// about a host is what the host reports, so no other attribute is accepted.
func (a *app) hostUpdate(ctx context.Context, pos []string, reason string) error {
	const usage = `usage: kryostate host update <host> [application=NAME] [stack=NAME] [role=ROLE] [--reason "note"]`
	if len(pos) < 2 {
		return errors.New(usage)
	}
	var app, stack, role string
	var haveApp, haveStack, haveRole bool
	for _, p := range pos[1:] {
		k, v, ok := strings.Cut(p, "=")
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		if !ok || k == "" {
			return fmt.Errorf("%q is not key=value; %s", p, usage)
		}
		switch k {
		case model.AttrApplication:
			app, haveApp = v, true
		case "stack":
			stack, haveStack = v, true
		case "role":
			role, haveRole = v, true
		default:
			return fmt.Errorf("%q cannot be set by hand: only application, stack and role can; everything else is what the host reports", k)
		}
	}
	if haveStack && stack == "" {
		return errors.New("stack= needs a name; take a host out of a stack with: kryostate stack rm <stack> <host>")
	}

	st, err := openStore(a.db)
	if err != nil {
		return err
	}
	defer st.Close()
	if _, err := buildEngine(st, engineFiles{}, nil); err != nil { // wires the normaliser
		return err
	}
	id, err := st.LookupCI(ctx, pos[0])
	if err != nil {
		return err
	}
	who := actor()
	note := strings.TrimSpace(reason)
	if note == "" {
		note = "set from the command line"
	}

	if haveApp {
		if err := st.SetApplication(ctx, id, app, who, note); err != nil {
			return err
		}
		fmt.Printf("%s: application = %s (stated by %s)\n", pos[0], app, who)
	}

	if haveStack || haveRole {
		ms, err := st.MembershipsOf(ctx, id)
		if err != nil {
			return err
		}
		if !haveStack {
			// A role belongs to a stack. Without one named, the only
			// unambiguous case is a host in exactly one.
			if len(ms) != 1 {
				return fmt.Errorf("%s is in %d stacks; name the one the role is for with stack=NAME", pos[0], len(ms))
			}
			stack = ms[0].Stack
		}
		if !haveRole {
			// Adding a host to a stack it is already in keeps its role there.
			for _, m := range ms {
				if m.Stack == stack {
					role = m.Role
				}
			}
		}
		stackID, err := st.EnsureStack(ctx, stack)
		if err != nil {
			return err
		}
		if err := st.AddToStack(ctx, stackID, id, role); err != nil {
			return err
		}
		detail := stack
		if role != "" {
			detail += " as " + role
		}
		if err := st.Audit(ctx, who, "set stack", id, detail+": "+note); err != nil {
			return err
		}
		fmt.Printf("%s: in stack %s\n", pos[0], detail)
	}
	return nil
}

// refresh is the whole loop in one command: optionally look names up,
// collect, then run the liveness checks. It is what the web
// interface's collect button does, plus the checks.
func (a *app) refresh(ctx context.Context, c collectFlags, resolve, noChecks, quiet bool) error {
	st, err := openStore(a.db)
	if err != nil {
		return err
	}
	defer st.Close()
	eng, err := buildEngine(st, c.files, a.log)
	if err != nil {
		return err
	}
	refs := onlyIDs(c.only)
	ids, err := st.LookupCIs(ctx, refs)
	if err != nil {
		return err
	}
	ids, anyLeft, err := enabledOnly(ctx, st, refs, ids)
	if err != nil || !anyLeft {
		return err
	}

	if resolve {
		sum, err := eng.Resolve(ctx, engine.ResolveOptions{})
		if err != nil {
			return fmt.Errorf("resolve: %w", err)
		}
		fmt.Printf("dns:     %d checked, %d resolved, %d nowhere, %d with several addresses\n",
			sum.Checked, sum.Resolved, sum.Unresolved, sum.Multi)
	}

	// Only the collection itself opens sessions, so only it waits for, and
	// holds off, collections by other processes on the same database.
	release, err := takeLease(ctx, st, "refresh", c.lease)
	if err != nil {
		return err
	}
	sum, err := runCollect(ctx, eng, engine.Options{Only: ids, Concurrency: c.concurrency,
		HostTimeout: c.hostTimeout, Actor: actor()}, quiet)
	release()
	if err != nil {
		return fmt.Errorf("collect: %w", err)
	}
	fmt.Printf("collect: %d targets, %d ok, %d failed\n", sum.Targets, sum.Succeeded, sum.Failed)
	if sum.Targets == 0 {
		fmt.Println("         nothing to collect from; add a connection with: kryostate target add <host> --user <user>")
	}

	if !noChecks {
		results, err := eng.RunChecks(ctx, ids, 16)
		if err != nil {
			return fmt.Errorf("checks: %w", err)
		}
		if len(results) > 0 {
			ok, failed := engine.CheckSummary(results)
			fmt.Printf("checks:  %d ok, %d failed\n", ok, failed)
		}
	}

	return nil
}

// runCollect runs a collection and prints one line per host as it finishes.
func runCollect(ctx context.Context, eng *engine.Engine, opt engine.Options, quiet bool) (store.RunSummary, error) {
	progress := make(chan engine.Progress, 32)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for p := range progress {
			if !quiet {
				fmt.Printf("[%d/%d] %-28s %-10s %s\n", p.Done, p.Total, p.Label, p.Outcome, p.Detail)
			}
		}
	}()
	sum, err := eng.Collect(ctx, opt, progress)
	<-done
	return sum, err
}

func agoText(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// oneLine keeps remote text from breaking a table row.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// portSuffix renders ":port", or nothing when the transport default applies.
func portSuffix(p int) string {
	if p == 0 {
		return ""
	}
	return fmt.Sprintf(":%d", p)
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// unreadJSON lists what a host could not report, for host show --json.
func unreadJSON(fs []store.ReadFailure) []map[string]any {
	out := []map[string]any{}
	for _, f := range fs {
		out = append(out, map[string]any{"attr": f.Attr, "scope": f.Scope, "since": f.Since, "last_at": f.LastAt, "error": f.Error})
	}
	return out
}
