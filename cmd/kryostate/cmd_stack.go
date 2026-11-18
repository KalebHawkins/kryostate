package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KalebHawkins/kryostate/internal/ingest"
	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/query"
	"github.com/KalebHawkins/kryostate/internal/store"
	"github.com/KalebHawkins/kryostate/internal/web"
)

func (a *app) stackCmd() *cobra.Command {
	cmd := group("stack", "define stacks and their members", "stacks")
	cmd.AddCommand(a.stackListCmd(), a.stackAddCmd(), a.stackUpdateCmd(), a.stackRmCmd(), a.stackDeleteCmd())
	return cmd
}

func (a *app) stackListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "list stacks with how their members are doing",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openStore(a.db)
			if err != nil {
				return err
			}
			defer st.Close()
			return listStacks(cmd.Context(), st)
		},
	}
}

// stackMeta are the details of a stack itself.
type stackMeta struct {
	description, owner, environment string
}

func addStackMetaFlags(cmd *cobra.Command, m *stackMeta) {
	fs := cmd.Flags()
	fs.StringVar(&m.description, "description", "", "what this stack is")
	fs.StringVar(&m.owner, "owner", "", "who is responsible for it")
	fs.StringVar(&m.environment, "environment", "", "environment label")
}

// putStack creates the stack or changes the details given, leaving the
// others as they were: a membership change must not wipe the description,
// owner and environment just because those flags were not repeated.
func putStack(ctx context.Context, st *store.Store, cmd *cobra.Command, name string, m stackMeta, mustExist bool) (int64, error) {
	views, err := st.Stacks(ctx)
	if err != nil {
		return 0, err
	}
	stk := model.Stack{Name: name}
	found := false
	for _, v := range views {
		if v.Stack.Name == name {
			stk, found = v.Stack, true
		}
	}
	if mustExist && !found {
		return 0, fmt.Errorf("no stack named %s; create it with: kryostate stack add %s", name, name)
	}
	changed := cmd.Flags().Changed
	if changed("description") {
		stk.Description = m.description
	}
	if changed("owner") {
		stk.Owner = m.owner
	}
	if changed("environment") {
		stk.Environment = m.environment
	}
	return st.PutStack(ctx, stk)
}

type stackAddOpts struct {
	meta                                 stackMeta
	file, user, credential, role, app, q string
	create                               bool
}

func (a *app) stackAddCmd() *cobra.Command {
	var o stackAddOpts
	cmd := &cobra.Command{
		Use:   "add <stack> [host[:role]...]",
		Short: "create a stack, or add hosts to one",
		Long: `Create a stack, or add hosts to one. Hosts are named after the stack, each as
name or name:role; --file reads more, one per line (name, then role,
optionally an fqdn), and --query adds every recorded host matching a query,
once: the stack does not follow the query afterwards.`,
		Example: `  kryostate stack add payments web01:app web02:app db01:db --owner 'Team Pay'
  kryostate stack add payments --file hosts.txt --create --user ops
  kryostate stack add linux -q 'os.family == "Linux"' --role server`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.stackAdd(cmd, args[0], args[1:], o)
		},
	}
	addStackMetaFlags(cmd, &o.meta)
	fs := cmd.Flags()
	fs.StringVar(&o.file, "file", "", "file listing hosts to add, one per line: name, then role, optionally an fqdn; - reads standard input")
	fs.BoolVar(&o.create, "create", false, "record hosts that are not known yet")
	fs.StringVarP(&o.user, "user", "u", "", "give added hosts with no connection one that logs in as this user, with --credential")
	fs.StringVarP(&o.credential, "credential", "c", "agent:", "with --user: the credential those connections use, such as keyring:kryostate/<user>")
	fs.StringVar(&o.role, "role", "", "role for added members that do not name their own")
	fs.StringVar(&o.app, "application", "", "the application the added hosts run")
	fs.StringVarP(&o.q, "query", "q", "", `add every recorded host matching a query, such as 'os.family == "Linux"', in the --role role; once, the stack does not follow the query afterwards (the query language of host list -q)`)
	return cmd
}

