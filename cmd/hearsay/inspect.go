package main

// The commands in this file read and settle the inventory from a terminal:
// hosts, show, set, disagreements and refresh. Everything here was already
// possible through the web interface or by chaining other commands; these
// exist so the common loop does not need a browser or five invocations.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/KalebHawkins/hearsay/internal/engine"
	"github.com/KalebHawkins/hearsay/internal/model"
	"github.com/KalebHawkins/hearsay/internal/store"
)

// parseInterspersed lets flags follow positional arguments, so that
// "hearsay show web01 -json" works as well as "hearsay show -json web01".
// The standard flag package stops at the first non-flag argument.
func parseInterspersed(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		_ = fs.Parse(args) // ExitOnError: a bad flag has already exited
		if fs.NArg() == 0 {
			return pos
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// hostFlag registers -host with -ci kept as an alias, so scripts written
// against the old flag keep working.
func hostFlag(fs *flag.FlagSet, usage string) *string {
	v := new(string)
	fs.StringVar(v, "host", "", usage+" (name, fqdn or id)")
	fs.StringVar(v, "ci", "", "alias of -host")
	return v
}

// shortID is the display form of an opaque id. Any prefix of six or more
// characters is accepted back wherever a host is named.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func newTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
}

func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func cmdHosts(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("hosts", flag.ExitOnError)
	db := dbFlag(fs)
	q := fs.String("q", "", "only hosts whose name or identity contains this")
	stack := fs.String("stack", "", "only members of this stack")
	disagrees := fs.Bool("disagrees", false, "only hosts where sources currently disagree")
	cols := fs.String("cols", "os.version,env", "comma-separated attributes to show as columns")
	asJSON := fs.Bool("json", false, "print JSON instead of a table")
	if pos := parseInterspersed(fs, args); len(pos) > 0 && *q == "" {
		*q = strings.Join(pos, " ")
	}

	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	rows, err := st.Hosts(ctx, store.HostFilter{Search: *q, Stack: *stack, Contested: *disagrees})
	if err != nil {
		return err
	}

	if *asJSON {
		type hostJSON struct {
			ID           string            `json:"id"`
			Name         string            `json:"name"`
			Values       map[string]string `json:"values"`
			Disagreeing  []string          `json:"disagreeing,omitempty"`
			Stacks       []string          `json:"stacks,omitempty"`
			LastCollect  *time.Time        `json:"last_collect,omitempty"`
			CollectOK    bool              `json:"collect_ok"`
			OpenFindings int               `json:"open_findings"`
		}
		out := make([]hostJSON, 0, len(rows))
		for _, r := range rows {
			h := hostJSON{ID: r.CI.ID, Name: r.CI.Label, Values: r.Values, Stacks: r.Stacks,
				CollectOK: r.CollectOK, OpenFindings: r.OpenDiscreps}
			if r.HasCollected {
				t := r.LastCollect
				h.LastCollect = &t
			}
			for a, c := range r.Contested {
				if c {
					h.Disagreeing = append(h.Disagreeing, a)
				}
			}
			sort.Strings(h.Disagreeing)
			out = append(out, h)
		}
		return writeJSON(out)
	}

	if len(rows) == 0 {
		fmt.Println("no hosts match; add one with: hearsay host -name <name>")
		return nil
	}
	columns := splitIDs(*cols)
	tw := newTable(os.Stdout)
	fmt.Fprint(tw, "NAME\tID")
	for _, c := range columns {
		fmt.Fprintf(tw, "\t%s", strings.ToUpper(c))
	}
	fmt.Fprintln(tw, "\tCOLLECTED\tOPEN\tSTACKS")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s", r.CI.Label, shortID(r.CI.ID))
		for _, c := range columns {
			v := r.Values[c]
			if v == "" {
				v = "-"
			}
			if r.Contested[c] {
				v += " (!)"
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
		fmt.Fprintf(tw, "\t%s\t%d\t%s\n", collected, r.OpenDiscreps, strings.Join(r.Stacks, ","))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("%d hosts. (!) marks a value another source disputes.\n", len(rows))
	return nil
}

func cmdShow(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	db := dbFlag(fs)
	asJSON := fs.Bool("json", false, "print JSON instead of text")
	pos := parseInterspersed(fs, args)
	if len(pos) != 1 {
		return errors.New("usage: hearsay show <host> [-json]")
	}

	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	id, err := st.LookupCI(ctx, pos[0])
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
	conflicts := map[string][]model.Fact{}
	for _, f := range facts {
		if f.Contested {
			all, err := st.Conflicts(ctx, id, f.Attr, f.Scope)
			if err != nil {
				return err
			}
			conflicts[f.Attr+"\x00"+f.Scope] = all
		}
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
	discs, err := st.Discrepancies(ctx, store.DiscrepancyFilter{CIID: id, Disposition: model.DispOpen}, 0)
	if err != nil {
		return err
	}

	if *asJSON {
		type factJSON struct {
			Attr     string    `json:"attr"`
			Scope    string    `json:"scope,omitempty"`
			Value    string    `json:"value"`
			Raw      string    `json:"raw"`
			Source   string    `json:"source"`
			Kind     string    `json:"source_kind"`
			Observed time.Time `json:"observed_at"`
			Decided  bool      `json:"decided,omitempty"`
			Disputed bool      `json:"disputed,omitempty"`
		}
		var fj []factJSON
		for _, f := range facts {
			fj = append(fj, factJSON{Attr: f.Attr, Scope: f.Scope, Value: f.Norm, Raw: f.Raw,
				Source: f.SourceName, Kind: string(f.SourceKind), Observed: f.ObservedAt,
				Decided: f.Decided, Disputed: f.Contested})
		}
		return writeJSON(map[string]any{
			"id": ci.ID, "name": ci.Label, "created_at": ci.CreatedAt, "retired_at": ci.RetiredAt,
			"identities": idents, "facts": fj, "reachability": reach, "open_disagreements": discs,
		})
	}

	fmt.Printf("%s  (id %s)\n", ci.Label, ci.ID)
	if ci.RetiredAt != nil {
		fmt.Printf("RETIRED %s\n", ci.RetiredAt.Format(time.DateOnly))
	}
	if len(idents) > 0 {
		var parts []string
		for _, i := range idents {
			parts = append(parts, string(i.Scheme)+"="+i.Value)
		}
		fmt.Printf("identity: %s\n", strings.Join(parts, "  "))
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
			v := factValue(f)
			if f.Decided {
				v += " (decided)"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s:%s\t%s\n", attr, v, f.SourceKind, f.SourceName, agoText(f.ObservedAt))
			for _, o := range conflicts[f.Attr+"\x00"+f.Scope] {
				if o.AssertionID == f.AssertionID {
					continue
				}
				fmt.Fprintf(tw, "\t  but %s\t%s:%s\t%s\n", factValue(o), o.SourceKind, o.SourceName, agoText(o.ObservedAt))
			}
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
				state = " (disabled)"
			}
			fmt.Printf("target: %s %s@%s%s %s%s\n", p.Transport, p.Username, p.Address, portSuffix(p.Port), p.CredentialRef, state)
		}
	} else {
		fmt.Printf("\nno connection profile; add one with: hearsay target -host %s -address <addr> -user <user> -credential agent:\n", ci.Label)
	}

	if len(discs) > 0 {
		fmt.Println()
		printDisagreements(discs)
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

func cmdSet(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("set", flag.ExitOnError)
	db := dbFlag(fs)
	scope := fs.String("scope", "", "instance on the host the values apply to, blank for host-wide")
	reason := fs.String("reason", "", "why, recorded in the audit log (required)")
	pos := parseInterspersed(fs, args)
	if len(pos) < 2 {
		return errors.New(`usage: hearsay set <host> attr=value [attr=value ...] -reason "why"`)
	}
	if strings.TrimSpace(*reason) == "" {
		return errors.New("-reason is required: a value a person states is only as good as the reason given for it")
	}
	type kv struct{ k, v string }
	var pairs []kv
	for _, p := range pos[1:] {
		k, v, ok := strings.Cut(p, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return fmt.Errorf("%q is not attr=value", p)
		}
		pairs = append(pairs, kv{k, v})
	}

	st, err := openStore(*db)
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
	for _, p := range pairs {
		if err := st.Claim(ctx, id, p.k, *scope, p.v, who, *reason); err != nil {
			return err
		}
		fmt.Printf("%s: %s = %s (claimed by %s)\n", pos[0], p.k, p.v, who)
	}
	fmt.Println("run hearsay detect to compare it with the other sources")
	return nil
}

// dispositionAliases maps the short words used on the command line to the
// stored dispositions.
var dispositionAliases = map[string]model.Disposition{
	"machine":  model.DispConfirmMachine,
	"document": model.DispKeepDocument,
	"doc":      model.DispKeepDocument,
	"accept":   model.DispAccepted,
	"accepted": model.DispAccepted,
}

func cmdDisagreements(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("disagreements", flag.ExitOnError)
	db := dbFlag(fs)
	host := hostFlag(fs, "only disagreements about this host")
	kind := fs.String("kind", "", "only this kind, such as conflict, stale, unparsed or undocumented")
	disp := fs.String("disposition", "open", "open, confirm_machine, keep_document, accepted, lapsed, or all")
	limit := fs.Int("limit", 200, "show at most this many, zero for no limit")
	decide := fs.String("decide", "", "comma-separated disagreement ids to settle")
	as := fs.String("as", "", "with -decide: machine, document or accept")
	note := fs.String("note", "", "with -decide: why, recorded with the decision")
	asJSON := fs.Bool("json", false, "print JSON instead of text")
	fs.Parse(args)

	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()

	if *decide != "" {
		d, ok := dispositionAliases[strings.ToLower(*as)]
		if !ok {
			return errors.New("-decide needs -as machine, -as document or -as accept")
		}
		if _, err := buildEngine(st, engineFiles{}, nil); err != nil { // wires the normaliser
			return err
		}
		for _, s := range splitIDs(*decide) {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return fmt.Errorf("%q is not a disagreement id", s)
			}
			if err := st.Dispose(ctx, n, d, actor(), *note); errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("no disagreement #%d; list them with: hearsay disagreements", n)
			} else if err != nil {
				return fmt.Errorf("disagreement %d: %w", n, err)
			}
			fmt.Printf("#%d recorded as %s\n", n, d)
		}
		return nil
	}

	f := store.DiscrepancyFilter{Kind: model.DiscrepancyKind(*kind)}
	if *disp != "all" {
		f.Disposition = model.Disposition(*disp)
	}
	if *host != "" {
		if f.CIID, err = st.LookupCI(ctx, *host); err != nil {
			return err
		}
	}
	list, err := st.Discrepancies(ctx, f, *limit)
	if err != nil {
		return err
	}
	if *asJSON {
		if list == nil {
			list = []model.Discrepancy{}
		}
		return writeJSON(list)
	}
	if len(list) == 0 {
		fmt.Println("nothing matches")
		return nil
	}
	printDisagreements(list)
	if f.Disposition == model.DispOpen {
		fmt.Println("\nsettle one with: hearsay disagreements -decide <id> -as machine|document|accept [-note why]")
	}
	return nil
}

func printDisagreements(list []model.Discrepancy) {
	tw := newTable(os.Stdout)
	fmt.Fprintln(tw, "ID\tHOST\tKIND\tATTRIBUTE\tCLAIMED\tOBSERVED\tSTATE")
	for _, d := range list {
		attr := d.Attr
		if d.Scope != "" {
			attr += " [" + d.Scope + "]"
		}
		claimed := oneLine(d.Claimed, 30)
		if d.ClaimedBy != "" {
			claimed += " (" + d.ClaimedBy + ")"
		}
		observed := oneLine(d.Observed, 30)
		if d.ObservedBy != "" {
			observed += " (" + d.ObservedBy + ")"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", d.ID, d.CILabel, d.Kind, dash(attr),
			dash(claimed), dash(observed), d.Disposition)
	}
	_ = tw.Flush()
}

// cmdRefresh is the whole loop in one command: optionally look names up,
// collect, run the liveness checks, then compare. It is what the web
// interface's collect button does, plus the checks.
func cmdRefresh(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("refresh", flag.ExitOnError)
	db := dbFlag(fs)
	only := fs.String("only", "", "comma-separated hosts (names or ids)")
	conc := fs.Int("concurrency", 8, "how many hosts at once")
	timeout := fs.Duration("host-timeout", 2*time.Minute, "limit for everything done against one host")
	resolve := fs.Bool("resolve", false, "look every host up in DNS first")
	noChecks := fs.Bool("no-checks", false, "skip the liveness checks")
	stale := fs.Duration("stale-after", 14*24*time.Hour, "age at which a value is called stale")
	rules := fs.String("rules", "", "extra normaliser rules file")
	collectors := fs.String("collectors", "", "extra collector spec file")
	externals := fs.String("externals", "", "file listing external collector programs")
	noEvidence := fs.Bool("no-evidence", false, "do not keep what each collector command printed")
	quiet := fs.Bool("quiet", false, "print only the summary lines, not one line per host")
	fs.Parse(args)

	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	eng, err := buildEngine(st, engineFiles{Rules: *rules, Collectors: *collectors,
		Externals: *externals, NoEvidence: *noEvidence}, log)
	if err != nil {
		return err
	}
	ids, err := st.LookupCIs(ctx, splitIDs(*only))
	if err != nil {
		return err
	}

	if *resolve {
		sum, err := eng.Resolve(ctx, engine.ResolveOptions{})
		if err != nil {
			return fmt.Errorf("resolve: %w", err)
		}
		fmt.Printf("dns:     %d checked, %d resolved, %d nowhere, %d with several addresses\n",
			sum.Checked, sum.Resolved, sum.Unresolved, sum.Multi)
	}

	sum, err := runCollect(ctx, eng, engine.Options{Only: ids, Concurrency: *conc,
		HostTimeout: *timeout, Actor: actor()}, *quiet)
	if err != nil {
		return fmt.Errorf("collect: %w", err)
	}
	fmt.Printf("collect: %d targets, %d ok, %d failed\n", sum.Targets, sum.Succeeded, sum.Failed)
	if sum.Targets == 0 {
		fmt.Println("         nothing to collect from; add a connection with: hearsay target -host <name> -user <user>")
	}

	if !*noChecks {
		results, err := eng.RunChecks(ctx, ids, 16)
		if err != nil {
			return fmt.Errorf("checks: %w", err)
		}
		if len(results) > 0 {
			ok, failed := engine.CheckSummary(results)
			fmt.Printf("checks:  %d ok, %d failed\n", ok, failed)
		}
	}

	det, err := eng.Detect(ctx, *stale)
	if err != nil {
		return fmt.Errorf("detect: %w", err)
	}
	fmt.Printf("detect:  %d conflicts, %d not understood, %d stale, %d undocumented\n",
		det.Conflicts, det.Unparsed, det.Stale, det.Undocumented)
	if _, total, err := st.DiscrepancyCounts(ctx); err == nil && total > 0 {
		fmt.Printf("%d disagreements open; list them with: hearsay disagreements\n", total)
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
