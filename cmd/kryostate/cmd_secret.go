package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/KalebHawkins/kryostate/internal/secret"
	"github.com/KalebHawkins/kryostate/internal/store"
)

func (a *app) secretCmd() *cobra.Command {
	cmd := group("secret", "manage credential references, and holds on credentials that failed", "secrets")
	cmd.AddCommand(a.secretListCmd(), a.secretSetCmd(), a.secretDeleteCmd(), a.secretTestCmd(),
		a.secretHoldsCmd(), a.secretReleaseCmd())
	return cmd
}

// withStore opens the database for a command and closes it afterwards.
func (a *app) withStore(fn func(ctx context.Context, st *store.Store, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		st, err := openStore(a.db)
		if err != nil {
			return err
		}
		defer st.Close()
		return fn(cmd.Context(), st, args)
	}
}

func (a *app) secretListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "list stored passwords, which targets use each, and entries made by mistake",
		Long: `List the passwords kryostate keeps in the OS credential store, the number of
connections and notification destinations that use each, and entries that
were made by mistake. On Windows the entries are read from the Credential
Manager. Elsewhere the store cannot be listed, so only the keyring references
in use are shown, with whether each is stored. Values are never shown.`,
		Args: cobra.NoArgs,
		RunE: a.withStore(func(ctx context.Context, st *store.Store, _ []string) error {
			return listSecrets(ctx, st)
		}),
	}
}

func listSecrets(ctx context.Context, st *store.Store) error {
	uses, err := st.CredentialRefs(ctx)
	if err != nil {
		return err
	}
	dests, err := st.NotifyDests(ctx)
	if err != nil {
		return err
	}
	for _, d := range dests {
		for _, r := range []string{d.URLRef, d.PasswordRef} {
			if r != "" {
				uses[r]++
			}
		}
	}
	services := map[string]bool{"kryostate": true}
	for ref := range uses {
		if v, ok := strings.CutPrefix(ref, "keyring:"); ok {
			if svc, _, ok := strings.Cut(v, "/"); ok {
				services[svc] = true
			}
		}
	}
	entries, err := secret.ListKeyring(func(e secret.KeyringEntry) bool {
		return services[e.Service] || strings.Contains(e.Service, "kryostate")
	})
	listable := !errors.Is(err, secret.ErrListUnsupported)
	if err != nil && listable {
		return err
	}

	type row struct{ entry, ref, state string }
	var rows []row
	var fixes []string
	stored := map[string]bool{}
	for _, e := range entries {
		stored[e.Ref()] = true
		r := row{entry: e.Name, ref: e.Ref()}
		switch {
		case e.Mistaken():
			r.ref, r.state = "-", "mistake"
			fixes = append(fixes, fmt.Sprintf("kryostate secret delete --entry '%s'", e.Name))
		case uses[e.Ref()] > 0:
			r.state = fmt.Sprintf("used by %d", uses[e.Ref()])
		default:
			r.state = "not used"
		}
		rows = append(rows, r)
	}
	// References in use that are not stored: on Windows read from the list,
	// elsewhere by asking the store for each one.
	reg := secret.NewRegistry(nil)
	refs := make([]string, 0, len(uses))
	for ref := range uses {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		if !strings.HasPrefix(ref, "keyring:") || stored[ref] {
			continue
		}
		state := "missing"
		if !listable {
			if s, err := reg.Resolve(ctx, ref); err == nil {
				s.Zero()
				state = fmt.Sprintf("used by %d", uses[ref])
			}
		}
		if state == "missing" {
			fixes = append(fixes, "kryostate secret set "+strings.TrimPrefix(ref, "keyring:"))
		}
		rows = append(rows, row{entry: "-", ref: ref, state: state})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].entry < rows[j].entry })

	if !listable {
		fmt.Println("This operating system's credential store cannot be listed; showing the keyring references in use.")
	}
	if len(rows) == 0 {
		fmt.Println("no passwords stored for kryostate, and no target uses one")
	} else {
		tw := newTable(os.Stdout)
		fmt.Fprintln(tw, "STORED AS\tREFERENCE\tSTATE")
		for _, r := range rows {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", r.entry, r.ref, r.state)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if len(fixes) > 0 {
		fmt.Println("\nTo fix:")
		for _, f := range fixes {
			fmt.Println("  " + f)
		}
	}
	schemes := reg.Schemes()
	sort.Strings(schemes)
	fmt.Printf("\nreference types this build reads: %s:\n", strings.Join(schemes, ": "))
	return nil
}

