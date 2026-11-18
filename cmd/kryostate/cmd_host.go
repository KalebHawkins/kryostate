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
	"github.com/KalebHawkins/kryostate/internal/store"
)

type hostListOpts struct {
	query, stack, cols string
	json, colsSet      bool
}

func (a *app) hostCmd() *cobra.Command {
	cmd := group("host", "list, show, add, update, disable, enable and delete hosts", "hosts")
	cmd.AddCommand(a.hostListCmd(), a.hostShowCmd("show"), a.hostAddCmd(), a.hostUpdateCmd("update"),
		a.hostDisableCmd(), a.hostEnableCmd(), a.hostRmCmd())
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

func (a *app) hostDisableCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "disable <host>...",
		Short: "stop contacting hosts; they stay listed and their connections are kept",
		Long: `Disable hosts. A disabled host is never contacted: no collection, scheduled
or by hand, no external collector, no liveness check and no DNS lookup. It
stays in the inventory, its stacks, charts and queries, marked disabled, with
everything recorded about it. Its connections are left exactly as they are,
so "kryostate host enable" brings it back as it was.

A disabled host is not counted as failing or never collected. List them with
kryostate host list -q 'disabled == true'.`,
		Example: "  kryostate host disable web01 web02 --reason 'powered off for CHG-1234'",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.hostSetDisabled(cmd.Context(), args, reason, true)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why, recorded in the audit log and shown on the host")
	return cmd
}

func (a *app) hostEnableCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:     "enable <host>...",
		Short:   "contact disabled hosts again, through the connections they had",
		Example: "  kryostate host enable web01",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.hostSetDisabled(cmd.Context(), args, reason, false)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why, recorded in the audit log")
	return cmd
}

func (a *app) hostSetDisabled(ctx context.Context, refs []string, reason string, disable bool) error {
	st, err := openStore(a.db)
	if err != nil {
		return err
	}
	defer st.Close()
	ids, err := st.LookupCIs(ctx, refs)
	if err != nil {
		return err
	}
	for i, id := range ids {
		var changed bool
		if disable {
			changed, err = st.DisableHost(ctx, id, actor(), reason)
		} else {
			changed, err = st.EnableHost(ctx, id, actor(), reason)
		}
		if err != nil {
			return err
		}
		switch {
		case disable && changed:
			fmt.Printf("disabled %s (%s): it will not be contacted until enabled\n", refs[i], shortID(id))
		case disable:
			fmt.Printf("%s (%s) was already disabled\n", refs[i], shortID(id))
		case changed:
			fmt.Printf("enabled %s (%s): it is collected through its connections again\n", refs[i], shortID(id))
		default:
			fmt.Printf("%s (%s) was not disabled\n", refs[i], shortID(id))
		}
	}
	if disable {
		fmt.Printf("bring back with: kryostate host enable %s\n", strings.Join(refs, " "))
	}
	return nil
}

// enabledOnly drops disabled hosts from a list given with --only, saying so.
// It reports false when hosts were named and every one of them is disabled:
// an empty list means every host, so the caller must then do nothing.
func enabledOnly(ctx context.Context, st *store.Store, refs, ids []string) ([]string, bool, error) {
	if len(ids) == 0 {
		return ids, true, nil
	}
	var kept []string
	for i, id := range ids {
		c, err := st.CI(ctx, id)
		if err != nil {
			return nil, false, err
		}
		if c.DisabledAt != nil {
			fmt.Printf("%s is disabled, so it is not contacted; enable it with: kryostate host enable %s\n", refs[i], refs[i])
			continue
		}
		kept = append(kept, id)
	}
	return kept, len(kept) > 0, nil
}

func (a *app) hostRmCmd() *cobra.Command {
	var (
		yes    bool
		reason string
	)
	cmd := &cobra.Command{
		Use:     "rm <host>...",
		Aliases: []string{"remove", "delete"},
		Short:   "delete hosts and everything recorded about them, permanently",
		Long: `Delete hosts for good, with everything recorded about them: values, command
output, probes, identities, addresses, checks, stack memberships and
connections. The audit log is kept, and one more entry records each deletion
with the host's name and the reason given, so the log still says what was
deleted. This cannot be undone, so it asks first unless --yes is given.

To stop contacting a host but keep it, use "kryostate host disable".`,
		Example: "  kryostate host rm test01 --reason 'built by mistake'\n  kryostate host rm --yes test01 test02",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.hostRm(cmd.Context(), cmd.InOrStdin(), args, yes, reason)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	cmd.Flags().StringVar(&reason, "reason", "", "why, recorded in the audit log")
	return cmd
}

func (a *app) hostRm(ctx context.Context, in io.Reader, refs []string, yes bool, reason string) error {
	st, err := openStore(a.db)
	if err != nil {
		return err
	}
	defer st.Close()
	// Every name is resolved before anything is deleted, so a typo in the
	// third host does not leave the first two done and the rest not.
	ids, err := st.LookupCIs(ctx, refs)
	if err != nil {
		return err
	}
	if !yes {
		var names []string
		for i, id := range ids {
			names = append(names, fmt.Sprintf("%s (%s)", refs[i], shortID(id)))
		}
		fmt.Printf("This permanently deletes %s and everything recorded about %s. The audit log is kept. It cannot be undone.\n",
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
		if _, err := st.DeleteHost(ctx, id, actor(), reason); err != nil {
			return err
		}
		fmt.Printf("deleted %s (%s) and all its history; the audit log keeps a record\n", refs[i], shortID(id))
	}
	return nil
}

func pluralIt(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}
