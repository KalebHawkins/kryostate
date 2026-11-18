package main

import (
	"os"

	"github.com/spf13/cobra"
)

// rulesCmd is "kryostate rules": rules track a rollout, and the counts come
// from what hosts report.
func (a *app) rulesCmd() *cobra.Command {
	cmd := group("rules", "track a rollout: which hosts in scope match, which do not yet", "rule")
	run := func(o *rulesOpts, set func(args []string)) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			set(args)
			st, err := openStore(a.db)
			if err != nil {
				return err
			}
			defer st.Close()
			return runRules(cmd.Context(), st, os.Stdout, *o)
		}
	}

	var lo rulesOpts
	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Args: cobra.NoArgs,
		Short: "list the rules with how far along each is",
		RunE:  run(&lo, func([]string) {}),
	}
	list.Flags().BoolVar(&lo.json, "json", false, "write JSON instead of a table")

	var ao rulesOpts
	add := &cobra.Command{
		Use:   "add <name>",
		Short: "create a rule: the hosts its scope chooses should match its expect query",
		Example: `  kryostate rules add 'nginx 1.24' --scope 'stack == "web"' --expect 'nginx.version >= "1.24"' \
      --description 'CVE-2026-1234, by June' --owner web-team`,
		Args: cobra.ExactArgs(1),
		RunE: run(&ao, func(args []string) { ao.add, ao.name = true, args[0] }),
	}
	af := add.Flags()
	af.StringVar(&ao.scope, "scope", "", "a query choosing the hosts it covers, such as 'stack == \"web\"'; blank for every host")
	af.StringVar(&ao.expect, "expect", "", "a query each host in scope should match once done, such as 'nginx.version >= \"1.24\"' (required)")
	af.StringVar(&ao.description, "description", "", "why, a ticket or a deadline")
	af.StringVar(&ao.owner, "owner", "", "who owns the rollout")
	_ = add.MarkFlagRequired("expect")

	var so rulesOpts
	show := &cobra.Command{
		Use:   "show <name>",
		Short: "list the hosts a rule is waiting on: not yet, and unknown with what is missing",
		Args:  cobra.ExactArgs(1),
		RunE:  run(&so, func(args []string) { so.show = args[0] }),
	}
	show.Flags().BoolVar(&so.json, "json", false, "write JSON instead of text, with every host in scope")

	var do rulesOpts
	rm := &cobra.Command{
		Use: "rm <name>", Aliases: []string{"delete"}, Args: cobra.ExactArgs(1),
		Short: "delete a rule and its history; no host is changed",
		RunE:  run(&do, func(args []string) { do.del = args[0] }),
	}

	cmd.AddCommand(list, add, show, rm)
	return cmd
}
