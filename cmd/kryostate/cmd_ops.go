package main

// The operational commands: the server, collection and the repairs run over
// what has been stored, and moving a whole store in and out.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/KalebHawkins/kryostate/internal/engine"
	"github.com/KalebHawkins/kryostate/internal/store"
	"github.com/KalebHawkins/kryostate/internal/transfer"
	"github.com/KalebHawkins/kryostate/internal/web"
)

const collectorsFileUsage = "collector spec file to use instead of " + defaultCollectorsFile + " beside the database"

// addEngineFlags registers the overrides of what is compiled in, for the
// commands that collect.
func addEngineFlags(fs *pflag.FlagSet, f *engineFiles) {
	fs.StringVar(&f.Rules, "rules", "", "extra normaliser rules file")
	fs.StringVar(&f.Collectors, "collectors", "", collectorsFileUsage)
	fs.StringVar(&f.Externals, "externals", "", "file listing external collector programs")
	fs.BoolVar(&f.NoEvidence, "no-evidence", false, "do not keep what each collector command printed")
}

// collectFlags are what collect and refresh share.
type collectFlags struct {
	only        []string
	concurrency int
	hostTimeout time.Duration
	files       engineFiles
	lease       leaseFlags
}

func addCollectFlags(fs *pflag.FlagSet, c *collectFlags) {
	fs.StringSliceVarP(&c.only, "only", "o", nil, "hosts to limit it to (names or ids), comma-separated or repeated")
	fs.IntVar(&c.concurrency, "concurrency", 8, "how many hosts at once")
	fs.DurationVar(&c.hostTimeout, "host-timeout", 2*time.Minute, "limit for everything done against one host")
	addEngineFlags(fs, &c.files)
	c.lease = addLeaseFlags(fs)
}

func (a *app) serveCmd() *cobra.Command {
	var (
		addr, trusted            string
		files                    engineFiles
		stale                    time.Duration
		allowContent, noSchedule bool
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "run the web interface, and the collection schedules",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.serve(cmd.Context(), addr, trusted, files, stale, allowContent, noSchedule)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&addr, "addr", "127.0.0.1:8099", "address to listen on (env KRYOSTATE_ADDR)")
	addEngineFlags(fs, &files)
	fs.DurationVar(&stale, "stale-after", 14*24*time.Hour, "when the built-in collectors run only by hand: how long after its last good collection the overview calls a host stale (otherwise twice their shortest schedule)")
	fs.StringVar(&trusted, "trusted-origin", "", "comma-separated origins, such as https://cmdb.example.com, allowed to submit forms when a proxy serves this under another name")
	fs.BoolVar(&allowContent, "allow-content-collectors", false, "let the web interface switch on catalog collectors that read the contents of files and variables")
	fs.BoolVar(&noSchedule, "no-schedule", false, "do not run collectors on their schedules; collect only when somebody asks")
	return cmd
}

func (a *app) serve(ctx context.Context, addr, trusted string, files engineFiles, stale time.Duration, allowContent, noSchedule bool) error {
	log := a.log
	st, err := openStore(a.db)
	if err != nil {
		return err
	}
	defer st.Close()
	// A broken collectors.json does not stop the server: the collectors page
	// says what is wrong with it, which is where someone fixing it will look.
	// Every other command still refuses to run with it.
	files = files.resolved()
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
	srv.StaleAfter = stale
	srv.TrustedOrigins = splitIDs(trusted)
	srv.AllowRevealing = allowContent
	// On a loopback address nothing but this machine should reach the
	// server, so a request addressed to any other name is refused. Behind a
	// proxy, on a real address, the proxy decides which names are served.
	if h, _, err := net.SplitHostPort(addr); err == nil && web.IsLoopbackHost(h) {
		srv.LoopbackOnly = true
	}
	// The scheduler stops with the server, and the store is not closed
	// until a run in flight has written its results.
	if !noSchedule && srv.Scheduler != nil {
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
		Addr:              addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sd, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.Shutdown(sd)
	}()
	log.Info("listening", "addr", addr, "db", a.db)
	if err := h.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (a *app) collectCmd() *cobra.Command {
	var (
		c         collectFlags
		transport string
	)
	cmd := &cobra.Command{
		Use:   "collect",
		Short: "connect to every enabled target and record what it reports",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.collect(cmd.Context(), c, transport)
		},
	}
	cmd.Flags().StringVar(&transport, "transport", "", "limit to one transport (ssh or winrm)")
	addCollectFlags(cmd.Flags(), &c)
	return cmd
}

func (a *app) collect(ctx context.Context, c collectFlags, transport string) error {
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
	release, err := takeLease(ctx, st, "collect", c.lease)
	if err != nil {
		return err
	}
	defer release()
	opt := engine.Options{
		Transport: transport, Concurrency: c.concurrency, Only: ids,
		HostTimeout: c.hostTimeout, Actor: actor(),
	}
	sum, err := runCollect(ctx, eng, opt, false)
	if err != nil {
		return err
	}
	fmt.Printf("run %s: %d targets, %d ok, %d failed\n", sum.ID, sum.Targets, sum.Succeeded, sum.Failed)
	if sum.Targets == 0 {
		fmt.Println("no enabled targets; add one with: kryostate target add <host> --user <user>")
	}
	return nil
}

