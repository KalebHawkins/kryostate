package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/secret"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// targetOpts are the flags of target add and target update.
type targetOpts struct {
	transport, address, user, credential, bastion string
	port                                          int
	password, disable                             bool
}

func (a *app) targetCmd() *cobra.Command {
	cmd := group("target", "add, change, list and remove the connections hosts are collected through", "targets")
	cmd.AddCommand(a.targetListCmd(), a.targetAddCmd(), a.targetUpdateCmd(), a.targetRmCmd(),
		a.targetForgetCmd(), a.targetSetCredentialCmd())
	return cmd
}

func (a *app) targetListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "list connections, and whether each is used or its host is disabled",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openStore(a.db)
			if err != nil {
				return err
			}
			defer st.Close()
			return listTargets(cmd.Context(), st)
		},
	}
}

func addTargetFlags(cmd *cobra.Command, o *targetOpts) {
	fs := cmd.Flags()
	fs.StringVarP(&o.transport, "transport", "t", "ssh", "ssh or winrm")
	fs.StringVarP(&o.address, "address", "a", "", "address or name to connect to, blank for the host's fqdn or name")
	fs.IntVarP(&o.port, "port", "p", 0, "port, zero for the transport default")
	fs.StringVarP(&o.user, "user", "u", "", "username; blank on add: the user named by a keyring:kryostate/<user> credential, or the one user your other targets log in as")
	fs.StringVarP(&o.credential, "credential", "c", "", "credential reference, such as agent: or keyring:kryostate/ops; blank on add: what your other targets for the user use, else a password stored for the user, else agent:")
	fs.BoolVar(&o.password, "password", false, "log in with a password: prompt for it, keep it in the OS credential store as kryostate/<user>, and use keyring:kryostate/<user>. Run again when the password changes")
	fs.StringVar(&o.bastion, "bastion", "", "bastion as user@host:port/credential-ref")
	fs.BoolVar(&o.disable, "disable", false, "record the profile but never collect through it")
	cmd.MarkFlagsMutuallyExclusive("credential", "password")
}

func (a *app) targetAddCmd() *cobra.Command {
	var o targetOpts
	cmd := &cobra.Command{
		Use:   "add <host>...",
		Short: "record how to reach hosts: transport, address, user and credential",
		Long: `Record how to reach one or more hosts. Each host is connected to by its own
fqdn or name unless --address is given, which names one host. Adding a
connection a host already has over the same transport replaces it; a pinned
host key is kept.`,
		Example: `  kryostate target add web01 --user ops
  kryostate target add web02                      # same user and credential as your other targets
  kryostate target add web01 web02 db01 --user ops --password
  kryostate target add win01 -t winrm --user 'CORP\svc' -c keyring:kryostate/svc`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.targetPut(cmd, args, o, false)
		},
	}
	addTargetFlags(cmd, &o)
	return cmd
}

func (a *app) targetUpdateCmd() *cobra.Command {
	var o targetOpts
	cmd := &cobra.Command{
		Use:   "update <host>...",
		Short: "change hosts' existing connections; only the flags given change",
		Long: `Change the connection hosts already have over --transport (ssh unless
given). Only the flags given change; everything else, the pinned host key
included, stays as it was.`,
		Example: "  kryostate target update web01 --port 2222\n  kryostate target update web01 web02 --password --user ops",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.targetPut(cmd, args, o, true)
		},
	}
	addTargetFlags(cmd, &o)
	return cmd
}

