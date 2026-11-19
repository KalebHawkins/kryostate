package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/engine"
	"github.com/KalebHawkins/kryostate/internal/query"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// starterCollectors is what "kryostate collectors init" writes. It holds one
// working, read-only example, so a new user edits something that already runs
// rather than starting from a blank file and a schema.
const starterCollectors = `{
  "version": 1,
  "specs": [
    {
      "id": "example.nodename",
      "attr": "host.nodename",
      "os": ["linux", "aix"],
      "comment": "An example to copy. It runs 'uname -n' and keeps the first line: the name the host calls itself.",
      "command": "uname -n",
      "parse": { "kind": "first_line" },
      "type": "text"
    }
  ]
}
`

// exampleCollector is what "kryostate collectors add --example" prints: one
// working, read-only collector in the form "collectors add" takes.
const exampleCollector = `{
  "id": "example.kernel",
  "attr": "custom.kernel",
  "os": ["linux"],
  "comment": "An example to copy. It runs 'uname -r' and keeps the first line: the kernel release.",
  "command": "uname -r",
  "parse": { "kind": "first_line" },
  "type": "version"
}
`

// addCollectors adds the collectors in a file to the database, or with dryRun
// says what it would add. Every collector is checked before any is written,
// so a file with one mistake in it changes nothing.
func addCollectors(ctx context.Context, w io.Writer, eng *engine.Engine, path string, dryRun bool) error {
	var (
		b   []byte
		err error
	)
	if path == "-" {
		b, err = io.ReadAll(os.Stdin)
	} else {
		b, err = os.ReadFile(path)
	}
	if err != nil {
		return err
	}
	specs, err := collect.ParseSpecList(b)
	if err == nil {
		err = eng.CheckAdd(ctx, specs)
	}
	if err != nil {
		return fmt.Errorf("%s: %w\nnothing was added", path, err)
	}
	have, err := eng.Store.UserCollectors(ctx)
	if err != nil {
		return err
	}
	added := map[string]bool{}
	for _, u := range have {
		added[u.Spec.ID] = true
	}
	for _, sp := range specs {
		verb, done := "add", "added"
		if added[sp.ID] {
			verb, done = "update", "updated"
		}
		line := fmt.Sprintf("%s records %s on %s (%s), with: %s", sp.ID, sp.Attr, runsOn(engine.SpecTarget(sp)), strings.Join(sp.OS, ", "), oneLine(sp.Command, 60))
		if dryRun {
			fmt.Fprintf(w, "would %s %s\n", verb, line)
			continue
		}
		if _, err := eng.Store.PutUserCollector(ctx, sp, actor()); err != nil {
			return fmt.Errorf("%s: %w", sp.ID, err)
		}
		fmt.Fprintf(w, "%s %s\n", done, line)
	}
	if dryRun {
		fmt.Fprintln(w, "\nNothing was saved (--dry-run).")
		return nil
	}
	fmt.Fprintf(w, "\ntry it now:  kryostate collectors try %s --host <name>\n", specs[0].ID)
	fmt.Fprintln(w, "collect:     kryostate refresh")
	fmt.Fprintln(w, "A running kryostate serve picks this up at its next collection and page view; no restart is needed.")
	return nil
}

// removeCollectors removes collectors added with "collectors add". Every id is
// checked first, so a typo removes nothing.
func removeCollectors(ctx context.Context, w io.Writer, eng *engine.Engine, ids []string) error {
	have, err := eng.Store.UserCollectors(ctx)
	if err != nil {
		return err
	}
	added := map[string]bool{}
	for _, u := range have {
		added[u.Spec.ID] = true
	}
	file := map[string]bool{}
	for _, sp := range eng.OwnSpecs() {
		file[sp.ID] = true
	}
	builtin := map[string]bool{}
	if set, err := collect.BuiltinSpecs(); err == nil {
		for _, sp := range set.Specs {
			builtin[sp.ID] = true
		}
	}
	for i, id := range ids {
		id = strings.TrimPrefix(id, collect.RefSpec)
		ids[i] = id
		switch {
		case added[id]:
		case builtin[id] || strings.HasPrefix(id, collect.RefBuiltin):
			return fmt.Errorf("%s is built in and cannot be removed; nothing was removed", id)
		case file[id]:
			return fmt.Errorf("%s comes from the collectors file; remove it there; nothing was removed", id)
		default:
			if _, ok := collect.Lookup(id); ok {
				return fmt.Errorf("%s is a catalog entry; switch it off with: kryostate collectors disable %s; nothing was removed", id, id)
			}
			return fmt.Errorf("no collector %q was added; see them with: kryostate collectors list; nothing was removed", id)
		}
	}
	for _, id := range ids {
		if err := eng.Store.DeleteUserCollector(ctx, id, actor()); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%s: %w", id, err)
		}
		fmt.Fprintf(w, "removed %s. Values it already collected are kept.\n", id)
	}
	return nil
}