// serviceAccount reads service/account. A colon in the service means a whole
// reference such as keyring:kryostate/ops was given, which would be stored
// under a name nothing reads.
func serviceAccount(arg string) (string, string, error) {
	if v, ok := strings.CutPrefix(arg, "keyring:"); ok {
		return "", "", fmt.Errorf("%q is a reference; give the part after keyring:, such as: %s", arg, v)
	}
	service, account, ok := strings.Cut(arg, "/")
	if !ok || service == "" || account == "" || strings.Contains(account, "/") {
		return "", "", fmt.Errorf("%q: give it as service/account, such as kryostate/ops", arg)
	}
	if strings.Contains(service, ":") {
		return "", "", fmt.Errorf("%q: the service %q holds a colon; give it as service/account, such as kryostate/ops", arg, service)
	}
	return service, account, nil
}

func (a *app) secretSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <service/account>",
		Short: "store material in the OS credential store; also releases a hold on keyring:service/account",
		Long: `Store material in the OS credential store, as service/account, and release any
hold on keyring:service/account. The value is read from the terminal, twice,
without echo, or from standard input in a script; never from a flag, which
would leave it in the process list and the shell history.`,
		Example: "  kryostate secret set kryostate/ops",
		Args:    cobra.ExactArgs(1),
		RunE: a.withStore(func(ctx context.Context, st *store.Store, args []string) error {
			service, account, err := serviceAccount(args[0])
			if err != nil {
				return err
			}
			_, err = storePassword(ctx, st, service, account)
			return err
		}),
	}
}

func (a *app) secretDeleteCmd() *cobra.Command {
	var entry string
	cmd := &cobra.Command{
		Use:     "delete [service/account]",
		Aliases: []string{"rm"},
		Short:   "remove a password from the OS credential store",
		Long: `Remove a password from the OS credential store. Name it as service/account,
the part of its reference after keyring:, or with --entry exactly as
"secret list" shows it under STORED AS, which also removes entries made by
mistake.`,
		Example: "  kryostate secret delete kryostate/ops\n  kryostate secret delete --entry 'keyring:kryostate:ops'",
		Args:    cobra.MaximumNArgs(1),
		RunE: a.withStore(func(ctx context.Context, st *store.Store, args []string) error {
			var service, account string
			switch {
			case entry != "" && len(args) == 0:
				e, ok := secret.EntryFromName(entry)
				if !ok {
					return fmt.Errorf("%q is not a stored entry name; copy it from: kryostate secret list", entry)
				}
				service, account = e.Service, e.Account
			case entry == "" && len(args) == 1:
				var err error
				if service, account, err = serviceAccount(args[0]); err != nil {
					return err
				}
			default:
				return errors.New("give service/account or --entry, not both")
			}
			ref := "keyring:" + service + "/" + account
			if n, err := st.CredentialRefs(ctx); err != nil {
				return err
			} else if n[ref] > 0 {
				fmt.Printf("note: %d connections use %s; they cannot log in until it is stored again\n", n[ref], ref)
			}
			if err := secret.DeleteKeyring(service, account); err != nil {
				return fmt.Errorf("remove %s:%s: %w", service, account, err)
			}
			if err := st.Audit(ctx, actor(), "delete credential", ref, ""); err != nil {
				return err
			}
			fmt.Printf("removed %s:%s from the OS credential store\n", service, account)
			return nil
		}),
	}
	cmd.Flags().StringVar(&entry, "entry", "", `an entry's name exactly as "secret list" shows it`)
	return cmd
}

