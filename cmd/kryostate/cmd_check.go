package main

import (
	"context"
	"fmt"
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
	ids, err := st.LookupCIs(ctx, onlyIDs(only))
	if err != nil {
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
	return &cobra.Command{
		Use:     "list <host>",
		Aliases: []string{"ls"},
		Short:   "list the checks on a host",
		Args:    cobra.ExactArgs(1),
		RunE: a.withStore(func(ctx context.Context, st *store.Store, args []string) error {
			id, err := st.LookupCI(ctx, args[0])
			if err != nil {
				return err
			}
			cs, err := st.Checks(ctx, id)
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
		}),
	}
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
