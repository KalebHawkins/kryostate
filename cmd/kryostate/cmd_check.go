package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/KalebHawkins/kryostate/internal/engine"
	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// checkCmd runs the liveness checks, and defines them. Defining and running
// share a command because the first thing anyone wants after adding a check
// is to know whether it passes.
func (a *app) checkCmd() *cobra.Command {
	var only []string
	cmd := &cobra.Command{
		Use:   "check",
		Short: "run the liveness checks and record the results; add, list and remove checks",
		Long: `Run every enabled liveness check and record the results (the same as
"check run"), or define checks with the subcommands.`,
		Example: `  kryostate check
  kryostate check add web01 https --http https://web01/health --expect 2xx
  kryostate check run --only web01`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.runChecks(cmd.Context(), only) },
	}
	cmd.Flags().StringSliceVarP(&only, "only", "o", nil, "hosts to limit it to (names or ids), comma-separated or repeated")
	cmd.AddCommand(a.checkRunCmd(), a.checkAddCmd(), a.checkListCmd(), a.checkRmCmd())
	return cmd
}

func (a *app) checkRunCmd() *cobra.Command {
	var only []string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "run every enabled check and record the results",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return a.runChecks(cmd.Context(), only) },
	}
	cmd.Flags().StringSliceVarP(&only, "only", "o", nil, "hosts to limit it to (names or ids), comma-separated or repeated")
	return cmd
}

func (a *app) runChecks(ctx context.Context, only []string) error {
	st, err := openStore(a.db)
	if err != nil {
		return err
	}
	defer st.Close()
	eng, err := buildEngine(st, engineFiles{}, nil)
	if err != nil {
		return err
	}
	refs := onlyIDs(only)
	ids, err := st.LookupCIs(ctx, refs)
	if err != nil {
		return err
	}
	ids, anyLeft, err := enabledOnly(ctx, st, refs, ids)
	if err != nil || !anyLeft {
		return err
	}
	results, err := eng.RunChecks(ctx, ids, 16)
	if err != nil {
		return err
	}
	if len(results) == 0 {
		fmt.Println("no checks defined; add one with: kryostate check add <host> <name> --tcp host:port")
		return nil
	}
	tw := newTable(os.Stdout)
	fmt.Fprintln(tw, "HOST\tCHECK\tRESULT\tTOOK\tDETAIL")
	for _, r := range results {
		status := "ok"
		if r.Outcome != model.OutcomeOK {
			status = string(r.Outcome)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%dms\t%s\n",
			r.Check.Label, r.Check.Name, status, r.Took.Milliseconds(), dash(oneLine(r.Detail, 60)))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	ok, failed := engine.CheckSummary(results)
	fmt.Printf("%d ok, %d failed\n", ok, failed)
	return nil
}

func (a *app) checkAddCmd() *cobra.Command {
	var (
		tcp, httpURL, expect string
		timeout              time.Duration
		disable              bool
	)
	cmd := &cobra.Command{
		Use:   "add <host> <name>",
		Short: "define a liveness check on a host, or replace one of the same name",
		Example: `  kryostate check add db01 postgres --tcp db01:5432
  kryostate check add web01 health --http https://web01/health --expect 2xx`,
		Args: cobra.ExactArgs(2),
		RunE: a.withStore(func(ctx context.Context, st *store.Store, args []string) error {
			id, err := st.LookupCI(ctx, args[0])
			if err != nil {
				return err
			}
			c := store.Check{CIID: id, Name: args[1], Expect: expect, Timeout: timeout, Enabled: !disable}
			if tcp != "" {
				c.Kind, c.Target = "tcp", tcp
			} else {
				c.Kind, c.Target = "http", httpURL
			}
			if err := st.PutCheck(ctx, c); err != nil {
				return err
			}
			fmt.Printf("check %s on %s recorded; run it with: kryostate check run --only %s\n", c.Name, args[0], args[0])
			return nil
		}),
	}
	fs := cmd.Flags()
	fs.StringVar(&tcp, "tcp", "", "host:port to connect to")
	fs.StringVar(&httpURL, "http", "", "URL to request")
	fs.StringVar(&expect, "expect", "", "status code or pattern such as 2xx, for --http")
	fs.DurationVar(&timeout, "timeout", 5*time.Second, "limit for one check")
	fs.BoolVar(&disable, "disable", false, "keep the check but stop running it")
	cmd.MarkFlagsOneRequired("tcp", "http")
	cmd.MarkFlagsMutuallyExclusive("tcp", "http")
	return cmd
}

func (a *app) checkListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "list [host]",
		Aliases: []string{"ls"},
		Short:   "list checks with their host and last result: every host's, or one host's",
		Example: "  kryostate check list\n  kryostate check list web01\n  kryostate check list --json",
		Args:    cobra.MaximumNArgs(1),
		RunE: a.withStore(func(ctx context.Context, st *store.Store, args []string) error {
			id := ""
			if len(args) == 1 {
				var err error
				if id, err = st.LookupCI(ctx, args[0]); err != nil {
					return err
				}
			}
			cs, err := st.ListChecks(ctx, id)
			if err != nil {
				return err
			}
			if asJSON {
				type checkJSON struct {
					Host        string     `json:"host"`
					HostID      string     `json:"host_id"`
					Name        string     `json:"name"`
					Kind        string     `json:"kind"`
					Target      string     `json:"target"`
					Expect      string     `json:"expect,omitempty"`
					Enabled     bool       `json:"enabled"`
					HostEnabled bool       `json:"host_enabled"`
					LastOutcome string     `json:"last_outcome,omitempty"`
					LastDetail  string     `json:"last_detail,omitempty"`
					LastAt      *time.Time `json:"last_at,omitempty"`
				}
				out := make([]checkJSON, 0, len(cs))
				for _, c := range cs {
					j := checkJSON{Host: c.Label, HostID: c.CIID, Name: c.Name, Kind: c.Kind, Target: c.Target,
						Expect: c.Expect, Enabled: c.Enabled, HostEnabled: !c.HostDisabled,
						LastOutcome: c.LastOutcome, LastDetail: c.LastDetail}
					if !c.LastAt.IsZero() {
						at := c.LastAt
						j.LastAt = &at
					}
					out = append(out, j)
				}
				return writeJSON(out)
			}
			if len(cs) == 0 {
				fmt.Println("no checks defined; add one with: kryostate check add <host> <name> --tcp host:port")
				return nil
			}
			tw := newTable(os.Stdout)
			fmt.Fprintln(tw, "HOST\tCHECK\tKIND\tTARGET\tEXPECT\tSTATUS\tLAST RESULT\tWHEN")
			for _, c := range cs {
				state := "enabled"
				switch {
				case c.HostDisabled:
					state = "host disabled"
				case !c.Enabled:
					state = "disabled"
				}
				last, when := "never", "-"
				if c.LastOutcome != "" {
					last, when = c.LastOutcome, agoText(c.LastAt)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", c.Label, c.Name, c.Kind,
					safeText(c.Target, false), dash(c.Expect), state, last, when)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Printf("%d check%s.\n", len(cs), pluralS(len(cs)))
			return nil
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func (a *app) checkRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <host> <name>",
		Aliases: []string{"delete"},
		Short:   "remove a check from a host",
		Args:    cobra.ExactArgs(2),
		RunE: a.withStore(func(ctx context.Context, st *store.Store, args []string) error {
			id, err := st.LookupCI(ctx, args[0])
			if err != nil {
				return err
			}
			return st.DeleteCheck(ctx, id, args[1])
		}),
	}
}