// sourceOf says where a collector named on the command line comes from.
func sourceOf(ctx context.Context, eng *engine.Engine, ref string) string {
	id := strings.TrimPrefix(ref, collect.RefSpec)
	if u, ok := eng.Added(ctx, id); ok {
		s := "added"
		if u.AddedBy != "" {
			s += " by " + u.AddedBy
		}
		return s + " on " + u.AddedAt.Local().Format("2006-01-02 15:04")
	}
	for _, sp := range eng.OwnSpecs() {
		if sp.ID == id {
			return "from the collectors file"
		}
	}
	switch {
	case strings.HasPrefix(ref, collect.RefBuiltin):
		return "built in"
	case strings.HasPrefix(ref, collect.RefExternal):
		return "external program"
	}
	if _, ok := collect.Lookup(ref); ok {
		return "catalog"
	}
	return "built in"
}

// showDefinition prints one collector as it would be written in a file, so a
// catalog entry or a built-in can be copied as the start of one's own. The
// definition goes to standard output on its own, so it can be redirected into
// a file; the note about which file goes to standard error.
func showDefinition(ctx context.Context, w io.Writer, eng *engine.Engine, ref, param string) error {
	file, b, err := eng.Definition(ctx, ref, param)
	if err != nil {
		return fmt.Errorf("%w; see them with: kryostate collectors list, and the catalog with: kryostate collectors catalog", err)
	}
	fmt.Fprintf(os.Stderr, "# %s (%s) as written in %s\n", ref, sourceOf(ctx, eng, ref), file)
	_, err = w.Write(b)
	return err
}

// enableTarget is where enable switches a collector on: --stacks written as a
// query, and --where, both of which a host must satisfy. A --where that does
// not parse is refused with a caret under the problem.
func enableTarget(stacks, where string) (string, error) {
	where = strings.TrimSpace(where)
	if where != "" {
		if _, err := query.Parse(where); err != nil {
			var qe *query.Error
			if errors.As(err, &qe) {
				return "", fmt.Errorf("--where: %s\n  %s\n  %s", qe.Msg, where, qe.Caret())
			}
			return "", fmt.Errorf("--where: %w", err)
		}
	}
	return query.And(store.StacksTarget(splitIDs(stacks)), where), nil
}

// runsOn names a target in a sentence.
func runsOn(target string) string {
	if target == "" {
		return "every host"
	}
	return "hosts matching " + target
}

func paramFlag(p string) string {
	if p == "" {
		return ""
	}
	return " --param '" + p + "'"
}

// enableGroup switches on every entry in a group. It only ever widens: an
// entry already on for every host is left alone, and one limited to other
// stacks gains these.
func enableGroup(ctx context.Context, st *store.Store, g collect.Group, target string) error {
	fmt.Printf("%s, for %s:\n", g.Title, runsOn(target))
	for _, id := range g.Entries {
		c, changed, err := st.EnableChoice(ctx, collect.Choice{Template: id, Target: target}, actor())
		if err != nil {
			return err
		}
		state := "on"
		if !changed {
			state = "already on"
		}
		fmt.Printf("  %-40s %-11s runs on %s\n", c.Label(), state, runsOn(c.Target))
	}
	fmt.Println("collect:     kryostate refresh")
	return nil
}

// findChoice finds a switched-on catalog collector by its number, or by its
// catalog name and, for a template, its value.
func findChoice(ctx context.Context, st *store.Store, ref, param string) (collect.Choice, error) {
	choices, err := st.Choices(ctx)
	if err != nil {
		return collect.Choice{}, err
	}
	if n, err := strconv.ParseInt(ref, 10, 64); err == nil {
		for _, c := range choices {
			if c.ID == n {
				return c, nil
			}
		}
	}
	var match []collect.Choice
	for _, c := range choices {
		if c.Template == ref && (param == "" || c.Param == param) {
			match = append(match, c)
		}
	}
	switch len(match) {
	case 1:
		return match[0], nil
	case 0:
		return collect.Choice{}, fmt.Errorf("%s is not switched on; see what is with: kryostate collectors list", ref)
	default:
		return collect.Choice{}, fmt.Errorf("%s is switched on for %d values; name one with --param, or use its number from: kryostate collectors list", ref, len(match))
	}
}