func (a *app) stackAdd(cmd *cobra.Command, name string, hosts []string, o stackAddOpts) error {
	ctx := cmd.Context()
	st, err := openStore(a.db)
	if err != nil {
		return err
	}
	defer st.Close()
	if _, err := putStack(ctx, st, cmd, name, o.meta, false); err != nil {
		return err
	}

	// Hosts to add come from the arguments, as name or name:role, and from
	// --file, one per line in the same forms the web page accepts.
	var lines []ingest.HostLine
	var problems []string
	for _, item := range hosts {
		for _, h := range splitIDs(item) {
			ls, ps := ingest.ParseHostList(h)
			lines, problems = append(lines, ls...), append(problems, ps...)
		}
	}
	if o.file != "" {
		var b []byte
		if o.file == "-" {
			b, err = io.ReadAll(os.Stdin)
		} else {
			b, err = os.ReadFile(o.file)
		}
		if err != nil {
			return err
		}
		ls, ps := ingest.ParseHostList(string(b))
		lines, problems = append(lines, ls...), append(problems, ps...)
	}
	for i := range lines {
		if lines[i].Role == "" {
			lines[i].Role = o.role
		}
	}
	if len(lines) > 0 {
		res, err := ingest.AddHostsToStack(ctx, st, name, lines,
			ingest.BulkOptions{Create: o.create, User: o.user, Credential: o.credential, Application: o.app, By: actor()})
		if err != nil {
			return err
		}
		res.Problems = append(problems, res.Problems...)
		fmt.Println(res.Summary(name))
		if len(res.Problems) > 0 && !o.create {
			fmt.Println("To add hosts that are not recorded yet, repeat with --create.")
		}
	}

	if o.q != "" {
		res, err := web.AddMatchingToStack(ctx, st, name, o.q, o.role)
		var qe *query.Error
		if errors.As(err, &qe) {
			return fmt.Errorf("query: %s\n  %s\n  %s", qe.Msg, o.q, qe.Caret())
		} else if err != nil {
			return err
		}
		fmt.Println(res.Summary(name))
	}
	return printStackSaved(ctx, st, name)
}

func printStackSaved(ctx context.Context, st *store.Store, name string) error {
	members, err := st.StackMembers(ctx, name)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(members))
	for _, m := range members {
		names = append(names, m.CI.Label)
	}
	fmt.Printf("stack %s saved, members (%d): %s\n", name, len(members), strings.Join(names, ", "))
	return nil
}

func (a *app) stackUpdateCmd() *cobra.Command {
	var m stackMeta
	cmd := &cobra.Command{
		Use:     "update <stack>",
		Short:   "change a stack's description, owner or environment",
		Example: "  kryostate stack update payments --owner 'Team Pay' --environment prod",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := openStore(a.db)
			if err != nil {
				return err
			}
			defer st.Close()
			if _, err := putStack(ctx, st, cmd, args[0], m, true); err != nil {
				return err
			}
			return printStackSaved(ctx, st, args[0])
		},
	}
	addStackMetaFlags(cmd, &m)
	return cmd
}

func (a *app) stackRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <stack> <host>...",
		Aliases: []string{"remove"},
		Short:   "take hosts out of a stack",
		Example: "  kryostate stack rm payments web02 web03",
		Args:    cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := openStore(a.db)
			if err != nil {
				return err
			}
			defer st.Close()
			id, err := st.StackID(ctx, args[0])
			if err != nil {
				return err
			}
			var refs []string
			for _, h := range args[1:] {
				refs = append(refs, splitIDs(h)...)
			}
			ids, err := st.LookupCIs(ctx, refs)
			if err != nil {
				return err
			}
			for i, ci := range ids {
				ok, err := st.RemoveFromStack(ctx, id, ci)
				if err != nil {
					return err
				}
				if !ok {
					fmt.Printf("%s was not a member\n", refs[i])
				}
			}
			return printStackSaved(ctx, st, args[0])
		},
	}
}

func (a *app) stackDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <stack>",
		Short: "delete a stack; its hosts, and what is recorded about them, are untouched",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := openStore(a.db)
			if err != nil {
				return err
			}
			defer st.Close()
			if err := st.DeleteStack(ctx, args[0]); err != nil {
				return err
			}
			if err := st.Audit(ctx, actor(), "delete stack", "", args[0]); err != nil {
				return err
			}
			fmt.Printf("stack %s deleted; its hosts are untouched\n", args[0])
			return nil
		},
	}
}

func listStacks(ctx context.Context, st *store.Store) error {
	views, err := st.Stacks(ctx)
	if err != nil {
		return err
	}
	if len(views) == 0 {
		fmt.Println("no stacks; define one with: kryostate stack add <stack> <host>...")
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
