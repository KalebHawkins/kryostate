package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/KalebHawkins/hearsay/internal/collect"
	"github.com/KalebHawkins/hearsay/internal/engine"
	"github.com/KalebHawkins/hearsay/internal/store"
)

// starterCollectors is what "hearsay collectors -init" writes. It holds one
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

func cmdCollectors(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("collectors", flag.ExitOnError)
	db := dbFlag(fs)
	catalog := fs.Bool("catalog", false, "list the ready-made collectors that can be switched on")
	enable := fs.String("enable", "", "switch on a catalog collector, such as cpu.cores or file.exists")
	disable := fs.String("disable", "", "switch off a catalog collector, by its number in the list or its catalog name")
	param := fs.String("param", "", "with -enable, -disable or -try: the path, variable or package a catalog template needs")
	attr := fs.String("attr", "", "with -enable: the attribute to record it as, blank for a sensible default")
	stacks := fs.String("stacks", "", "with -enable: comma-separated stacks to limit it to, blank for every host")
	initFile := fs.Bool("init", false, "create a starter "+defaultCollectorsFile+" next to the database, for collectors of your own")
	try := fs.String("try", "", "test a collector, by catalog name or id, and record nothing")
	host := fs.String("host", "", "with -try: run the command on this host")
	sample := fs.String("sample", "", "with -try: read command output from this file (- for standard input) instead of a host")
	collectors := fs.String("collectors", "", "collector spec file, instead of "+defaultCollectorsFile+" next to the database")
	fs.Parse(args)

	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	path := filepath.Join(dataDir, defaultCollectorsFile)
	if *collectors != "" {
		path = *collectors
	}

	switch {
	case *initFile:
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists; edit it, or delete it to start again", path)
		}
		if err := os.WriteFile(path, []byte(starterCollectors), 0o600); err != nil {
			return err
		}
		fmt.Printf("created %s with one example collector.\n\n", path)
		fmt.Println("next:")
		fmt.Printf("  1. edit it (the user guide explains each field: hearsay serve, then open /guide/collectors)\n")
		fmt.Printf("  2. test it:   hearsay collectors -try example.nodename -host <name>\n")
		fmt.Printf("  3. collect:   hearsay refresh\n")
		return nil

	case *catalog:
		return listCatalog(ctx, st)

	case *enable != "":
		c, err := st.PutChoice(ctx, collect.Choice{Template: *enable, Param: *param, Attr: *attr,
			Stacks: splitIDs(*stacks)}, actor())
		if err != nil {
			if _, ok := collect.Lookup(*enable); !ok {
				return fmt.Errorf("%w; list the catalog with: hearsay collectors -catalog", err)
			}
			return err
		}
		where := "every host"
		if len(c.Stacks) > 0 {
			where = "hosts in " + strings.Join(c.Stacks, ", ")
		}
		fmt.Printf("%s is on for %s, recorded as %s.\n", c.Label(), where, c.Attr)
		fmt.Printf("try it now:  hearsay collectors -try %s%s -host <name>\n", c.Template, paramFlag(c.Param))
		fmt.Println("collect:     hearsay refresh")
		return nil

	case *disable != "":
		c, err := findChoice(ctx, st, *disable, *param)
		if err != nil {
			return err
		}
		if err := st.DeleteChoice(ctx, c.ID, actor()); err != nil {
			return err
		}
		fmt.Printf("%s is off. Values it already collected are kept.\n", c.Label())
		return nil
	}

	eng, err := buildEngine(st, engineFiles{Collectors: *collectors}, nil)
	if err != nil {
		return err
	}

	if *try != "" {
		specs, err := specsToTry(ctx, eng, *try, *param)
		if err != nil {
			return err
		}
		hint := ""
		if _, ok := collect.Lookup(*try); ok {
			hint = "hearsay collectors -enable " + *try + paramFlag(*param)
		}
		return tryCollector(ctx, eng, st, specs, *host, *sample, hint)
	}
	return listCollectors(ctx, eng, st, path)
}

func paramFlag(p string) string {
	if p == "" {
		return ""
	}
	return " -param '" + p + "'"
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
		return collect.Choice{}, fmt.Errorf("%s is not switched on; see what is with: hearsay collectors", ref)
	default:
		return collect.Choice{}, fmt.Errorf("%s is switched on for %d values; name one with -param, or use its number from: hearsay collectors", ref, len(match))
	}
}

// specsToTry resolves what -try names. A catalog name works whether or not it
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
		return nil, fmt.Errorf("no collector named %q; see them with: hearsay collectors, and the catalog with: hearsay collectors -catalog", ref)
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
	category := ""
	tw := newTable(os.Stdout)
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
			name += " -param <" + strings.ToLower(t.Param.Label) + ">"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", name, t.Title, strings.Join(t.Systems(), ","), status)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Println("\nswitch one on:   hearsay collectors -enable cpu.cores")
	fmt.Println("with a value:    hearsay collectors -enable file.exists -param /etc/resolv.conf")
	fmt.Println("for some stacks: hearsay collectors -enable splunk.uf.version -stacks splunk-prod")
	fmt.Println("try first:       hearsay collectors -try memory.total -host <name>")
	return nil
}

func listCollectors(ctx context.Context, eng *engine.Engine, st *store.Store, path string) error {
	choices, err := st.Choices(ctx)
	if err != nil {
		return err
	}
	if len(choices) == 0 {
		fmt.Println("No catalog collectors are switched on. See what is available with: hearsay collectors -catalog")
	} else {
		fmt.Println("Switched on from the catalog:")
		tw := newTable(os.Stdout)
		fmt.Fprintln(tw, "  #\tCOLLECTOR\tRECORDED AS\tRUNS ON")
		for _, c := range choices {
			where := "all hosts"
			if len(c.Stacks) > 0 {
				where = strings.Join(c.Stacks, ",")
			}
			fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\n", c.ID, c.Label(), c.Attr, where)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		fmt.Println("  switch one off: hearsay collectors -disable <#>")
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
	if _, err := os.Stat(path); err != nil {
		fmt.Println("When nothing in the catalog fits, write your own: hearsay collectors -init")
	} else {
		fmt.Printf("Your own, from %s:\n", path)
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
	fmt.Println("\nTest any collector with: hearsay collectors -try <name> -host <host>")
	return nil
}

// tryCollector runs one collector and explains the result in words, including
// what to do when it did not work. Nothing is written to the database. Of
// several variants, the one for the host's system is used.
func tryCollector(ctx context.Context, eng *engine.Engine, st *store.Store, specs []collect.Spec, host, sample, enableHint string) error {
	if (host == "") == (sample == "") {
		return errors.New("-try needs either -host <name> to run the command, or -sample <file> to test the parse rule on saved output")
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
			return fmt.Errorf("%s has no enabled connection; add one on its page in the web interface, or with: hearsay target -host %s -user <user>", host, host)
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
			fmt.Println("What to check:")
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
			fmt.Printf("  hearsay collectors -try %s -sample output.txt\n", spec.ID)
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
		fmt.Printf("Switch it on with: %s, then run hearsay refresh.\n", enableHint)
	} else {
		fmt.Println("Run hearsay refresh to collect for real.")
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