func (a *app) secretTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "test <ref>",
		Short:   "resolve a reference and report only whether it worked",
		Example: "  kryostate secret test keyring:kryostate/ops\n  kryostate secret test env:KRYOSTATE_SMTP_PASSWORD",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := secret.NewRegistry(nil).Resolve(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			defer s.Zero()
			fmt.Printf("resolved, kind %s\n", s.Kind)
			return nil
		},
	}
}

func (a *app) secretHoldsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "holds",
		Short: "list password credentials that failed to log in, and which are on hold",
		Args:  cobra.NoArgs,
		RunE: a.withStore(func(ctx context.Context, st *store.Store, _ []string) error {
			hs, err := st.CredentialHolds(ctx)
			if err != nil {
				return err
			}
			if len(hs) == 0 {
				fmt.Println("no credential has failed to log in")
				return nil
			}
			tw := newTable(os.Stdout)
			fmt.Fprintln(tw, "CREDENTIAL\tUSER\tFAILURES\tSTATE\tLAST HOST\tWHEN\tLAST ERROR")
			for _, h := range hs {
				state := "counting"
				if h.Held {
					state = "on hold"
				}
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n", h.Ref, h.Username, h.Fails, state,
					dash(h.LastHost), h.UpdatedAt.Local().Format("2006-01-02 15:04"), dash(oneLine(h.LastError, 200)))
			}
			return tw.Flush()
		}),
	}
}

func (a *app) secretReleaseCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "release <ref>",
		Short:   "lift the hold on a credential reference, so hosts are tried with it again",
		Example: "  kryostate secret release keyring:kryostate/ops",
		Args:    cobra.ExactArgs(1),
		RunE: a.withStore(func(ctx context.Context, st *store.Store, args []string) error {
			n, err := st.ClearCredentialHold(ctx, args[0], "")
			if err != nil {
				return err
			}
			if n == 0 {
				fmt.Printf("%s was not on hold\n", args[0])
				return nil
			}
			if err := st.Audit(ctx, actor(), "release credential", args[0], ""); err != nil {
				return err
			}
			fmt.Printf("released %s: hosts are tried with it again\n", args[0])
			return nil
		}),
	}
}

// storePassword prompts for a password, keeps it in the OS credential store
// as service/account, lifts any hold on it, and returns its reference. At a
// terminal it asks twice: a mistyped password would otherwise be tried
// against every host until the account locked.
func storePassword(ctx context.Context, st *store.Store, service, account string) (string, error) {
	v, err := readSecret(fmt.Sprintf("password for %s: ", account))
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", errors.New("refusing to store an empty password")
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		again, err := readSecret("again: ")
		if err != nil {
			return "", err
		}
		if again != v {
			return "", errors.New("the two entries differ; nothing stored")
		}
	}
	if err := secret.StoreKeyring(service, account, v); err != nil {
		return "", err
	}
	ref := "keyring:" + service + "/" + account
	if err := releaseHold(ctx, st, ref); err != nil {
		return "", err
	}
	if err := st.Audit(ctx, actor(), "store credential", ref, ""); err != nil {
		return "", err
	}
	fmt.Printf("stored the password in the OS credential store; reference it as %s\n", ref)
	return ref, nil
}

func releaseHold(ctx context.Context, st *store.Store, ref string) error {
	n, err := st.ClearCredentialHold(ctx, ref, "")
	if err != nil {
		return err
	}
	if n > 0 {
		if err := st.Audit(ctx, actor(), "release credential", ref, ""); err != nil {
			return err
		}
		fmt.Printf("released %s: hosts are tried with it again\n", ref)
	}
	return nil
}