// specsToTry resolves what try names. A catalog name works whether or not it
// is switched on, so an entry can be tried before anyone commits to it.
func specsToTry(ctx context.Context, eng *engine.Engine, ref, param string) ([]collect.Spec, error) {
	if _, ok := collect.Lookup(ref); ok {
		return collect.Choice{Template: ref, Param: param}.Specs()
	}
	all, err := eng.AllSpecs(ctx)
	if err != nil {
		return nil, err
	}
	var out []collect.Spec
	for _, s := range all.Specs {
		if s.ID == ref {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no collector named %q; see them with: kryostate collectors list, and the catalog with: kryostate collectors catalog", ref)
	}
	return out, nil
}

func listCatalog(ctx context.Context, st *store.Store) error {
	choices, err := st.Choices(ctx)
	if err != nil {
		return err
	}
	on := map[string]int{}
	for _, c := range choices {
		on[c.Template]++
	}
	tw := newTable(os.Stdout)
	fmt.Fprintf(tw, "GROUPS\t\t\t\n")
	for _, g := range collect.Groups {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t\n", g.ID, g.Title, strings.Join(g.Entries, ", "))
	}
	fmt.Fprintln(tw, "\t\t\t")
	category := ""
	for _, t := range collect.Catalog {
		if t.Category != category {
			if category != "" {
				fmt.Fprintln(tw, "\t\t\t")
			}
			category = t.Category
			fmt.Fprintf(tw, "%s\t\t\t\n", strings.ToUpper(category))
		}
		status := ""
		if n := on[t.ID]; n > 0 {
			status = "ON"
			if !t.Fixed() {
				status = fmt.Sprintf("ON x%d", n)
			}
		}
		name := t.ID
		if !t.Fixed() {
			name += " --param <" + strings.ToLower(t.Param.Label) + ">"
			if t.Param.Config() {
				name += " --key <key> [--section <section>]"
			}
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", name, t.Title, strings.Join(t.Systems(), ","), status)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Println("\nswitch one on:   kryostate collectors enable cpu.cores")
	fmt.Println("a whole group:   kryostate collectors enable group.web --stacks web-prod")
	fmt.Println("a config value:  kryostate collectors enable config.value --param /etc/app/app.conf --section database --key host")
	fmt.Println("with a value:    kryostate collectors enable file.exists --param /etc/resolv.conf")
	fmt.Println("for some stacks: kryostate collectors enable package.version --param nginx --stacks web-prod")
	fmt.Println(`for some hosts:  kryostate collectors enable nginx.version --where 'name in ("web01", "web02")'`)
	fmt.Println("try first:       kryostate collectors try memory.total --host <name>")
	fmt.Println("written out:     kryostate collectors show file.exists --param /etc/hosts  (as you would write it in " + defaultCollectorsFile + ")")
	return nil
}

// listAdded lists the collectors added with "collectors add", with who added
// them and when.
func listAdded(ctx context.Context, eng *engine.Engine, st *store.Store) error {
	all, err := st.UserCollectors(ctx)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		fmt.Println("When nothing in the catalog fits, add your own:")
		fmt.Println("  kryostate collectors add --example > mine.json")
		fmt.Println("  kryostate collectors add mine.json")
		return nil
	}
	running := map[string]bool{}
	added, err := eng.AddedCollectors(ctx)
	if err != nil {
		return err
	}
	for _, u := range added {
		running[u.Spec.ID] = true
	}
	fmt.Println("Added with kryostate collectors add:")
	tw := newTable(os.Stdout)
	fmt.Fprintln(tw, "  ID\tATTRIBUTE\tSYSTEMS\tADDED BY\tADDED\tCOMMAND")
	for _, u := range all {
		id := u.Spec.ID
		if !running[id] {
			id += " (skipped: the collectors file has the same id)"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", id, u.Spec.Attr, strings.Join(u.Spec.OS, ","), u.AddedBy,
			u.AddedAt.Local().Format("2006-01-02 15:04"), oneLine(u.Spec.Command, 50))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Println("  replace one: kryostate collectors add <file>    remove one: kryostate collectors rm <id>")
	return nil
}

func listCollectors(ctx context.Context, eng *engine.Engine, st *store.Store, path string) error {
	choices, err := st.Choices(ctx)
	if err != nil {
		return err
	}
	if len(choices) == 0 {
		fmt.Println("No catalog collectors are switched on. See what is available with: kryostate collectors catalog")
	} else {
		fmt.Println("Switched on from the catalog:")
		tw := newTable(os.Stdout)
		fmt.Fprintln(tw, "  #\tCOLLECTOR\tRECORDED AS\tRUNS ON")
		var gone []collect.Choice
		for _, c := range choices {
			where := "all hosts"
			if c.Target != "" {
				where = c.Target
			}
			label := c.Label()
			if !c.InCatalog() {
				label += " (no longer in the catalog, skipped)"
				gone = append(gone, c)
			}
			fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\n", c.ID, label, c.Attr, where)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		fmt.Println("  switch one off: kryostate collectors disable <#>")
		for _, c := range gone {
			fmt.Printf("  %s is not in this build's catalog, so it never runs. Values it recorded are kept.\n"+
				"    remove it:    kryostate collectors disable %d\n", c.Template, c.ID)
		}
	}

	builtin, err := collect.BuiltinSpecs()
	if err != nil {
		return err
	}
	isBuiltin := map[string]bool{}
	for _, s := range builtin.Specs {
		isBuiltin[s.ID] = true
	}
	var mine []collect.Spec
	for _, s := range eng.Specs.Specs {
		if !isBuiltin[s.ID] {
			mine = append(mine, s)
		}
	}
	fmt.Println()
	if err := listAdded(ctx, eng, st); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		fmt.Printf("\nFrom the collectors file %s:\n", path)
		tw := newTable(os.Stdout)
		fmt.Fprintln(tw, "  ID\tATTRIBUTE\tSYSTEMS\tCOMMAND")
		for _, s := range mine {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", s.ID, s.Attr, strings.Join(s.OS, ","), oneLine(s.Command, 50))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		if len(mine) == 0 {
			fmt.Println("  (the file has no collectors in it)")
		}
	}
	if len(eng.Externals) > 0 {
		fmt.Println("\nExternal programs:")
		for _, ex := range eng.Externals {
			fmt.Printf("  %s  %s\n", ex.Name, ex.Path)
		}
	}

	fmt.Println("\nAlways collected:")
	tw := newTable(os.Stdout)
	var order []string
	systems := map[string][]string{}
	for _, s := range builtin.Specs {
		if _, ok := systems[s.Attr]; !ok {
			order = append(order, s.Attr)
		}
		for _, o := range s.OS {
			if !contains(systems[s.Attr], o) {
				systems[s.Attr] = append(systems[s.Attr], o)
			}
		}
	}
	for _, a := range order {
		fmt.Fprintf(tw, "  %s\t%s\n", a, strings.Join(systems[a], ","))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Println("\nTest any collector with: kryostate collectors try <name> --host <host>")
	return nil
}

// tryCollector runs one collector and explains the result in words, including
// what to do when it did not work. Nothing is written to the database. Of
// several variants, the one for the host's system is used.
func tryCollector(ctx context.Context, eng *engine.Engine, st *store.Store, specs []collect.Spec, host, sample, enableHint string) error {
	if (host == "") == (sample == "") {
		return errors.New("try needs either --host <name> to run the command, or --sample <file> to test the parse rule on saved output")
	}
	spec := specs[0]

	var output string
	var hostLine string
	if sample != "" {
		// With no host there is no system to choose by; prefer Linux, the
		// commonest, and say which was used.
		for _, s := range specs {
			if len(specs) > 1 && contains(s.OS, "linux") {
				spec = s
				break
			}
		}
		var b []byte
		var err error
		if sample == "-" {
			b, err = io.ReadAll(os.Stdin)
		} else {
			b, err = os.ReadFile(sample)
		}
		if err != nil {
			return err
		}
		output = string(b)
		hostLine = "output     from " + sample
	} else {
		ciID, err := st.LookupCI(ctx, host)
		if err != nil {
			return err
		}
		targets, err := st.CollectTargets(ctx, "")
		if err != nil {
			return err
		}
		var prof *store.ConnProfile
		for i := range targets {
			if targets[i].CIID == ciID {
				prof = &targets[i]
				break
			}
		}
		if prof == nil {
			if c, err := st.CI(ctx, ciID); err == nil && c.DisabledAt != nil {
				return fmt.Errorf("%s is disabled, so nothing is run on it; enable it with: kryostate host enable %s", host, host)
			}
			return fmt.Errorf("%s has no enabled connection; add one on its page in the web interface, or with: kryostate target add %s --user <user>", host, host)
		}
		res, err := eng.TrySpecs(ctx, *prof, specs)
		if err != nil {
			return err
		}
		spec = res.Spec
		hostLine = fmt.Sprintf("host       %s (%s@%s, %s)", host, prof.Username, prof.Address, res.OS)
		printTryHeader(spec, hostLine)
		hostLine = ""
		if !res.OSMatches {
			fmt.Printf("\nNOTE: the host says it is %q, and this collector only runs on %s.\n"+
				"It ran now because you asked, but a real collection will skip this host.\n",
				res.OS, strings.Join(spec.OS, ", "))
		}
		output = res.Reading.Output
		if res.Reading.Err != nil && !res.Reading.Ran {
			fmt.Printf("\nRESULT     the command failed: %v\n\n", res.Reading.Err)
			fmt.Println(wrap(collect.Explain(res.Reading), 76))
			fmt.Println("\nWhat to check:")
			fmt.Println("  - Log in to the host as the same user and run the command by hand.")
			fmt.Println("  - A \"not found\" error means the program is not installed or not on the PATH.")
			fmt.Println("  - A program that prints to stderr needs 2>&1 at the end of the command.")
			if !spec.Optional {
				fmt.Println("  - If some hosts legitimately lack it, add \"optional\": true.")
			}
			return errors.New("the collector did not produce a value")
		}
	}
	if hostLine != "" {
		printTryHeader(spec, hostLine)
	}

	fmt.Println("\n--- what the command printed ---")
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	for i, l := range lines {
		if i == 20 {
			fmt.Printf("... %d more lines\n", len(lines)-20)
			break
		}
		fmt.Println(l)
	}
	fmt.Println("--------------------------------")

	value, err := collect.Extract(spec.Parse, output)
	if err != nil {
		fmt.Printf("\nRESULT     the parse rule (%s) found nothing: %v\n\n", parseKindOf(spec), err)
		fmt.Println("The command ran, so the rule needs changing. Compare the rule with the output above.")
		if sample == "" {
			fmt.Println("Save the output to a file and test changes without a host:")
			fmt.Printf("  kryostate collectors try %s --sample output.txt\n", spec.ID)
		} else {
			fmt.Println("Edit the rule in your collectors file and run this command again.")
		}
		return errors.New("the collector did not produce a value")
	}
	if strings.Contains(value, "\n") {
		fmt.Printf("\nRESULT     %s = (%d lines, as printed above)\n", spec.Attr, strings.Count(value, "\n")+1)
	} else {
		fmt.Printf("\nRESULT     %s = %q\n", spec.Attr, value)
	}
	fmt.Println("\nThis is what a collection would record. Nothing was saved.")
	if enableHint != "" {
		fmt.Printf("Switch it on with: %s, then run kryostate refresh.\n", enableHint)
	} else {
		fmt.Println("Run kryostate refresh to collect for real.")
	}
	return nil
}

func printTryHeader(spec collect.Spec, hostLine string) {
	fmt.Printf("collector  %s\n", spec.ID)
	fmt.Printf("records    %s\n", spec.Attr)
	fmt.Printf("command    %s\n", spec.Command)
	fmt.Println(hostLine)
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

func parseKindOf(s collect.Spec) string {
	if s.Parse.Kind == "" {
		return string(collect.ParseFirstLine)
	}
	return string(s.Parse.Kind)
}

// wrap breaks text into lines of at most width characters, at spaces.
func wrap(text string, width int) string {
	var b strings.Builder
	n := 0
	for _, w := range strings.Fields(text) {
		if n > 0 && n+1+len(w) > width {
			b.WriteByte('\n')
			n = 0
		} else if n > 0 {
			b.WriteByte(' ')
			n++
		}
		b.WriteString(w)
		n += len(w)
	}
	return b.String()
}
