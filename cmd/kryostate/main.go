// Command kryostate keeps an inventory of an estate: the hosts, the stacks they
// belong to and what they run come from people and documents, and everything
// else is what the machines themselves report.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/engine"
	"github.com/KalebHawkins/kryostate/internal/ingest"
	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/normalize"
	"github.com/KalebHawkins/kryostate/internal/query"
	"github.com/KalebHawkins/kryostate/internal/secret"
	"github.com/KalebHawkins/kryostate/internal/store"
	"github.com/KalebHawkins/kryostate/internal/transfer"
	"github.com/KalebHawkins/kryostate/internal/web"
)

const usage = `kryostate — an inventory of servers, read from the servers themselves

Usage: kryostate <command> [flags]

Everyday:
  hosts          list hosts, filtered by a query (-q), with chosen attributes as columns
  show           everything known about one host
  refresh        collect and run checks, in one go
  set            state a host's application, stack or role yourself
  collectors     list, switch on, test and schedule collectors; create your own
  serve          run the web interface, and the collection schedules

Setup and maintenance:
  import         read hosts, stacks, roles and applications from a Markdown or CSV inventory
  collect        connect to every enabled target and record what it reports
  check          run the liveness checks and record the results
  resolve        look names up in DNS and record the answers
  renormalize    re-run normaliser rules over values already collected
  reparse        re-run collector parse rules over stored command output
  evidence       list or delete the stored output of collector commands
  host           add a host
  target         add or list a connection profile
  stack          define a stack or add members
  secret         manage credential references
  notify         send a daily digest of changes to chat or email
  export         write the whole store to a JSON file
  restore        load a JSON file into an empty store

Wherever a command takes a host, a name, fqdn, or id (or its first 6+
characters) is accepted. Run "kryostate <command> -h" for the flags of one
command.
`

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "hosts", "ls":
		err = cmdHosts(ctx, os.Args[2:])
	case "show":
		err = cmdShow(ctx, os.Args[2:])
	case "set":
		err = cmdSet(ctx, os.Args[2:])
	case "collectors":
		err = cmdCollectors(ctx, os.Args[2:])
	case "refresh":
		err = cmdRefresh(ctx, log, os.Args[2:])
	case "serve":
		err = cmdServe(ctx, log, os.Args[2:])
	case "import":
		err = cmdImport(ctx, os.Args[2:])
	case "collect":
		err = cmdCollect(ctx, log, os.Args[2:])
	case "check":
		err = cmdCheck(ctx, os.Args[2:])
	case "resolve":
		err = cmdResolve(ctx, os.Args[2:])
	case "renormalize":
		err = cmdRenormalize(ctx, os.Args[2:])
	case "reparse":
		err = cmdReparse(ctx, os.Args[2:])
	case "evidence":
		err = cmdEvidence(ctx, os.Args[2:])
	case "host":
		err = cmdHost(ctx, os.Args[2:])
	case "target":
		err = cmdTarget(ctx, os.Args[2:])
	case "stack":
		err = cmdStack(ctx, os.Args[2:])
	case "secret":
		err = cmdSecret(ctx, os.Args[2:])
	case "notify":
		err = cmdNotify(ctx, os.Args[2:])
	case "export":
		err = cmdExport(ctx, os.Args[2:])
	case "restore":
		err = cmdRestore(ctx, os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "kryostate:", err)
		os.Exit(1)
	}
}

// dbFlag is repeated rather than made global so each subcommand's -h output
// names it.
func dbFlag(fs *flag.FlagSet) *string {
	return fs.String("db", defaultDB(), "path to the database file")
}

