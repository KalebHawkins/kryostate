// Command hearsay records what documents claim about an estate, reads what the
// machines actually report, and lists where the two disagree.
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

	"github.com/KalebHawkins/hearsay/internal/collect"
	"github.com/KalebHawkins/hearsay/internal/engine"
	"github.com/KalebHawkins/hearsay/internal/ingest"
	"github.com/KalebHawkins/hearsay/internal/model"
	"github.com/KalebHawkins/hearsay/internal/normalize"
	"github.com/KalebHawkins/hearsay/internal/secret"
	"github.com/KalebHawkins/hearsay/internal/store"
	"github.com/KalebHawkins/hearsay/internal/transfer"
	"github.com/KalebHawkins/hearsay/internal/web"
)

const usage = `hearsay — an inventory is hearsay until a machine confirms it

Usage: hearsay <command> [flags]

Commands:
  serve        run the web interface
  import       read a Markdown or CSV inventory as document claims
  collect      connect to every enabled target and record what it reports
  check        run the liveness checks and record the results
  resolve      look names up in DNS and record the answers
  detect       compare sources and raise disagreements
  renormalize  re-run normaliser rules over values already collected
  reparse      re-run collector parse rules over stored command output
  evidence     list or delete the stored output of collector commands
  host         add a host
  target       add or list a connection profile
  stack        define a stack or add members
  secret       manage credential references
  export       write the whole store to a JSON file
  restore      load a JSON file into an empty store

Run "hearsay <command> -h" for the flags of one command.
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
	case "detect":
		err = cmdDetect(ctx, os.Args[2:])
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
		fmt.Fprintln(os.Stderr, "hearsay:", err)
		os.Exit(1)
	}
}

// dbFlag is repeated rather than made global so each subcommand's -h output
// names it.
func dbFlag(fs *flag.FlagSet) *string {
	return fs.String("db", defaultDB(), "path to the database file")
}

func defaultDB() string {
	if v := os.Getenv("HEARSAY_DB"); v != "" {
		return v
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "hearsay.db"
	}
	return filepath.Join(dir, "hearsay", "hearsay.db")
}

func openStore(path string) (*store.Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
	}
	return store.Open(path)
}

// engineFiles are the optional overrides of what is compiled in.
type engineFiles struct {
	Rules      string
	Collectors string
	Externals  string
	// NoEvidence turns off keeping what each command printed. The repair
	// path for a bad parse rule then requires contacting every host again.
	NoEvidence bool
}

// buildEngine wires the pieces that talk to hosts. Rule and collector files
// are optional overrides of what is compiled in, so a site can add a command
// without rebuilding.
func buildEngine(st *store.Store, f engineFiles, log *slog.Logger) (*engine.Engine, error) {
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
			return nil, err
		}
	}
	specs, err := collect.BuiltinSpecs()
	if err != nil {
		return nil, err
	}
	if f.Collectors != "" {
		b, err := os.ReadFile(f.Collectors)
		if err != nil {
			return nil, err
		}
		if err := specs.Merge(b); err != nil {
			return nil, err
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
		_ = st.Audit(ctx, "hearsay", "credential "+action, "", ref+" "+detail)
		if log != nil && detail != "" {
			log.Debug("credential lookup", "action", action, "ref", ref, "detail", detail)
		}
	})
	eng := &engine.Engine{Store: st, Specs: specs, Norm: norm, Secret: reg, Externals: externals,
		KeepEvidence: !f.NoEvidence, Log: log}
	eng.Wire()
	return eng, nil
}

func cmdServe(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	db := dbFlag(fs)
	addr := fs.String("addr", "127.0.0.1:8099", "address to listen on")
	rules := fs.String("rules", "", "extra normaliser rules file")
	collectors := fs.String("collectors", "", "extra collector spec file")
	externals := fs.String("externals", "", "file listing external collector programs")
	stale := fs.Duration("stale-after", 14*24*time.Hour, "age at which a collected value is called stale, zero to not check")
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
	srv, err := web.New(st, eng, log)
	if err != nil {
		return err
	}
	srv.StaleAfter = *stale
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
	section := fs.String("section-attr", "", "attribute to record the heading a row appeared under")
	fs.Parse(args)

	if *name == "" || fs.NArg() != 1 {
		return errors.New("usage: hearsay import -name <source> [-map map.json] <file.md|file.csv>")
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
	if *section != "" {
		m.SectionAttr = *section
	}
	if m.HostColumn == "" {
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
	fmt.Printf("%d rows: %d hosts created, %d matched, %d claims recorded, %d rows skipped\n",
		res.Rows, res.Created, res.Matched, res.Asserted, res.Skipped)
	for sec, n := range res.BySection {
		// A document with no headings reports one unnamed group, which is
		// just the row count again under a blank label.
		if sec == "" {
			continue
		}
		fmt.Printf("  %-40s %d\n", sec, n)
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
	only := fs.String("only", "", "comma-separated configuration item ids")
	conc := fs.Int("concurrency", 8, "how many hosts at once")
	timeout := fs.Duration("host-timeout", 2*time.Minute, "limit for everything done against one host")
	rules := fs.String("rules", "", "extra normaliser rules file")
	collectors := fs.String("collectors", "", "extra collector spec file")
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

	opt := engine.Options{
		Transport: *transport, Concurrency: *conc,
		HostTimeout: *timeout, Actor: actor(),
	}
	if *only != "" {
		opt.Only = strings.Split(*only, ",")
	}
	progress := make(chan engine.Progress, 32)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for p := range progress {
			fmt.Printf("[%d/%d] %-28s %-10s %s\n", p.Done, p.Total, p.Label, p.Outcome, p.Detail)
		}
	}()
	sum, err := eng.Collect(ctx, opt, progress)
	<-done
	if err != nil {
		return err
	}
	fmt.Printf("run %s: %d targets, %d ok, %d failed\n", sum.ID, sum.Targets, sum.Succeeded, sum.Failed)
	return nil
}

// cmdCheck defines liveness checks and runs them. Defining and running share
// a command because the first thing anyone wants after adding a check is to
// know whether it passes.
func cmdCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	db := dbFlag(fs)
	ci := fs.String("ci", "", "configuration item id")
	name := fs.String("name", "", "a name for this check, unique per host")
	tcp := fs.String("tcp", "", "host:port to connect to")
	httpURL := fs.String("http", "", "URL to request")
	expect := fs.String("expect", "", "status code or pattern such as 2xx, for -http")
	timeout := fs.Duration("timeout", 5*time.Second, "limit for one check")
	del := fs.Bool("delete", false, "remove the named check from -ci")
	list := fs.Bool("list", false, "list the checks on -ci")
	run := fs.Bool("run", false, "run every enabled check and record the results")
	only := fs.String("only", "", "comma-separated configuration item ids, with -run")
	disable := fs.Bool("disable", false, "keep the check but stop running it")
	fs.Parse(args)

	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()

	switch {
	case *run:
		eng, err := buildEngine(st, engineFiles{}, nil)
		if err != nil {
			return err
		}
		results, err := eng.RunChecks(ctx, splitIDs(*only), 16)
		if err != nil {
			return err
		}
		if len(results) == 0 {
			fmt.Println("no checks defined; add one with: hearsay check -ci <id> -name <name> -tcp host:port")
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
			return errors.New("-list needs -ci")
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
			return errors.New("-delete needs -ci and -name")
		}
		return st.DeleteCheck(ctx, *ci, *name)
	}

	if *ci == "" || *name == "" || (*tcp == "" && *httpURL == "") {
		return errors.New("usage: hearsay check -ci <id> -name <name> (-tcp host:port | -http url [-expect 2xx]) | -list -ci <id> | -run")
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
	fmt.Printf("check %s on %s recorded; run it with: hearsay check -run -only %s\n", c.Name, *ci, *ci)
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

func cmdDetect(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("detect", flag.ExitOnError)
	db := dbFlag(fs)
	stale := fs.Duration("stale-after", 14*24*time.Hour, "age at which a value is called stale")
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
	sum, err := eng.Detect(ctx, *stale)
	if err != nil {
		return err
	}
	fmt.Printf("%d conflicts, %d values no rule understood, %d past their freshness budget, %d undocumented\n",
		sum.Conflicts, sum.Unparsed, sum.Stale, sum.Undocumented)
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
	ci := fs.String("ci", "", "single configuration item id to redo")
	collectors := fs.String("collectors", "", "extra collector spec file")
	rules := fs.String("rules", "", "extra normaliser rules file")
	fs.Parse(args)

	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()
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
	ci := fs.String("ci", "", "configuration item id")
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
	f := store.EvidenceFilter{CIID: *ci, SpecID: *spec, Attr: *attr}

	if *forget {
		if f.CIID == "" && f.SpecID == "" && f.Attr == "" {
			return errors.New("refusing to delete every stored output without -ci, -spec or -attr")
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
	retire := fs.String("retire", "", "mark the given configuration item id as gone")
	unretire := fs.String("unretire", "", "undo a retirement")
	reason := fs.String("reason", "", "why, recorded in the audit log")
	fs.Parse(args)

	if *retire != "" || *unretire != "" {
		st, err := openStore(*db)
		if err != nil {
			return err
		}
		defer st.Close()
		if *retire != "" {
			if err := st.Retire(ctx, *retire, actor(), *reason); err != nil {
				return err
			}
			fmt.Printf("retired %s; history kept, collection stopped\n", *retire)
			return nil
		}
		if err := st.Unretire(ctx, *unretire, actor(), *reason); err != nil {
			return err
		}
		fmt.Printf("restored %s; re-enable its connection profile to collect again\n", *unretire)
		return nil
	}

	if *name == "" {
		return errors.New("usage: hearsay host -name <name> [-fqdn <fqdn>] | hearsay host -retire <id> [-reason <why>]")
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
		fmt.Printf("created %s\n", id)
	} else {
		fmt.Printf("already known as %s\n", id)
	}
	return nil
}

func cmdTarget(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("target", flag.ExitOnError)
	db := dbFlag(fs)
	ci := fs.String("ci", "", "configuration item id")
	transport := fs.String("transport", "ssh", "ssh or winrm")
	address := fs.String("address", "", "address or name to connect to")
	port := fs.Int("port", 0, "port, zero for the transport default")
	user := fs.String("user", "", "username")
	cred := fs.String("credential", "", "credential reference, for example agent: or keyring:hearsay/ops")
	bastion := fs.String("bastion", "", "bastion as user@host:port/credential-ref")
	disable := fs.Bool("disable", false, "record the profile but never collect through it")
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
		for _, p := range profiles {
			fmt.Printf("%s %s %s@%s:%d %s\n", p.CIID, p.Transport, p.Username, p.Address, p.Port, p.CredentialRef)
		}
		return nil
	}
	if *address == "" || *user == "" || *cred == "" {
		return errors.New("a target needs -address, -user and -credential")
	}
	p := store.ConnProfile{
		CIID: *ci, Transport: *transport, Address: *address, Port: *port,
		Username: *user, CredentialRef: *cred, BastionRef: *bastion,
		Enabled: !*disable,
	}
	if err := st.PutConnProfile(ctx, p); err != nil {
		return err
	}
	fmt.Println("recorded")
	return nil
}

func cmdStack(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stack", flag.ExitOnError)
	db := dbFlag(fs)
	name := fs.String("name", "", "stack name")
	desc := fs.String("description", "", "what this stack is")
	owner := fs.String("owner", "", "who is responsible for it")
	env := fs.String("environment", "", "environment label")
	add := fs.String("add", "", "comma-separated configuration item ids to add as members")
	role := fs.String("role", "", "role the added members play in this stack")
	fs.Parse(args)

	if *name == "" {
		return errors.New("usage: hearsay stack -name <name> [-add <ci-id,...>]")
	}
	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()

	id, err := st.PutStack(ctx, model.Stack{
		Name: *name, Description: *desc, Owner: *owner, Environment: *env,
	})
	if err != nil {
		return err
	}
	for _, ci := range strings.Split(*add, ",") {
		ci = strings.TrimSpace(ci)
		if ci == "" {
			continue
		}
		if err := st.AddToStack(ctx, id, ci, *role); err != nil {
			return err
		}
	}
	fmt.Printf("stack %s saved\n", *name)
	return nil
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
		return errors.New("usage: hearsay restore -db <new.db> -in <dump.json>")
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
	for _, k := range []string{"HEARSAY_ACTOR", "USER", "USERNAME"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return "unknown"
}
