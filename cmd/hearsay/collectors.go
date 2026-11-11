package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
      "id": "example.timezone",
      "attr": "host.timezone",
      "os": ["linux", "aix"],
      "comment": "An example to copy. It runs 'date +%Z' and keeps the first line, such as UTC.",
      "command": "date +%Z",
      "parse": { "kind": "first_line" },
      "type": "text"
    }
  ]
}
`

func cmdCollectors(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("collectors", flag.ExitOnError)
	db := dbFlag(fs)
	initFile := fs.Bool("init", false, "create a starter "+defaultCollectorsFile+" next to the database")
	try := fs.String("try", "", "collector id to test; records nothing")
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

	if *initFile {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists; edit it, or delete it to start again", path)
		}
		if err := os.WriteFile(path, []byte(starterCollectors), 0o600); err != nil {
			return err
		}
		fmt.Printf("created %s with one example collector.\n\n", path)
		fmt.Println("next:")
		fmt.Printf("  1. edit it (the user guide explains each field: hearsay serve, then open /guide/collectors)\n")
		fmt.Printf("  2. test it:   hearsay collectors -try example.timezone -host <name>\n")
		fmt.Printf("  3. collect:   hearsay refresh\n")
		return nil
	}

	eng, err := buildEngine(st, engineFiles{Collectors: *collectors}, nil)
	if err != nil {
		return err
	}
	builtin, err := collect.BuiltinSpecs()
	if err != nil {
		return err
	}
	isBuiltin := map[string]bool{}
	for _, s := range builtin.Specs {
		isBuiltin[s.ID] = true
	}

	if *try != "" {
		var spec *collect.Spec
		for i := range eng.Specs.Specs {
			if eng.Specs.Specs[i].ID == *try {
				spec = &eng.Specs.Specs[i]
			}
		}
		if spec == nil {
			return fmt.Errorf("no collector with id %q; list them with: hearsay collectors", *try)
		}
		return tryCollector(ctx, eng, st, *spec, *host, *sample)
	}

	// The listing.
	var mine []collect.Spec
	tw := newTable(os.Stdout)
	fmt.Fprintln(tw, "BUILT IN\tATTRIBUTE\tSYSTEMS\tCOMMAND")
	for _, s := range eng.Specs.Specs {
		if !isBuiltin[s.ID] {
			mine = append(mine, s)
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.ID, s.Attr, strings.Join(s.OS, ","), oneLine(s.Command, 50))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Println()
	if _, err := os.Stat(path); err != nil {
		fmt.Printf("You have no collectors of your own. Create a starter file with:\n  hearsay collectors -init\n")
		fmt.Printf("It will be %s, and every command picks it up from there.\n", path)
	} else {
		fmt.Printf("Yours, from %s:\n", path)
		tw := newTable(os.Stdout)
		fmt.Fprintln(tw, "ID\tATTRIBUTE\tSYSTEMS\tCOMMAND")
		for _, s := range mine {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.ID, s.Attr, strings.Join(s.OS, ","), oneLine(s.Command, 50))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		if len(mine) == 0 {
			fmt.Println("  (the file has no collectors in it)")
		}
		fmt.Println("\nTest one with: hearsay collectors -try <id> -host <name>")
	}
	if len(eng.Externals) > 0 {
		fmt.Println("\nExternal programs:")
		for _, ex := range eng.Externals {
			fmt.Printf("  %s  %s\n", ex.Name, ex.Path)
		}
	}
	return nil
}

// tryCollector runs one spec and explains the result in words, including what
// to do when it did not work. Nothing is written to the database.
func tryCollector(ctx context.Context, eng *engine.Engine, st *store.Store, spec collect.Spec, host, sample string) error {
	if (host == "") == (sample == "") {
		return errors.New("-try needs either -host <name> to run the command, or -sample <file> to test the parse rule on saved output")
	}
	fmt.Printf("collector  %s\n", spec.ID)
	fmt.Printf("records    %s\n", spec.Attr)
	fmt.Printf("command    %s\n", spec.Command)

	var output string
	var runErr error
	if sample != "" {
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
		fmt.Printf("output     from %s\n", sample)
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
		fmt.Printf("host       %s (%s@%s)\n", host, prof.Username, prof.Address)
		res, err := eng.TrySpec(ctx, *prof, spec)
		if err != nil {
			return err
		}
		if !res.OSMatches {
			fmt.Printf("\nNOTE: the host says it is %q, and this collector only runs on %s.\n"+
				"It ran now because you asked, but a real collection will skip this host.\n"+
				"Add %q to its \"os\" list if it should run here.\n",
				res.OS, strings.Join(spec.OS, ", "), res.OS)
		}
		output, runErr = res.Reading.Output, res.Reading.Err
		if runErr != nil && !res.Reading.Ran {
			fmt.Printf("\nRESULT     the command failed: %v\n\n", runErr)
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
	fmt.Printf("\nRESULT     %s = %q\n", spec.Attr, value)
	fmt.Println("\nThis is what a collection would record. Nothing was saved. Run hearsay refresh to collect for real.")
	return nil
}

func parseKindOf(s collect.Spec) string {
	if s.Parse.Kind == "" {
		return string(collect.ParseFirstLine)
	}
	return string(s.Parse.Kind)
}