// targetPut records a connection for each host. With update, the existing
// profile is the starting point and only the flags given change it.
func (a *app) targetPut(cmd *cobra.Command, refs []string, o targetOpts, update bool) error {
	ctx := cmd.Context()
	changed := cmd.Flags().Changed
	if o.address != "" && len(refs) > 1 {
		return errors.New("--address names one host; leave it out to connect to each host by its own name")
	}
	st, err := openStore(a.db)
	if err != nil {
		return err
	}
	defer st.Close()
	ids, err := st.LookupCIs(ctx, refs)
	if err != nil {
		return err
	}
	profiles := make([]store.ConnProfile, len(ids))
	for i, id := range ids {
		if !update {
			continue
		}
		existing, err := st.ConnProfiles(ctx, id)
		if err != nil {
			return err
		}
		found := false
		for _, p := range existing {
			if p.Transport == o.transport {
				profiles[i], found = p, true
			}
		}
		if !found {
			return fmt.Errorf("%s has no %s connection; add one with: kryostate target add %s --user <user>", refs[i], o.transport, refs[i])
		}
	}
	if !update {
		if err := fillTargetDefaults(ctx, st, &o, changed("credential")); err != nil {
			return err
		}
	}
	if o.password {
		user := o.user
		if user == "" && update {
			user = profiles[0].Username
			for _, p := range profiles {
				if p.Username != user {
					return errors.New("these connections log in as different users; name the one the password is for with --user")
				}
			}
		}
		if o.credential, err = storePassword(ctx, st, "kryostate", user); err != nil {
			return err
		}
	}
	if o.credential == "" && (!update || changed("credential")) {
		return errors.New("--credential cannot be empty")
	}
	for i, id := range ids {
		p := profiles[i]
		if !update {
			p = store.ConnProfile{CIID: id, Transport: o.transport, Port: o.port, Username: o.user,
				CredentialRef: o.credential, BastionRef: o.bastion, Enabled: !o.disable}
		} else {
			if changed("port") {
				p.Port = o.port
			}
			if changed("user") {
				p.Username = o.user
			}
			if changed("credential") || o.password {
				p.CredentialRef = o.credential
			}
			if changed("bastion") {
				p.BastionRef = o.bastion
			}
			if changed("disable") {
				p.Enabled = !o.disable
			}
		}
		// The pin is left alone by PutConnProfile when no key is given.
		p.HostKey, p.BastionHostKey = "", ""
		switch {
		case o.address != "":
			p.Address = o.address
		case p.Address == "":
			p.Address = defaultAddress(ctx, st, id)
		}
		if p.Address == "" {
			return fmt.Errorf("%s has no name to connect to: give --address", refs[i])
		}
		if p.Username == "" {
			return errors.New("a target needs --user")
		}
		if err := st.PutConnProfile(ctx, p); err != nil {
			return err
		}
		fmt.Printf("recorded: %s via %s as %s@%s using %s\n", refs[i], p.Transport, p.Username, p.Address, p.CredentialRef)
	}
	fmt.Printf("try it with: kryostate refresh --only %s\n", strings.Join(refs, ","))
	return nil
}

// fillTargetDefaults works out a new target's user and credential when they
// are not given, from what the other targets already use, so that adding one
// more host like the others needs only its name. Whatever it picks is said.
func fillTargetDefaults(ctx context.Context, st *store.Store, o *targetOpts, credGiven bool) error {
	uses, err := st.CredentialUses(ctx, o.transport)
	if err != nil {
		return err
	}
	if o.user == "" {
		if acct, ok := strings.CutPrefix(o.credential, "keyring:kryostate/"); ok && acct != "" {
			o.user = acct
			fmt.Printf("user %s, from the credential\n", o.user)
		} else {
			users := map[string]bool{}
			var names []string
			for _, u := range uses {
				if !users[u.Username] {
					users[u.Username] = true
					names = append(names, u.Username)
				}
			}
			if len(names) != 1 {
				if len(names) > 1 {
					return fmt.Errorf("give --user: your %s targets log in as %s", o.transport, strings.Join(names, ", "))
				}
				return errors.New("a target needs --user")
			}
			o.user = names[0]
			fmt.Printf("user %s, as your other %s targets\n", o.user, o.transport)
		}
	}
	if credGiven || o.password {
		return nil
	}
	for _, u := range uses {
		if u.Username == o.user {
			o.credential = u.CredentialRef
			fmt.Printf("credential %s, as your other %s targets for %s\n", o.credential, o.transport, o.user)
			return nil
		}
	}
	ref := "keyring:kryostate/" + o.user
	if s, err := secret.NewRegistry(nil).Resolve(ctx, ref); err == nil {
		s.Zero()
		o.credential = ref
		fmt.Printf("credential %s, the password stored for %s\n", ref, o.user)
		return nil
	}
	o.credential = "agent:"
	return nil
}

