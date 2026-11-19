package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/KalebHawkins/kryostate/internal/ingest"
	"github.com/KalebHawkins/kryostate/internal/secret"
)

func (a *app) importCmd() *cobra.Command {
	var (
		format                  string
		dryRun, example, fromDB bool
	)
	cmd := &cobra.Command{
		Use:   "import <file.yaml|file.json|->",
		Short: "create hosts in bulk from a YAML or JSON file, with their targets and stacks",
		Long: `Create or update hosts in bulk from a YAML or JSON file: each host, the target
it is collected through, and the stacks it belongs to with its role in each.
Defaults in the file apply to every host's target; a host's own target
fields override them, and "target: false" gives a host no connection.

The whole file is checked first and every problem is listed with its line;
nothing is written while there are any. Hosts are matched by name or fqdn.
Importing the same file again changes nothing. Hosts, connections and stack
memberships that are not in the file are left alone, never removed.

Credentials are references such as agent: or keyring:kryostate/ops, never
passwords. A keyring credential not yet in the OS credential store is warned
about, with the command that stores it.

--example prints a commented file to start from; --from-db prints what is
recorded now in the same format.`,
		Example: `  kryostate import --example > hosts.yaml
  kryostate import hosts.yaml --dry-run
  kryostate import hosts.yaml
  kryostate import - --format json < hosts.json
  kryostate import --from-db > hosts.yaml`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case example:
				if len(args) > 0 || fromDB {
					return errors.New("--example takes no file")
				}
				fmt.Print(ingest.Example)
				return nil
			case fromDB:
				if len(args) > 0 {
					return errors.New("--from-db takes no file; redirect its output to one")
				}
				return a.importFromDB(cmd, format)
			case len(args) == 0:
				return errors.New("name the file to import, or - for standard input; kryostate import --example prints one to start from")
			}
			return a.importFile(cmd, args[0], format, dryRun)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&format, "format", "", "yaml or json; needed for standard input, otherwise taken from the extension or the content (with --from-db: yaml unless given)")
	fs.BoolVar(&dryRun, "dry-run", false, "print what would change, and write nothing")
	fs.BoolVar(&example, "example", false, "print a commented example file")
	fs.BoolVar(&fromDB, "from-db", false, "print the recorded hosts, targets and stacks as an import file")
	return cmd
}

func (a *app) importFile(cmd *cobra.Command, path, format string, dryRun bool) error {
	ctx := cmd.Context()
	var (
		data []byte
		err  error
	)
	source := filepath.Base(path)
	if path == "-" {
		if format == "" {
			return errors.New("give --format yaml or --format json when reading standard input")
		}
		source = "standard input"
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return err
	}
	if format == "" {
		format = ingest.DetectFormat(path, data)
	}
	f, err := ingest.Load(data, format)
	if err != nil {
		return err
	}
	st, err := openStore(a.db)
	if err != nil {
		return err
	}
	defer st.Close()
	reg := secret.NewRegistry(nil)
	opt := ingest.Options{By: actor(), Source: source, CredentialMissing: func(ref string) bool {
		s, err := reg.Resolve(ctx, ref)
		if err == nil {
			s.Zero()
			return false
		}
		return errors.Is(err, secret.ErrNotFound)
	}}
	plan, err := ingest.PlanImport(ctx, st, f, opt)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	plan.Write(out)
	const kept = "nothing is removed: hosts, targets and stack memberships not in the file are left as they are"
	if dryRun {
		fmt.Fprintln(out, plan.Summary())
		if !plan.Empty() {
			fmt.Fprintln(out, "dry run: nothing was written; "+kept)
		}
		return nil
	}
	if err := ingest.Apply(ctx, st, plan, opt); err != nil {
		return err
	}
	fmt.Fprintln(out, plan.Summary())
	if !plan.Empty() {
		fmt.Fprintln(out, kept)
	}
	return nil
}

func (a *app) importFromDB(cmd *cobra.Command, format string) error {
	if format == "" {
		format = ingest.FormatYAML
	}
	st, err := openStore(a.db)
	if err != nil {
		return err
	}
	defer st.Close()
	f, notes, err := ingest.FromStore(cmd.Context(), st)
	if err != nil {
		return err
	}
	b, err := ingest.Encode(f, format)
	if err != nil {
		return err
	}
	for _, n := range notes {
		fmt.Fprintln(os.Stderr, "note:", n)
	}
	_, err = cmd.OutOrStdout().Write(b)
	return err
}
