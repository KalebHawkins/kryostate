// Command kryostate keeps an inventory of an estate: the hosts, the stacks they
// belong to and what they run come from people and documents, and everything
// else is what the machines themselves report.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"golang.org/x/term"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/engine"
	"github.com/KalebHawkins/kryostate/internal/normalize"
	"github.com/KalebHawkins/kryostate/internal/secret"
	"github.com/KalebHawkins/kryostate/internal/store"
	"github.com/KalebHawkins/kryostate/internal/version"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := newRootCmd(log).ExecuteContext(ctx)
	stop()
	if err != nil {
		lipgloss.Fprintln(os.Stderr, render(os.Stderr, styleBad, "kryostate:"), err)
		os.Exit(1)
	}
}

// app is what every command shares: the logger, and the global flags.
type app struct {
	log    *slog.Logger
	db     string
	config string
}

// newRootCmd builds the whole command tree. It is built afresh for each run,
// so tests can execute commands without state leaking between them.
func newRootCmd(log *slog.Logger) *cobra.Command {
	a := &app{log: log}
	root := &cobra.Command{
		Use:   "kryostate",
		Short: "an inventory of servers, read from the servers themselves",
		Long: `kryostate — an inventory of servers, read from the servers themselves

The hosts, the stacks they belong to and what they run come from people and
documents; everything else is what the machines themselves report.

Wherever a command takes a host, a name, fqdn, or id (or its first 6+
characters) is accepted.

Settings come from flags, then KRYOSTATE_* environment variables (such as
KRYOSTATE_DB or KRYOSTATE_ADDR), then the config file, then the defaults.`,
		Version:           version.Short(),
		SilenceUsage:      true,
		SilenceErrors:     true,
		DisableAutoGenTag: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return a.loadConfig(cmd)
		},
	}
	root.SetVersionTemplate(version.Get().String())
	pf := root.PersistentFlags()
	pf.StringVar(&a.db, "db", defaultDB(), "path to the database file (env KRYOSTATE_DB)")
	pf.StringVar(&a.config, "config", "", "config file (env KRYOSTATE_CONFIG; default "+displayPath(defaultConfigPath())+" when it exists)")

	root.AddGroup(
		&cobra.Group{ID: "everyday", Title: "Everyday:"},
		&cobra.Group{ID: "resources", Title: "Inventory:"},
		&cobra.Group{ID: "ops", Title: "Collection and maintenance:"},
	)
	add := func(group string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = group
			root.AddCommand(c)
		}
	}
	add("everyday", a.refreshCmd(), a.serveCmd(), a.hostShowCmd("show"), a.hostUpdateCmd("set"))
	add("resources", a.hostCmd(), a.targetCmd(), a.stackCmd(), a.secretCmd(),
		a.collectorsCmd(), a.rulesCmd(), a.notifyCmd(), a.evidenceCmd())
	add("ops", a.collectCmd(), a.checkCmd(), a.resolveCmd(), a.renormalizeCmd(), a.reparseCmd(),
		a.importCmd(), a.exportCmd(), a.restoreCmd(), a.versionCmd())
	return root
}

// group is a noun whose verbs are its subcommands. Run on its own it prints
// its help; an unknown verb is an error rather than a silent help page.
func group(use, short string, aliases ...string) *cobra.Command {
	return &cobra.Command{
		Use: use, Short: short, Aliases: aliases, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
}

// configurable are the flags that may also come from the environment or the
// config file. The rest are what a command is being asked to do, which
// belongs on its command line.
var configurable = map[string]bool{
	"db": true, "addr": true, "concurrency": true, "host-timeout": true,
	"rules": true, "collectors": true, "externals": true, "no-evidence": true,
	"stale-after": true, "trusted-origin": true, "allow-content-collectors": true,
	"no-schedule": true, "wait": true, "no-wait": true,
}

// envName is the environment variable for a flag: KRYOSTATE_HOST_TIMEOUT for
// --host-timeout.
func envName(flag string) string {
	return "KRYOSTATE_" + strings.ToUpper(strings.ReplaceAll(flag, "-", "_"))
}

func defaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "kryostate", "config.yaml")
}

func displayPath(p string) string {
	if p == "" {
		return "none"
	}
	return p
}

// loadConfig fills every configurable flag not given on the command line from
// KRYOSTATE_* environment variables, then from the config file. In the file a
// setting may be at the top level, or under the command's name (nested for a
// subcommand, as host: add:) to apply to that command alone:
//
//	db: /var/lib/kryostate/kryostate.db
//	serve:
//	  addr: 0.0.0.0:8099
func (a *app) loadConfig(cmd *cobra.Command) error {
	v := viper.New()
	path, explicit := a.config, a.config != ""
	if !explicit {
		if e := os.Getenv("KRYOSTATE_CONFIG"); e != "" {
			path, explicit = e, true
		} else {
			path = defaultConfigPath()
		}
	}
	if path != "" {
		v.SetConfigFile(path)
		if filepath.Ext(path) == "" {
			v.SetConfigType("yaml")
		}
		if err := v.ReadInConfig(); err != nil && (explicit || !errors.Is(err, fs.ErrNotExist)) {
			return fmt.Errorf("config file %s: %w", path, err)
		}
	}
	// The section for "kryostate host add" is host.add.
	section := strings.ReplaceAll(strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" "), " ", ".")
	var ferr error
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if ferr != nil || f.Changed || !configurable[f.Name] {
			return
		}
		if err := v.BindEnv(f.Name, envName(f.Name)); err != nil {
			ferr = err
			return
		}
		var val any
		switch {
		case os.Getenv(envName(f.Name)) != "":
			val = v.Get(f.Name)
		case v.InConfig(section + "." + f.Name):
			val = v.Get(section + "." + f.Name)
		case v.InConfig(f.Name):
			val = v.Get(f.Name)
		default:
			return
		}
		if _, isMap := val.(map[string]any); isMap {
			return // a section named like the flag, such as collectors:
		}
		if err := f.Value.Set(fmt.Sprint(val)); err != nil {
			ferr = fmt.Errorf("%s (from %s or the config file): %w", f.Name, envName(f.Name), err)
		}
	})
	return ferr
}