func (a *app) targetRmCmd() *cobra.Command {
	var transport string
	cmd := &cobra.Command{
		Use:     "rm <host>...",
		Aliases: []string{"remove"},
		Short:   "remove hosts' connections; values already collected are kept",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := openStore(a.db)
			if err != nil {
				return err
			}
			defer st.Close()
			ids, err := st.LookupCIs(ctx, args)
			if err != nil {
				return err
			}
			for i, id := range ids {
				profiles, err := st.ConnProfiles(ctx, id)
				if err != nil {
					return err
				}
				n := 0
				for _, p := range profiles {
					if transport != "" && p.Transport != transport {
						continue
					}
					if err := st.DeleteConnProfile(ctx, id, p.Transport); err != nil {
						return err
					}
					if err := st.Audit(ctx, actor(), "remove connection", id, p.Transport); err != nil {
						return err
					}
					fmt.Printf("removed the %s connection of %s\n", p.Transport, args[i])
					n++
				}
				if n == 0 {
					what := "connection"
					if transport != "" {
						what = transport + " connection"
					}
					return fmt.Errorf("%s has no %s", args[i], what)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&transport, "transport", "t", "", "only this transport (ssh or winrm); blank for every connection the host has")
	return cmd
}

func (a *app) targetForgetCmd() *cobra.Command {
	var transport string
	cmd := &cobra.Command{
		Use:   "forget-host-key <host>",
		Short: "clear a pinned host key, after checking the host was really rebuilt",
		Long: `Clear the pinned host key, so the next collection trusts whatever key the host
presents. Use it only after checking the host was really rebuilt.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStore(a.db)
			if err != nil {
				return err
			}
			defer st.Close()
			return forgetHostKey(cmd.Context(), st, args[0], transport)
		},
	}
	cmd.Flags().StringVarP(&transport, "transport", "t", "ssh", "ssh or winrm")
	return cmd
}

func (a *app) targetSetCredentialCmd() *cobra.Command {
	var (
		forUser, credential, transport string
		password                       bool
	)
	cmd := &cobra.Command{
		Use:   "set-credential --for-user <user> (--credential <ref> | --password)",
		Short: "change the credential of every target that logs in as a user",
		Example: `  kryostate target set-credential --for-user ops --password
  kryostate target set-credential --for-user svc -t winrm -c keyring:kryostate/svc`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if !password && !cmd.Flags().Changed("credential") {
				return errors.New("give the new credential with --credential or --password")
			}
			st, err := openStore(a.db)
			if err != nil {
				return err
			}
			defer st.Close()
			ref := credential
			if password {
				if ref, err = storePassword(ctx, st, "kryostate", forUser); err != nil {
					return err
				}
			}
			if ref == "" {
				return errors.New("--credential cannot be empty")
			}
			n, err := st.SetCredentialForUser(ctx, forUser, transport, ref)
			if err != nil {
				return err
			}
			if err := st.Audit(ctx, actor(), "set credential", ref, fmt.Sprintf("%d targets logging in as %s", n, forUser)); err != nil {
				return err
			}
			fmt.Printf("%d targets logging in as %s now use %s\n", n, forUser, ref)
			return nil
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&forUser, "for-user", "", "the user whose targets change (required)")
	fs.StringVarP(&credential, "credential", "c", "", "credential reference, for example keyring:kryostate/ops")
	fs.BoolVar(&password, "password", false, "prompt for a password, keep it in the OS credential store as kryostate/<user>, and use it")
	fs.StringVarP(&transport, "transport", "t", "", "only targets over this transport (ssh or winrm); blank for every transport")
	_ = cmd.MarkFlagRequired("for-user")
	cmd.MarkFlagsMutuallyExclusive("credential", "password")
	return cmd
}

// listTargets lists every connection, with a status saying whether a
// collection uses it: a connection switched off, or one whose host is
// disabled, is listed but not used.
func listTargets(ctx context.Context, st *store.Store) error {
	hosts, err := st.Hosts(ctx, store.HostFilter{})
	if err != nil {
		return err
	}
	type target struct {
		host     store.HostRow
		profiles []store.ConnProfile
	}
	var all []target
	n := 0
	for _, h := range hosts {
		ps, err := st.ConnProfiles(ctx, h.CI.ID)
		if err != nil {
			return err
		}
		if len(ps) > 0 {
			all = append(all, target{h, ps})
			n += len(ps)
		}
	}
	if n == 0 {
		fmt.Println("no targets; add one with: kryostate target add <host> --user <user>")
		return nil
	}
	holds, err := st.CredentialHolds(ctx)
	if err != nil {
		return err
	}
	held := map[string]bool{}
	for _, h := range holds {
		if h.Held {
			held[h.Ref+"\x00"+h.Username] = true
		}
	}
	tw := newTable(os.Stdout)
	fmt.Fprintln(tw, "HOST\tID\tTRANSPORT\tCONNECT AS\tCREDENTIAL\tSTATUS")
	for _, t := range all {
		for _, p := range t.profiles {
			credential := p.CredentialRef
			if held[p.CredentialRef+"\x00"+p.Username] {
				credential += " (on hold)"
			}
			status := "enabled"
			switch {
			case t.host.CI.DisabledAt != nil:
				status = "HOST DISABLED"
			case !p.Enabled:
				status = "connection disabled"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s@%s%s\t%s\t%s\n", t.host.CI.Label, shortID(p.CIID), p.Transport,
				p.Username, p.Address, portSuffix(p.Port), credential, status)
		}
	}
	return tw.Flush()
}

func forgetHostKey(ctx context.Context, st *store.Store, ref, transport string) error {
	id, err := st.LookupCI(ctx, ref)
	if err != nil {
		return err
	}
	profiles, err := st.ConnProfiles(ctx, id)
	if err != nil {
		return err
	}
	for _, p := range profiles {
		if p.Transport != transport {
			continue
		}
		if p.HostKey == "" {
			fmt.Printf("%s has no pinned %s host key; the next collection will pin one.\n", ref, p.Transport)
			return nil
		}
		if err := st.PinHostKey(ctx, id, p.Transport, ""); err != nil {
			return err
		}
		if err := st.Audit(ctx, actor(), "forget host key", id, p.Transport+" "+p.HostKey); err != nil {
			return err
		}
		fmt.Printf("forgot the %s host key of %s. The next collection pins the key it presents then.\n", p.Transport, ref)
		fmt.Printf("collect now: kryostate refresh --only %s\n", ref)
		return nil
	}
	return fmt.Errorf("%s has no %s connection", ref, transport)
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