func defaultDB() string {
	if v := os.Getenv("KRYOSTATE_DB"); v != "" {
		return v
	}
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

func cmdServe(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	db := dbFlag(fs)
	addr := fs.String("addr", "127.0.0.1:8099", "address to listen on")
	rules := fs.String("rules", "", "extra normaliser rules file")
	collectors := fs.String("collectors", "", "collector spec file to use instead of collectors.json beside the database")
	externals := fs.String("externals", "", "file listing external collector programs")
	stale := fs.Duration("stale-after", 14*24*time.Hour, "when the built-in collectors run only by hand: how long after its last good collection the overview calls a host stale (otherwise twice their shortest schedule)")
	noEvidence := fs.Bool("no-evidence", false, "do not keep what each collector command printed")
	trusted := fs.String("trusted-origin", "", "comma-separated origins, such as https://cmdb.example.com, allowed to submit forms when a proxy serves this under another name")
	allowContent := fs.Bool("allow-content-collectors", false, "let the web interface switch on catalog collectors that read the contents of files and variables")
	noSchedule := fs.Bool("no-schedule", false, "do not run collectors on their schedules; collect only when somebody asks")
	fs.Parse(args)

	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	// A broken collectors.json does not stop the server: the collectors page
	// says what is wrong with it, which is where someone fixing it will look.
	// Every other command still refuses to run with it.
	files := engineFiles{Rules: *rules, Collectors: *collectors, Externals: *externals, NoEvidence: *noEvidence}.resolved()
	collectorsErr := ""
	if files.Collectors != "" {
		if err := checkCollectorsFile(files.Collectors); err != nil {
			collectorsErr = err.Error()
			files.SkipCollectors = true
			log.Error("collectors file not loaded; its collectors will not run until it is fixed and the server restarted",
				"file", files.Collectors, "err", err)
		}
	}
	eng, err := buildEngine(st, files, log)
	if err != nil {
		return err
	}
	srv, err := web.New(st, eng, log)
	if err != nil {
		return err
	}
	srv.StaleAfter = *stale
	srv.TrustedOrigins = splitIDs(*trusted)
	srv.AllowRevealing = *allowContent
	// The scheduler stops with the server, and the store is not closed
	// until a run in flight has written its results.
	if !*noSchedule && srv.Scheduler != nil {
		sctx, stopSched := context.WithCancel(ctx)
		schedDone := make(chan struct{})
		go func() {
			defer close(schedDone)
			srv.Scheduler.Run(sctx)
		}()
		defer func() {
			stopSched()
			<-schedDone
			if srv.Notifier != nil {
				srv.Notifier.Wait()
			}
		}()
		log.Info("scheduler on", "sessions", srv.Scheduler.MaxSessions, "per_host", srv.Scheduler.PerHost)
	} else {
		log.Info("scheduler off; collectors run only when asked")
	}
	srv.Config = web.ConfigFiles{Dir: dataDir, Collectors: files.Collectors,
		Rules: files.Rules, Externals: files.Externals, CollectorsErr: collectorsErr}
	for _, p := range []string{files.Collectors, files.Rules, files.Externals} {
		if p != "" && (p != files.Collectors || collectorsErr == "") {
			log.Info("loaded", "file", p)
		}
	}
	h := &http.Server{
		Addr:              *addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sd, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.Shutdown(sd)
	}()
	log.Info("listening", "addr", *addr, "db", *db)
	if err := h.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func cmdImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	db := dbFlag(fs)
	name := fs.String("name", "", "name for this document, used as the source")
	mapPath := fs.String("map", "", "JSON file describing which column means what")
	fs.Parse(args)

	if *name == "" || fs.NArg() != 1 {
		return errors.New("usage: kryostate import -name <source> [-map map.json] <file.md|file.csv>  (without -map, columns such as Host, Address, Stack, Role and Application are recognised by name)")
	}
	path := fs.Arg(0)

	var m ingest.Mapping
	if *mapPath != "" {
		b, err := os.ReadFile(*mapPath)
		if err != nil {
			return err
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		// A misspelled key in a mapping file would otherwise mean a column is
		// silently never imported, and the absence is very hard to notice.
		dec.DisallowUnknownFields()
		if err := dec.Decode(&m); err != nil {
			return fmt.Errorf("parse mapping: %w", err)
		}
	}
	if *mapPath != "" && m.HostColumn == "" {
		return errors.New(`the mapping needs "host_column": nothing can be imported without knowing which column names the host`)
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var tables []ingest.Table
	if strings.EqualFold(filepath.Ext(path), ".csv") {
		t, err := ingest.ParseCSV(f, "")
		if err != nil {
			return err
		}
		tables = []ingest.Table{t}
	} else {
		tables, err = ingest.ParseMarkdown(f)
		if err != nil {
			return err
		}
	}
	if *mapPath == "" {
		guessed, notes, err := ingest.GuessMapping(tables)
		if err != nil {
			return err
		}
		m = guessed
		fmt.Println("no -map given; read the columns as:")
		for _, n := range notes {
			fmt.Println("  " + n)
		}
	}

	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	norm, err := normalize.Builtin()
	if err != nil {
		return err
	}
	im := &ingest.Importer{Store: st, Norm: norm, Name: *name}
	res, err := im.Import(ctx, tables, m)
	if err != nil {
		return err
	}
	fmt.Printf("%d rows: %d hosts created, %d matched, %d applications recorded, %d rows skipped\n",
		res.Rows, res.Created, res.Matched, res.Asserted, res.Skipped)
	for sec, n := range res.BySection {
		// A document with no headings reports one unnamed group, which is
		// just the row count again under a blank label.
		if sec == "" {
			continue
		}
		fmt.Printf("  %-40s %d\n", sec, n)
	}
	if len(res.Ignored) > 0 {
		fmt.Printf("not imported: %s (an inventory supplies hosts, addresses, stacks, roles and applications; everything else is collected from the host)\n",
			strings.Join(res.Ignored, ", "))
	}
	for _, p := range res.Problems {
		fmt.Println("  problem:", p)
	}
	return nil
}

func cmdCollect(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("collect", flag.ExitOnError)
	db := dbFlag(fs)
	transport := fs.String("transport", "", "limit to one transport (ssh or winrm)")
	only := fs.String("only", "", "comma-separated hosts (names or ids)")
	conc := fs.Int("concurrency", 8, "how many hosts at once")
	timeout := fs.Duration("host-timeout", 2*time.Minute, "limit for everything done against one host")
	rules := fs.String("rules", "", "extra normaliser rules file")
	collectors := fs.String("collectors", "", "collector spec file to use instead of collectors.json beside the database")
	externals := fs.String("externals", "", "file listing external collector programs")
	noEvidence := fs.Bool("no-evidence", false, "do not keep what each collector command printed")
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
	opt := engine.Options{
		Transport: *transport, Concurrency: *conc, Only: ids,
		HostTimeout: *timeout, Actor: actor(),
	}
	sum, err := runCollect(ctx, eng, opt, false)
	if err != nil {
		return err
	}
	fmt.Printf("run %s: %d targets, %d ok, %d failed\n", sum.ID, sum.Targets, sum.Succeeded, sum.Failed)
	if sum.Targets == 0 {
		fmt.Println("no enabled targets; add one with: kryostate target -host <name> -address <addr> -user <user> -credential agent:")
	}
	return nil
}

// cmdCheck defines liveness checks and runs them. Defining and running share
// a command because the first thing anyone wants after adding a check is to
// know whether it passes.
func cmdCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	db := dbFlag(fs)
	ci := hostFlag(fs, "host the check belongs to")
	name := fs.String("name", "", "a name for this check, unique per host")
	tcp := fs.String("tcp", "", "host:port to connect to")
	httpURL := fs.String("http", "", "URL to request")
	expect := fs.String("expect", "", "status code or pattern such as 2xx, for -http")
	timeout := fs.Duration("timeout", 5*time.Second, "limit for one check")
	del := fs.Bool("delete", false, "remove the named check from -host")
	list := fs.Bool("list", false, "list the checks on -host")
	run := fs.Bool("run", false, "run every enabled check and record the results")
	only := fs.String("only", "", "comma-separated hosts (names or ids), with -run")
	disable := fs.Bool("disable", false, "keep the check but stop running it")
	fs.Parse(args)

	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	label := *ci
	if *ci != "" {
		if *ci, err = st.LookupCI(ctx, *ci); err != nil {
			return err
		}
	}

	switch {
	case *run:
		eng, err := buildEngine(st, engineFiles{}, nil)
		if err != nil {
			return err
		}
		ids, err := st.LookupCIs(ctx, splitIDs(*only))
		if err != nil {
			return err
		}
		results, err := eng.RunChecks(ctx, ids, 16)
		if err != nil {
			return err
		}
		if len(results) == 0 {
			fmt.Println("no checks defined; add one with: kryostate check -host <name> -name <name> -tcp host:port")
			return nil
		}
		for _, r := range results {
			status := "ok"
			if r.Outcome != model.OutcomeOK {
				status = string(r.Outcome)
			}
			fmt.Printf("%-24s %-16s %-8s %6dms  %s\n",
				r.Check.Label, r.Check.Name, status, r.Took.Milliseconds(), r.Detail)
		}
		ok, failed := engine.CheckSummary(results)
		fmt.Printf("%d ok, %d failed\n", ok, failed)
		return nil

	case *list:
		if *ci == "" {
			return errors.New("-list needs -host")
		}
		cs, err := st.Checks(ctx, *ci)
		if err != nil {
			return err
		}
		for _, c := range cs {
			state := "enabled"
			if !c.Enabled {
				state = "disabled"
			}
			fmt.Printf("%-16s %-5s %-40s %s\n", c.Name, c.Kind, c.Target, state)
		}
		return nil

	case *del:
		if *ci == "" || *name == "" {
			return errors.New("-delete needs -host and -name")
		}
		return st.DeleteCheck(ctx, *ci, *name)
	}

	if *ci == "" || *name == "" || (*tcp == "" && *httpURL == "") {
		return errors.New("usage: kryostate check -host <host> -name <name> (-tcp host:port | -http url [-expect 2xx]) | -list -host <host> | -run")
	}
	c := store.Check{
		CIID: *ci, Name: *name, Expect: *expect,
		Timeout: *timeout, Enabled: !*disable,
	}
	if *tcp != "" {
		c.Kind, c.Target = "tcp", *tcp
	} else {
		c.Kind, c.Target = "http", *httpURL
	}
	if err := st.PutCheck(ctx, c); err != nil {
		return err
	}
	fmt.Printf("check %s on %s recorded; run it with: kryostate check -run -only %s\n", c.Name, label, label)
	return nil
}

func cmdResolve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("resolve", flag.ExitOnError)
	db := dbFlag(fs)
	suffixes := fs.String("suffixes", "", "comma-separated DNS suffixes to try, in order")
	server := fs.String("server", "", "resolver address, blank for the system resolver")
	fs.Parse(args)

	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	eng, err := buildEngine(st, engineFiles{}, nil)
	if err != nil {
		return err
	}
	opt := engine.ResolveOptions{Resolver: *server}
	if *suffixes != "" {
		opt.Suffixes = strings.Split(*suffixes, ",")
	}
	sum, err := eng.Resolve(ctx, opt)
	if err != nil {
		return err
	}
	fmt.Printf("%d checked: %d resolved, %d resolve nowhere, %d answer with several addresses and no choice recorded\n",
		sum.Checked, sum.Resolved, sum.Unresolved, sum.Multi)
	if sum.Failed > 0 {
		fmt.Printf("%d lookups failed outright and were not counted either way\n", sum.Failed)
	}
	return nil
}

func cmdRenormalize(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("renormalize", flag.ExitOnError)
	db := dbFlag(fs)
	attr := fs.String("attr", "", "SQL LIKE pattern for the attributes to redo, blank for all")
	rules := fs.String("rules", "", "extra normaliser rules file")
	fs.Parse(args)

	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	eng, err := buildEngine(st, engineFiles{Rules: *rules}, nil)
	if err != nil {
		return err
	}
	matched, unmatched, err := eng.Renormalize(ctx, *attr)
	if err != nil {
		return err
	}
	fmt.Printf("%d values normalised, %d still unmatched\n", matched, unmatched)
	return nil
}

// cmdReparse re-derives values from stored command output. It is the repair
// path for a collector rule that took the wrong substring, as distinct from
// renormalize, which repairs a rule that read the substring wrongly.
func cmdReparse(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("reparse", flag.ExitOnError)
	db := dbFlag(fs)
	attr := fs.String("attr", "", "SQL LIKE pattern for the attributes to redo, blank for all")
	spec := fs.String("spec", "", "single collector id to redo")
	ci := hostFlag(fs, "single host to redo")
	collectors := fs.String("collectors", "", "collector spec file to use instead of collectors.json beside the database")
	rules := fs.String("rules", "", "extra normaliser rules file")
	fs.Parse(args)

	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	if *ci != "" {
		if *ci, err = st.LookupCI(ctx, *ci); err != nil {
			return err
		}
	}
	eng, err := buildEngine(st, engineFiles{Rules: *rules, Collectors: *collectors}, nil)
	if err != nil {
		return err
	}
	sum, err := eng.Reparse(ctx, store.EvidenceFilter{CIID: *ci, SpecID: *spec, Attr: *attr})
	if err != nil {
		return err
	}
	if sum.Stored == 0 {
		fmt.Println("no stored command output matched; collect with evidence enabled first")
		return nil
	}
	if sum.Considered == 0 {
		fmt.Printf("%d stored outputs matched, but none names a collector this build knows; "+
			"pass the spec file with -collectors\n", sum.Stored)
		return nil
	}
	fmt.Printf("%d stored outputs read: %d values changed, %d recovered from nothing, %d still unparsed\n",
		sum.Considered, sum.Changed, sum.Recovered, sum.Failed)
	if sum.Skipped > 0 {
		fmt.Printf("%d skipped: the stored output was cut short at the size limit, so a value read from it\n"+
			"could be a prefix of the real one. Collect from those hosts again to repair them.\n", sum.Skipped)
	}
	return nil
}

// cmdEvidence lists or deletes stored command output.
func cmdEvidence(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("evidence", flag.ExitOnError)
	db := dbFlag(fs)
	ci := hostFlag(fs, "only this host")
	spec := fs.String("spec", "", "collector id")
	attr := fs.String("attr", "", "SQL LIKE pattern for attributes")
	forget := fs.Bool("forget", false, "delete the matching stored output instead of listing it")
	show := fs.Bool("show", false, "print the stored output, not just a summary of it")
	fs.Parse(args)

	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	if *ci != "" {
		if *ci, err = st.LookupCI(ctx, *ci); err != nil {
			return err
		}
	}
	f := store.EvidenceFilter{CIID: *ci, SpecID: *spec, Attr: *attr}

	if *forget {
		if f.CIID == "" && f.SpecID == "" && f.Attr == "" {
			return errors.New("refusing to delete every stored output without -host, -spec or -attr")
		}
		n, err := st.ForgetEvidence(ctx, f)
		if err != nil {
			return err
		}
		fmt.Printf("%d stored outputs deleted\n", n)
		return nil
	}

	ev, err := st.AllEvidence(ctx, f)
	if err != nil {
		return err
	}
	if len(ev) == 0 {
		fmt.Println("nothing stored")
		return nil
	}
	for _, e := range ev {
		marker := ""
		if e.Truncated {
			marker = " (truncated)"
		}
		fmt.Printf("%s  %s  %s  %d bytes%s  %s\n",
			e.CIID, e.SpecID, e.Attr, len(e.Output), marker, e.At.Format(time.RFC3339))
		if *show {
			fmt.Printf("  $ %s\n", e.Command)
			for _, line := range strings.Split(strings.TrimRight(e.Output, "\n"), "\n") {
				fmt.Printf("  | %s\n", line)
			}
		}
	}
	return nil
}

func cmdHost(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("host", flag.ExitOnError)
	db := dbFlag(fs)
	name := fs.String("name", "", "display name")
	fqdn := fs.String("fqdn", "", "fully qualified name, stronger identity evidence than a bare name")
	retire := fs.String("retire", "", "mark the given host (name or id) as gone")
	unretire := fs.String("unretire", "", "undo a retirement")
	reason := fs.String("reason", "", "why, recorded in the audit log")
	if pos := parseInterspersed(fs, args); len(pos) == 1 && *name == "" {
		// "kryostate host web01" reads as naturally as "kryostate host -name web01".
		*name = pos[0]
	} else if len(pos) > 0 {
		return errors.New("usage: kryostate host <name> [-fqdn <fqdn>]")
	}

	if *retire != "" || *unretire != "" {
		st, err := openStore(*db)
		if err != nil {
			return err
		}
		defer st.Close()
		if *retire != "" {
			id, err := st.LookupCI(ctx, *retire)
			if err != nil {
				return err
			}
			if err := st.Retire(ctx, id, actor(), *reason); err != nil {
				return err
			}
			fmt.Printf("retired %s (%s); history kept, collection stopped\n", *retire, shortID(id))
			return nil
		}
		id, err := st.LookupCI(ctx, *unretire)
		if err != nil {
			return err
		}
		if err := st.Unretire(ctx, id, actor(), *reason); err != nil {
			return err
		}
		fmt.Printf("restored %s (%s); re-enable its connection profile to collect again\n", *unretire, shortID(id))
		return nil
	}

	if *name == "" {
		return errors.New("usage: kryostate host <name> [-fqdn <fqdn>] | kryostate host -retire <host> [-reason <why>]")
	}
	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()

	ev := []model.Identity{{Scheme: model.IdentHostname, Value: *name}}
	if *fqdn != "" {
		ev = append([]model.Identity{{Scheme: model.IdentFQDN, Value: *fqdn}}, ev...)
	}
	id, created, err := st.ResolveCI(ctx, model.KindHost, *name, ev)
	if err != nil {
		return err
	}
	if created {
		fmt.Printf("created %s (id %s)\n", *name, id)
		fmt.Printf("next: kryostate target -host %s -user <user>\n", *name)
	} else {
		fmt.Printf("already known as %s\n", id)
	}
	return nil
}

func cmdTarget(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("target", flag.ExitOnError)
	db := dbFlag(fs)
	ci := hostFlag(fs, "host to reach")
	transport := fs.String("transport", "ssh", "ssh or winrm")
	address := fs.String("address", "", "address or name to connect to, blank for the host's fqdn or name")
	port := fs.Int("port", 0, "port, zero for the transport default")
	user := fs.String("user", "", "username")
	cred := fs.String("credential", "agent:", "credential reference, for example agent: or keyring:kryostate/ops")
	bastion := fs.String("bastion", "", "bastion as user@host:port/credential-ref")
	disable := fs.Bool("disable", false, "record the profile but never collect through it")
	forget := fs.Bool("forget-host-key", false, "with -host: clear the pinned host key, so the next collection trusts whatever key the host presents. Use it only after checking the host was really rebuilt")
	fs.Parse(args)

	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()

	if *ci == "" {
		profiles, err := st.CollectTargets(ctx, "")
		if err != nil {
			return err
		}
		if len(profiles) == 0 {
			fmt.Println("no enabled targets; add one with: kryostate target -host <name> -user <user>")
			return nil
		}
		tw := newTable(os.Stdout)
		fmt.Fprintln(tw, "HOST\tID\tTRANSPORT\tCONNECT AS\tCREDENTIAL")
		for _, p := range profiles {
			name := p.CIID
			if c, err := st.CI(ctx, p.CIID); err == nil {
				name = c.Label
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s@%s%s\t%s\n", name, shortID(p.CIID), p.Transport,
				p.Username, p.Address, portSuffix(p.Port), p.CredentialRef)
		}
		return tw.Flush()
	}
	ref := *ci
	if *ci, err = st.LookupCI(ctx, *ci); err != nil {
		return err
	}
	if *forget {
		profiles, err := st.ConnProfiles(ctx, *ci)
		if err != nil {
			return err
		}
		for _, p := range profiles {
			if p.Transport != *transport {
				continue
			}
			if p.HostKey == "" {
				fmt.Printf("%s has no pinned %s host key; the next collection will pin one.\n", ref, p.Transport)
				return nil
			}
			if err := st.PinHostKey(ctx, *ci, p.Transport, ""); err != nil {
				return err
			}
			if err := st.Audit(ctx, actor(), "forget host key", *ci, p.Transport+" "+p.HostKey); err != nil {
				return err
			}
			fmt.Printf("forgot the %s host key of %s. The next collection pins the key it presents then.\n", p.Transport, ref)
			fmt.Printf("collect now: kryostate refresh -only %s\n", ref)
			return nil
		}
		return fmt.Errorf("%s has no %s connection", ref, *transport)
	}
	if *address == "" {
		*address = defaultAddress(ctx, st, *ci)
	}
	if *address == "" || *user == "" || *cred == "" {
		return errors.New("a target needs -user (and -address when the host has no name to connect to)")
	}
	p := store.ConnProfile{
		CIID: *ci, Transport: *transport, Address: *address, Port: *port,
		Username: *user, CredentialRef: *cred, BastionRef: *bastion,
		Enabled: !*disable,
	}
	if err := st.PutConnProfile(ctx, p); err != nil {
		return err
	}
	fmt.Printf("recorded: %s via %s as %s@%s using %s\n", ref, p.Transport, p.Username, p.Address, p.CredentialRef)
	fmt.Printf("try it with: kryostate refresh -only %s\n", ref)
	return nil
}

// defaultAddress picks the strongest name a host is known by: its fqdn, then
// its hostname, then its label.
func defaultAddress(ctx context.Context, st *store.Store, ciID string) string {
	ids, _ := st.Identities(ctx, ciID)
	for _, scheme := range []model.IdentityScheme{model.IdentFQDN, model.IdentHostname} {
		for _, i := range ids {
			if i.Scheme == scheme && i.Value != "" {
				return i.Value
			}
		}
	}
	if c, err := st.CI(ctx, ciID); err == nil {
		return c.Label
	}
	return ""
}

func cmdStack(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stack", flag.ExitOnError)
	db := dbFlag(fs)
	name := fs.String("name", "", "stack name")
	desc := fs.String("description", "", "what this stack is")
	owner := fs.String("owner", "", "who is responsible for it")
	env := fs.String("environment", "", "environment label")
	add := fs.String("add", "", "comma-separated hosts to add, each as name or name:role")
	file := fs.String("file", "", "file listing hosts to add, one per line: name, then role, optionally an fqdn; - reads standard input")
	create := fs.Bool("create", false, "with -add or -file: record hosts that are not known yet")
	user := fs.String("user", "", "with -add or -file: give hosts with no connection one that logs in as this user through ssh-agent")
	remove := fs.String("remove", "", "comma-separated hosts (names or ids) to take out")
	role := fs.String("role", "", "role for added members that do not name their own")
	app := fs.String("application", "", "with -add or -file: the application the added hosts run")
	q := fs.String("q", "", `add every recorded host matching a query, such as 'os.family == "Linux"', in the -role role; once, the stack does not follow the query afterwards (the query language of hosts -q)`)
	fs.Parse(args)

	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()

	if *name == "" {
		return listStacks(ctx, st)
	}

	// Membership changes on an existing stack must not wipe the description,
	// owner and environment just because those flags were not repeated.
	views, err := st.Stacks(ctx)
	if err != nil {
		return err
	}
	stk := model.Stack{Name: *name}
	for _, v := range views {
		if v.Stack.Name == *name {
			stk = v.Stack
		}
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "description":
			stk.Description = *desc
		case "owner":
			stk.Owner = *owner
		case "environment":
			stk.Environment = *env
		}
	})
	id, err := st.PutStack(ctx, stk)
	if err != nil {
		return err
	}

	// Hosts to add come from -add, as name or name:role, and from -file, one
	// per line in the same forms the web page accepts.
	var lines []ingest.HostLine
	var problems []string
	for _, item := range splitIDs(*add) {
		ls, ps := ingest.ParseHostList(item)
		lines, problems = append(lines, ls...), append(problems, ps...)
	}
	if *file != "" {
		var b []byte
		var err error
		if *file == "-" {
			b, err = io.ReadAll(os.Stdin)
		} else {
			b, err = os.ReadFile(*file)
		}
		if err != nil {
			return err
		}
		ls, ps := ingest.ParseHostList(string(b))
		lines, problems = append(lines, ls...), append(problems, ps...)
	}
	for i := range lines {
		if lines[i].Role == "" {
			lines[i].Role = *role
		}
	}
	if len(lines) > 0 {
		res, err := ingest.AddHostsToStack(ctx, st, *name, lines,
			ingest.BulkOptions{Create: *create, User: *user, Application: *app, By: actor()})
		if err != nil {
			return err
		}
		res.Problems = append(problems, res.Problems...)
		fmt.Println(res.Summary(*name))
		if len(res.Problems) > 0 && !*create {
			fmt.Println("To add hosts that are not recorded yet, repeat with -create.")
		}
	}

	if *q != "" {
		res, err := web.AddMatchingToStack(ctx, st, *name, *q, *role)
		var qe *query.Error
		if errors.As(err, &qe) {
			return fmt.Errorf("query: %s\n  %s\n  %s", qe.Msg, *q, qe.Caret())
		} else if err != nil {
			return err
		}
		fmt.Println(res.Summary(*name))
	}

	removeIDs, err := st.LookupCIs(ctx, splitIDs(*remove))
	if err != nil {
		return err
	}
	for i, ci := range removeIDs {
		ok, err := st.RemoveFromStack(ctx, id, ci)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Printf("%s was not a member\n", splitIDs(*remove)[i])
		}
	}
	members, err := st.StackMembers(ctx, *name)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(members))
	for _, m := range members {
		names = append(names, m.CI.Label)
	}
	fmt.Printf("stack %s saved, members (%d): %s\n", *name, len(members), strings.Join(names, ", "))
	return nil
}