func defaultDB() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "kryostate.db"
	}
	return filepath.Join(dir, "kryostate", "kryostate.db")
}

// dataDir is the folder holding the database this process opened. Custom
// collector, rule and external files placed there are picked up without a
// flag, so a new user's collector file works with every command.
var dataDir string

func openStore(path string) (*store.Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		dataDir = filepath.Dir(path)
	}
	return store.Open(path)
}

// The names of the files looked for in dataDir.
const (
	defaultCollectorsFile = "collectors.json"
	defaultRulesFile      = "rules.json"
	defaultExternalsFile  = "externals.json"
)

// engineFiles are the optional overrides of what is compiled in.
type engineFiles struct {
	Rules      string
	Collectors string
	Externals  string
	// NoEvidence turns off keeping what each command printed. The repair
	// path for a bad parse rule then requires contacting every host again.
	NoEvidence bool
	// SkipCollectors leaves the collectors file out, for a server that
	// found it broken and runs on without it.
	SkipCollectors bool
}

// resolved fills each file not named by a flag with the default file in the
// database folder, when that file exists.
func (f engineFiles) resolved() engineFiles {
	if dataDir == "" {
		return f
	}
	pick := func(flagged *string, name string) {
		if *flagged != "" {
			return
		}
		p := filepath.Join(dataDir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			*flagged = p
		}
	}
	pick(&f.Collectors, defaultCollectorsFile)
	pick(&f.Rules, defaultRulesFile)
	pick(&f.Externals, defaultExternalsFile)
	return f
}

// buildEngine wires the pieces that talk to hosts. Rule and collector files
// are optional overrides of what is compiled in, so a site can add a command
// without rebuilding.
func buildEngine(st *store.Store, f engineFiles, log *slog.Logger) (*engine.Engine, error) {
	f = f.resolved()
	norm, err := normalize.Builtin()
	if err != nil {
		return nil, err
	}
	if f.Rules != "" {
		b, err := os.ReadFile(f.Rules)
		if err != nil {
			return nil, err
		}
		if err := norm.Merge(b); err != nil {
			return nil, fmt.Errorf("%s: %w", f.Rules, err)
		}
	}
	specs, err := collect.BuiltinSpecs()
	if err != nil {
		return nil, err
	}
	if f.Collectors != "" && !f.SkipCollectors {
		b, err := os.ReadFile(f.Collectors)
		if err != nil {
			return nil, err
		}
		// Naming the file matters here: it may have been picked up without
		// a flag, and an error that does not say where would be a puzzle.
		if err := specs.Merge(b); err != nil {
			return nil, fmt.Errorf("%s: %w", f.Collectors, err)
		}
	}
	var externals []collect.External
	if f.Externals != "" {
		externals, err = engine.LoadExternals(f.Externals)
		if err != nil {
			return nil, err
		}
	}
	// Every credential lookup is written to the audit trail with the
	// reference redacted, so "which keys did this run touch" is answerable
	// afterwards.
	reg := secret.NewRegistry(func(ctx context.Context, action, ref, detail string) {
		_ = st.Audit(ctx, "kryostate", "credential "+action, "", ref+" "+detail)
		if log != nil && detail != "" {
			log.Debug("credential lookup", "action", action, "ref", ref, "detail", detail)
		}
	})
	eng := &engine.Engine{Store: st, Specs: specs, Norm: norm, Secret: reg, Externals: externals,
		KeepEvidence: !f.NoEvidence, Log: log}
	eng.Wire()
	return eng, nil
}

// checkCollectorsFile reports whether a collectors file would load beside the
// built-in specs.
func checkCollectorsFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	specs, err := collect.BuiltinSpecs()
	if err != nil {
		return err
	}
	return specs.Merge(b)
}

// readSecret takes one line without echoing it. A pipe is accepted so the
// command can be driven from a script, which is the only way to set a value on
// a machine with no terminal attached.
func readSecret(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, prompt)
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		return string(b), err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// splitIDs turns a comma-separated flag into a list, ignoring the blanks a
// trailing comma leaves behind.
func splitIDs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// onlyIDs flattens a repeatable, comma-separated flag such as --only.
func onlyIDs(vals []string) []string {
	return splitIDs(strings.Join(vals, ","))
}

// actor names whoever ran the command, for the audit trail.
func actor() string {
	for _, k := range []string{"KRYOSTATE_ACTOR", "USER", "USERNAME"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return "unknown"
}
