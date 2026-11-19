package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/engine"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// collectorsEnv is what every collectors subcommand works with: the store,
// and the collectors file, beside the database unless --collectors names one.
type collectorsEnv struct {
	st   *store.Store
	path string
	file string
}

func (e collectorsEnv) engine() (*engine.Engine, error) {
	return buildEngine(e.st, engineFiles{Collectors: e.file}, nil)
}

// templateParam is the one value a catalog template takes. A config.value
// template takes its file, key and section as one value.
type templateParam struct {
	param, key, section string
}

func (p *templateParam) add(cmd *cobra.Command, config bool) {
	cmd.Flags().StringVar(&p.param, "param", "", "the path, variable or package a catalog template needs")
	if config {
		cmd.Flags().StringVar(&p.key, "key", "", "with config.value: the key to read, such as max_connections")
		cmd.Flags().StringVar(&p.section, "section", "", "with config.value: only read the key inside this [section]")
	}
}

func (p templateParam) value(ref string) string {
	if t, ok := collect.Lookup(ref); ok && t.Param != nil && t.Param.Config() && p.key != "" {
		return collect.ConfigParam(p.param, p.section, p.key)
	}
	return p.param
}

func (a *app) collectorsCmd() *cobra.Command {
	cmd := group("collectors", "list, switch on, test and schedule collectors; add your own", "collector")
	var file string
	cmd.PersistentFlags().StringVar(&file, "collectors", "", "collector spec file, instead of "+defaultCollectorsFile+" next to the database")
	run := func(fn func(ctx context.Context, e collectorsEnv, args []string) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			st, err := openStore(a.db)
			if err != nil {
				return err
			}
			defer st.Close()
			e := collectorsEnv{st: st, path: filepath.Join(dataDir, defaultCollectorsFile), file: file}
			if file != "" {
				e.path = file
			}
			return fn(cmd.Context(), e, args)
		}
	}

	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Args: cobra.NoArgs,
		Short: "list the collectors that run: switched on from the catalog, your own and the built-in ones",
		RunE: run(func(ctx context.Context, e collectorsEnv, _ []string) error {
			eng, err := e.engine()
			if err != nil {
				return err
			}
			return listCollectors(ctx, eng, e.st, e.path)
		}),
	}

	catalog := &cobra.Command{
		Use: "catalog", Args: cobra.NoArgs,
		Short: "list the ready-made collectors that can be switched on",
		RunE: run(func(ctx context.Context, e collectorsEnv, _ []string) error {
			return listCatalog(ctx, e.st)
		}),
	}

	var (
		enableP             templateParam
		attr, stacks, where string
	)
	enable := &cobra.Command{
		Use:   "enable <catalog-name>",
		Short: "switch on a catalog collector, such as cpu.cores or file.exists, or a whole group",
		Example: `  kryostate collectors enable cpu.cores
  kryostate collectors enable file.exists --param /etc/resolv.conf --stacks web-prod
  kryostate collectors enable nginx.version --where 'name in ("web01", "web02")'`,
		Args: cobra.ExactArgs(1),
		RunE: run(func(ctx context.Context, e collectorsEnv, args []string) error {
			return enableCollector(ctx, e.st, args[0], enableP.value(args[0]), attr, stacks, where)
		}),
	}
	enableP.add(enable, true)
	enable.Flags().StringVar(&attr, "attr", "", "the attribute to record it as, blank for a sensible default")
	enable.Flags().StringVar(&stacks, "stacks", "", "comma-separated stacks to limit it to, the same as --where 'stack in (\"a\", \"b\")'")
	enable.Flags().StringVar(&where, "where", "", "a query choosing the hosts it runs on, such as 'name in (\"web01\", \"web02\")' or 'stack == \"web\" && os.family == \"Linux\"'; blank for every host")

	var disableP templateParam
	disable := &cobra.Command{
		Use:   "disable <catalog-name|#>",
		Short: "switch off a catalog collector, by its number in the list or its catalog name",
		Args:  cobra.ExactArgs(1),
		RunE: run(func(ctx context.Context, e collectorsEnv, args []string) error {
			c, err := findChoice(ctx, e.st, args[0], disableP.value(args[0]))
			if err != nil {
				return err
			}
			if err := e.st.DeleteChoice(ctx, c.ID, actor()); err != nil {
				return err
			}
			fmt.Printf("%s is off. Values it already collected are kept.\n", c.Label())
			return nil
		}),
	}
	disableP.add(disable, true)

	var (
		tryP         templateParam
		host, sample string
	)
	try := &cobra.Command{
		Use:   "try <name>",
		Short: "test a collector, by catalog name or id, and record nothing",
		Example: `  kryostate collectors try memory.total --host web01
  kryostate collectors try example.nodename --sample output.txt`,
		Args: cobra.ExactArgs(1),
		RunE: run(func(ctx context.Context, e collectorsEnv, args []string) error {
			eng, err := e.engine()
			if err != nil {
				return err
			}
			param := tryP.value(args[0])
			specs, err := specsToTry(ctx, eng, args[0], param)
			if err != nil {
				return err
			}
			hint := ""
			if _, ok := collect.Lookup(args[0]); ok {
				hint = "kryostate collectors enable " + args[0] + paramFlag(param)
			}
			return tryCollector(ctx, eng, e.st, specs, host, sample, hint)
		}),
	}
	tryP.add(try, true)
	try.Flags().StringVar(&host, "host", "", "run the command on this host")
	try.Flags().StringVar(&sample, "sample", "", "read command output from this file (- for standard input) instead of a host")
	try.MarkFlagsMutuallyExclusive("host", "sample")

	var showP templateParam
	show := &cobra.Command{
		Use:   "show <ref>",
		Short: "print a collector written out as in " + defaultCollectorsFile,
		Long: `Print a collector written out as in ` + defaultCollectorsFile + `, to copy as the start of
your own: a catalog name (with --param, or its example value),
builtin.<attribute>, spec.<id>, external.<name>, or any spec id.`,
		Example: "  kryostate collectors show file.exists --param /etc/hosts > " + defaultCollectorsFile,
		Args:    cobra.ExactArgs(1),
		RunE: run(func(ctx context.Context, e collectorsEnv, args []string) error {
			eng, err := e.engine()
			if err != nil {
				return err
			}
			return showDefinition(ctx, os.Stdout, eng, args[0], showP.value(args[0]))
		}),
	}
	showP.add(show, true)

	initCmd := &cobra.Command{
		Use: "init", Args: cobra.NoArgs,
		Short: "create a starter " + defaultCollectorsFile + " next to the database, for collectors of your own",
		RunE: run(func(_ context.Context, e collectorsEnv, _ []string) error {
			if _, err := os.Stat(e.path); err == nil {
				return fmt.Errorf("%s already exists; edit it, or delete it to start again", e.path)
			}
			if err := os.WriteFile(e.path, []byte(starterCollectors), 0o600); err != nil {
				return err
			}
			fmt.Printf("created %s with one example collector.\n\n", e.path)
			fmt.Println("next:")
			fmt.Printf("  1. edit it (the user guide explains each field: kryostate serve, then open /guide/collectors)\n")
			fmt.Printf("  2. test it:   kryostate collectors try example.nodename --host <name>\n")
			fmt.Printf("  3. collect:   kryostate refresh\n")
			return nil
		}),
	}

	schedules := &cobra.Command{
		Use: "schedules", Args: cobra.NoArgs,
		Short: "list when each collector runs, soonest first, grouped by time, with how many hosts each reaches",
		RunE: run(func(ctx context.Context, e collectorsEnv, _ []string) error {
			eng, err := e.engine()
			if err != nil {
				return err
			}
			return listSchedules(ctx, eng)
		}),
	}

	var (
		every, at       string
		manual, resetTo bool
	)
	schedule := &cobra.Command{
		Use:   "schedule <collector>",
		Short: "change when a collector runs on its own, such as builtin.os or cpu.cores",
		Example: `  kryostate collectors schedule builtin.os --every 1d --at 02:00
  kryostate collectors schedule cpu.cores --manual`,
		Args: cobra.ExactArgs(1),
		RunE: run(func(ctx context.Context, e collectorsEnv, args []string) error {
			eng, err := e.engine()
			if err != nil {
				return err
			}
			return setSchedule(ctx, eng, args[0], every, at, manual, resetTo)
		}),
	}
	schedule.Flags().StringVar(&every, "every", "", "how often, such as 30m, 6h, 1d or 7d")
	schedule.Flags().StringVar(&at, "at", "", "with a whole-day --every: the local time of day, such as 02:00 (without it, a time is picked from the collector's name)")
	schedule.Flags().BoolVar(&manual, "manual", false, "only run it when somebody collects")
	schedule.Flags().BoolVar(&resetTo, "default", false, "go back to the default schedule for its kind")

	var dryRun, example bool
	add := &cobra.Command{
		Use:   "add <file.json>",
		Short: "add your own collectors from a JSON file, or replace ones added before",
		Long: `Add your own collectors from a JSON file. The file holds one collector, a
list of them, or a whole collectors file ({"version": 1, "specs": [...]}),
each written as in ` + defaultCollectorsFile + `. Every collector is checked before any
is saved. One added before under the same id is replaced; an id that a
built-in collector, a catalog entry or the collectors file already uses is
refused. Added collectors are kept in the database, and a running server
collects, schedules and shows them without a restart. Use - to read the
file from standard input.`,
		Example: `  kryostate collectors add --example > mine.json
  kryostate collectors add mine.json --dry-run
  kryostate collectors add mine.json`,
		Args: func(cmd *cobra.Command, args []string) error {
			if example {
				return cobra.NoArgs(cmd, args)
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		RunE: run(func(ctx context.Context, e collectorsEnv, args []string) error {
			if example {
				fmt.Print(exampleCollector)
				return nil
			}
			eng, err := e.engine()
			if err != nil {
				return err
			}
			return addCollectors(ctx, os.Stdout, eng, args[0], dryRun)
		}),
	}
	add.Flags().BoolVar(&dryRun, "dry-run", false, "check the file and say what would be added, saving nothing")
	add.Flags().BoolVar(&example, "example", false, "print an example collector to start from")

	rm := &cobra.Command{
		Use: "rm <id>...", Aliases: []string{"remove"},
		Short: "remove collectors added with collectors add; the values they recorded are kept",
		Args:  cobra.MinimumNArgs(1),
		RunE: run(func(ctx context.Context, e collectorsEnv, args []string) error {
			eng, err := e.engine()
			if err != nil {
				return err
			}
			return removeCollectors(ctx, os.Stdout, eng, args)
		}),
	}

	cmd.AddCommand(list, catalog, enable, disable, try, show, initCmd, add, rm, schedules, schedule)
	return cmd
}

func enableCollector(ctx context.Context, st *store.Store, name, param, attr, stacks, where string) error {
	target, err := enableTarget(stacks, where)
	if err != nil {
		return err
	}
	if g, ok := collect.LookupGroup(name); ok {
		return enableGroup(ctx, st, g, target)
	}
	c, err := st.PutChoice(ctx, collect.Choice{Template: name, Param: param, Attr: attr,
		Target: target}, actor())
	if err != nil {
		if _, ok := collect.Lookup(name); !ok {
			return fmt.Errorf("%w; list the catalog with: kryostate collectors catalog", err)
		}
		return err
	}
	fmt.Printf("%s is on for %s, recorded as %s.\n", c.Label(), runsOn(c.Target), c.Attr)
	if c.Target != "" {
		fmt.Printf("see them:    kryostate host list -q '%s'\n", c.Target)
	}
	fmt.Printf("try it now:  kryostate collectors try %s%s --host <name>\n", c.Template, paramFlag(c.Param))
	fmt.Println("collect:     kryostate refresh")
	return nil
}