func (a *app) refreshCmd() *cobra.Command {
	var (
		c                        collectFlags
		resolve, noChecks, quiet bool
	)
	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "collect and run checks, in one go",
		Long: `Collect and run the liveness checks, in one go: what the web interface's
collect button does, plus the checks. With --resolve, every host is looked up
in DNS first.`,
		Example: "  kryostate refresh\n  kryostate refresh --only web01,web02 --quiet",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.refresh(cmd.Context(), c, resolve, noChecks, quiet)
		},
	}
	fs := cmd.Flags()
	addCollectFlags(fs, &c)
	fs.BoolVar(&resolve, "resolve", false, "look every host up in DNS first")
	fs.BoolVar(&noChecks, "no-checks", false, "skip the liveness checks")
	fs.BoolVarP(&quiet, "quiet", "q", false, "print only the summary lines, not one line per host")
	return cmd
}

func (a *app) resolveCmd() *cobra.Command {
	var suffixes, server string
	cmd := &cobra.Command{
		Use:   "resolve",
		Short: "look names up in DNS and record the answers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			st, err := openStore(a.db)
			if err != nil {
				return err
			}
			defer st.Close()
			eng, err := buildEngine(st, engineFiles{}, nil)
			if err != nil {
				return err
			}
			opt := engine.ResolveOptions{Resolver: server}
			if suffixes != "" {
				opt.Suffixes = strings.Split(suffixes, ",")
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
		},
	}
	cmd.Flags().StringVar(&suffixes, "suffixes", "", "comma-separated DNS suffixes to try, in order")
	cmd.Flags().StringVar(&server, "server", "", "resolver address, blank for the system resolver")
	return cmd
}

func (a *app) renormalizeCmd() *cobra.Command {
	var attr, rules string
	cmd := &cobra.Command{
		Use:   "renormalize",
		Short: "re-run normaliser rules over values already collected",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openStore(a.db)
			if err != nil {
				return err
			}
			defer st.Close()
			eng, err := buildEngine(st, engineFiles{Rules: rules}, nil)
			if err != nil {
				return err
			}
			matched, unmatched, err := eng.Renormalize(cmd.Context(), attr)
			if err != nil {
				return err
			}
			fmt.Printf("%d values normalised, %d still unmatched\n", matched, unmatched)
			return nil
		},
	}
	cmd.Flags().StringVar(&attr, "attr", "", "SQL LIKE pattern for the attributes to redo, blank for all")
	cmd.Flags().StringVar(&rules, "rules", "", "extra normaliser rules file")
	return cmd
}

// reparseCmd re-derives values from stored command output. It is the repair
// path for a collector rule that took the wrong substring, as distinct from
// renormalize, which repairs a rule that read the substring wrongly.
func (a *app) reparseCmd() *cobra.Command {
	var attr, spec, host string
	var files engineFiles
	cmd := &cobra.Command{
		Use:   "reparse",
		Short: "re-run collector parse rules over stored command output",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			st, err := openStore(a.db)
			if err != nil {
				return err
			}
			defer st.Close()
			ci := ""
			if host != "" {
				if ci, err = st.LookupCI(ctx, host); err != nil {
					return err
				}
			}
			eng, err := buildEngine(st, files, nil)
			if err != nil {
				return err
			}
			sum, err := eng.Reparse(ctx, store.EvidenceFilter{CIID: ci, SpecID: spec, Attr: attr})
			if err != nil {
				return err
			}
			if sum.Stored == 0 {
				fmt.Println("no stored command output matched; collect with evidence enabled first")
				return nil
			}
			if sum.Considered == 0 {
				fmt.Printf("%d stored outputs matched, but none names a collector this build knows; "+
					"pass the spec file with --collectors\n", sum.Stored)
				return nil
			}
			fmt.Printf("%d stored outputs read: %d values changed, %d recovered from nothing, %d still unparsed\n",
				sum.Considered, sum.Changed, sum.Recovered, sum.Failed)
			if sum.Skipped > 0 {
				fmt.Printf("%d skipped: the stored output was cut short at the size limit, so a value read from it\n"+
					"could be a prefix of the real one. Collect from those hosts again to repair them.\n", sum.Skipped)
			}
			return nil
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&attr, "attr", "", "SQL LIKE pattern for the attributes to redo, blank for all")
	fs.StringVar(&spec, "spec", "", "single collector id to redo")
	fs.StringVar(&host, "host", "", "single host to redo (name, fqdn or id)")
	fs.StringVar(&files.Collectors, "collectors", "", collectorsFileUsage)
	fs.StringVar(&files.Rules, "rules", "", "extra normaliser rules file")
	return cmd
}

func (a *app) exportCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "write the whole store to a JSON file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openStore(a.db)
			if err != nil {
				return err
			}
			defer st.Close()
			d, err := transfer.Export(cmd.Context(), st)
			if err != nil {
				return err
			}
			w := os.Stdout
			if out != "" {
				f, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
				if err != nil {
					return err
				}
				defer f.Close()
				w = f
			}
			return d.Write(w)
		},
	}
	cmd.Flags().StringVarP(&out, "out", "o", "", "file to write, blank for standard output")
	return cmd
}

func (a *app) restoreCmd() *cobra.Command {
	var in string
	cmd := &cobra.Command{
		Use:     "restore",
		Short:   "load a JSON file into an empty store",
		Example: "  kryostate restore --db new.db --in dump.json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := os.Open(in)
			if err != nil {
				return err
			}
			defer f.Close()
			d, err := transfer.Read(f)
			if err != nil {
				return err
			}
			st, err := openStore(a.db)
			if err != nil {
				return err
			}
			defer st.Close()
			if err := transfer.Restore(cmd.Context(), st, d); err != nil {
				return err
			}
			for _, t := range d.Order {
				fmt.Printf("%-18s %d\n", t, d.Counts[t])
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&in, "in", "i", "", "file to read (required)")
	_ = cmd.MarkFlagRequired("in")
	return cmd
}
