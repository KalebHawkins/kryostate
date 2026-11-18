package main

import (
	"context"
	"errors"
	"fmt"
	"os"
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
		Short:   "list the reference schemes this build understands",
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			fmt.Println("reference schemes this build understands:")
			for _, s := range secret.NewRegistry(nil).Schemes() {
				fmt.Println("  " + s + ":")
			}
			return nil
		},
	}
}

func serviceAccount(arg string) (string, string, error) {
	service, account, ok := strings.Cut(arg, "/")
	if !ok || service == "" || account == "" {
		return "", "", fmt.Errorf("%q: give it as service/account, such as kryostate/ops", arg)
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
	return &cobra.Command{
		Use:     "delete <service/account>",
		Aliases: []string{"rm"},
		Short:   "remove material from the OS credential store",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			service, account, err := serviceAccount(args[0])
			if err != nil {
				return err
			}
			return secret.DeleteKeyring(service, account)
		},
	}
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
					dash(h.LastHost), h.UpdatedAt.Local().Format("2006-01-02 15:04"), dash(h.LastError))
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