func listStacks(ctx context.Context, st *store.Store) error {
	views, err := st.Stacks(ctx)
	if err != nil {
		return err
	}
	if len(views) == 0 {
		fmt.Println("no stacks; define one with: kryostate stack -name <name> -add <host,...>")
		return nil
	}
	tw := newTable(os.Stdout)
	fmt.Fprintln(tw, "STACK\tENV\tOWNER\tMEMBERS\tCOLLECTED\tFAILED\tNEVER")
	for _, v := range views {
		h := v.Health
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%d\n", v.Stack.Name, dash(v.Stack.Environment),
			dash(v.Stack.Owner), h.Members, h.Collected, h.Failed, h.NeverSeen)
	}
	return tw.Flush()
}

func cmdSecret(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("secret", flag.ExitOnError)
	db := dbFlag(fs)
	set := fs.String("set", "", "store material in the OS credential store, as service/account")
	del := fs.String("delete", "", "remove material from the OS credential store, as service/account")
	test := fs.String("test", "", "resolve a reference and report only whether it worked")
	fs.Parse(args)

	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	reg := secret.NewRegistry(nil)

	switch {
	case *set != "":
		service, account, ok := strings.Cut(*set, "/")
		if !ok {
			return errors.New("-set takes service/account")
		}
		// Read from the terminal rather than a flag, because a flag value is
		// in the process list and in the shell history. Nothing is echoed, and
		// the whole line is taken, so a passphrase may contain spaces.
		v, err := readSecret("value: ")
		if err != nil {
			return err
		}
		if v == "" {
			return errors.New("refusing to store an empty value")
		}
		if err := secret.StoreKeyring(service, account, v); err != nil {
			return err
		}
		fmt.Printf("stored; reference it as keyring:%s/%s\n", service, account)
		return nil
	case *del != "":
		service, account, ok := strings.Cut(*del, "/")
		if !ok {
			return errors.New("-delete takes service/account")
		}
		return secret.DeleteKeyring(service, account)
	case *test != "":
		s, err := reg.Resolve(ctx, *test)
		if err != nil {
			return err
		}
		defer s.Zero()
		fmt.Printf("resolved, kind %s\n", s.Kind)
		return nil
	default:
		fmt.Println("reference schemes this build understands:")
		for _, s := range reg.Schemes() {
			fmt.Println("  " + s + ":")
		}
		return nil
	}
}

func cmdExport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	db := dbFlag(fs)
	out := fs.String("out", "", "file to write, blank for standard output")
	fs.Parse(args)

	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	d, err := transfer.Export(ctx, st)
	if err != nil {
		return err
	}
	w := os.Stdout
	if *out != "" {
		f, err := os.OpenFile(*out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	return d.Write(w)
}

func cmdRestore(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	db := dbFlag(fs)
	in := fs.String("in", "", "file to read")
	fs.Parse(args)

	if *in == "" {
		return errors.New("usage: kryostate restore -db <new.db> -in <dump.json>")
	}
	f, err := os.Open(*in)
	if err != nil {
		return err
	}
	defer f.Close()
	d, err := transfer.Read(f)
	if err != nil {
		return err
	}
	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := transfer.Restore(ctx, st, d); err != nil {
		return err
	}
	for _, t := range d.Order {
		fmt.Printf("%-18s %d\n", t, d.Counts[t])
	}
	return nil
}

// actor names whoever ran the command, for the audit trail.
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

func actor() string {
	for _, k := range []string{"KRYOSTATE_ACTOR", "USER", "USERNAME"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return "unknown"
}
