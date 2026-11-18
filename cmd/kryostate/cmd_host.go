package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KalebHawkins/kryostate/internal/model"
)

type hostListOpts struct {
	query, stack, cols string
	json, colsSet      bool
}

func (a *app) hostCmd() *cobra.Command {
	cmd := group("host", "list, show, add, update and remove hosts", "hosts")
	cmd.AddCommand(a.hostListCmd(), a.hostShowCmd("show"), a.hostAddCmd(), a.hostUpdateCmd("update"),
		a.hostRmCmd(), a.hostRestoreCmd())
	return cmd
}

func (a *app) hostListCmd() *cobra.Command {
	var o hostListOpts
	cmd := &cobra.Command{
		Use:     "list [query...]",
		Aliases: []string{"ls"},
		Short:   "list hosts, filtered by a query, with chosen attributes as columns",
		Example: `  kryostate host list
  kryostate host list -q 'os.family == "Linux" && stack == "web"' --cols os.version,cpu.cores
  kryostate host ls web --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && o.query == "" {
				o.query = strings.Join(args, " ")
			}
			o.colsSet = cmd.Flags().Changed("cols")
			return a.hostList(cmd.Context(), o)
		},
	}
	fs := cmd.Flags()
	fs.StringVarP(&o.query, "query", "q", "", `a query, such as 'os.family == "Linux" && stack == "web"', or part of a name (see the user guide)`)
	fs.StringVar(&o.stack, "stack", "", "only members of this stack")
	fs.StringVar(&o.cols, "cols", "os.version,application", "comma-separated attributes to show as columns; with --json, the values to include (default all)")
	fs.BoolVar(&o.json, "json", false, "print JSON instead of a table, in the shape /hosts.json uses")
	return cmd
}

// hostShowCmd is "host show", and the top-level "show" kept as a shortcut.
func (a *app) hostShowCmd(use string) *cobra.Command {
	var (
		asJSON bool
		at     string
	)
	cmd := &cobra.Command{
		Use:     use + " <host>",
		Short:   "everything known about one host",
		Example: "  kryostate host show web01\n  kryostate host show web01 --at 2026-03-01 --json",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.hostShow(cmd.Context(), args[0], asJSON, at)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON instead of text")
	cmd.Flags().StringVar(&at, "at", "", "show each attribute's value as it was at `TIME` (2026-03-01 for the end of that day, or 2026-03-01T15:04)")
	return cmd
}

func (a *app) hostAddCmd() *cobra.Command {
	var fqdn string
	cmd := &cobra.Command{
		Use:     "add <name>",
		Short:   "add a host",
		Example: "  kryostate host add web01 --fqdn web01.example.com",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.hostAdd(cmd.Context(), args[0], fqdn)
		},
	}
	cmd.Flags().StringVar(&fqdn, "fqdn", "", "fully qualified name, stronger identity evidence than a bare name")
	return cmd
}

func (a *app) hostAdd(ctx context.Context, name, fqdn string) error {
	st, err := openStore(a.db)
	if err != nil {
		return err
	}
	defer st.Close()
	ev := []model.Identity{{Scheme: model.IdentHostname, Value: name}}
	if fqdn != "" {
		ev = append([]model.Identity{{Scheme: model.IdentFQDN, Value: fqdn}}, ev...)
	}
	id, created, err := st.ResolveCI(ctx, model.KindHost, name, ev)
	if err != nil {
		return err
	}
	if created {
		fmt.Printf("created %s (id %s)\n", name, id)
		fmt.Printf("next: kryostate target add %s --user <user>\n", name)
	} else {
		fmt.Printf("already known as %s\n", id)
	}
	return nil
}

// hostUpdateCmd is "host update", and the top-level "set" kept as a shortcut.
func (a *app) hostUpdateCmd(use string) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   use + " <host> [application=NAME] [stack=NAME] [role=ROLE]",
		Short: "state a host's application, stack or role yourself",
		Long: `State a host's application, stack or role yourself.

Only application, stack and role can be set by hand; everything else is what
the host reports. A role alone goes to the one stack the host is in.`,
		Example: "  kryostate host update web01 application=ledger stack=pay role=app --reason CHG-1234",
		Args:    cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.hostUpdate(cmd.Context(), args, reason)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "optional note for the audit log")
	return cmd
}

func (a *app) hostRmCmd() *cobra.Command {
	var (
		purge, yes bool
		reason     string
	)
	cmd := &cobra.Command{
		Use:     "rm <host>...",
		Aliases: []string{"remove", "retire"},
		Short:   "take hosts out of service, or with --purge delete them and their history",
		Long: `Take hosts out of service. The host is retired and removed from everything
that would act on it: its connections (and their pinned host keys), its stack
memberships, its liveness checks and its collection back-off. What was
recorded about it is kept, so the history still explains itself, and
"kryostate host restore" brings it back.

With --purge the host is deleted for good, with everything recorded about it:
values, command output, probes, identities, addresses, checks, memberships,
connections and its audit entries. One audit entry records the purge. This
cannot be undone, so it asks first unless --yes is given.`,
		Example: "  kryostate host rm pmap13was1 --reason 'decommissioned, CHG-1234'\n  kryostate host rm --purge --yes test01 test02",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.hostRm(cmd.Context(), cmd.InOrStdin(), args, purge, yes, reason)
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "delete the hosts and all their history, permanently")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "with --purge: do not ask for confirmation")
	cmd.Flags().StringVar(&reason, "reason", "", "why, recorded in the audit log")
	return cmd
}

func (a *app) hostRm(ctx context.Context, in io.Reader, refs []string, purge, yes bool, reason string) error {
	st, err := openStore(a.db)
	if err != nil {
		return err
	}
	defer st.Close()
	// Every name is resolved before anything is removed, so a typo in the
	// third host does not leave the first two half done.
	ids, err := st.LookupCIs(ctx, refs)
	if err != nil {
		return err
	}
	if purge && !yes {
		var names []string
		for i, id := range ids {
			names = append(names, fmt.Sprintf("%s (%s)", refs[i], shortID(id)))
		}
		fmt.Printf("This permanently deletes %s and everything recorded about %s. It cannot be undone.\n",
			strings.Join(names, ", "), pluralIt(len(ids)))
		fmt.Print("Type yes to continue: ")
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if a := strings.ToLower(strings.TrimSpace(line)); a != "yes" && a != "y" {
			return errors.New("not confirmed; nothing deleted (give --yes to skip the question)")
		}
	}
	for i, id := range ids {
		if purge {
			if err := st.PurgeCI(ctx, id, actor(), reason); err != nil {
				return err
			}
			fmt.Printf("purged %s (%s) and all its history\n", refs[i], shortID(id))
			continue
		}
		r, err := st.RemoveHost(ctx, id, actor(), reason)
		if err != nil {
			return err
		}
		state := "retired"
		if r.AlreadyRetired {
			state = "already retired"
		}
		fmt.Printf("removed %s (%s): %s; %s; history kept\n", refs[i], shortID(id), state, r)
	}
	if !purge {
		fmt.Println("bring one back with: kryostate host restore <host>")
	}
	return nil
}

func pluralIt(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

func (a *app) hostRestoreCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:     "restore <host>",
		Aliases: []string{"unretire"},
		Short:   "bring back a host taken out of service with host rm",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := openStore(a.db)
			if err != nil {
				return err
			}
			defer st.Close()
			id, err := st.LookupCI(ctx, args[0])
			if err != nil {
				return err
			}
			if err := st.Unretire(ctx, id, actor(), reason); err != nil {
				return err
			}
			fmt.Printf("restored %s (%s); add a connection to collect again: kryostate target add %s --user <user>\n",
				args[0], shortID(id), args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why, recorded in the audit log")
	return cmd
}
