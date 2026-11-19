package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/KalebHawkins/kryostate/internal/store"
)

func (a *app) evidenceCmd() *cobra.Command {
	cmd := group("evidence", "list or delete the stored output of collector commands")
	var f evidenceFlags
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "list stored command output",
		Args:    cobra.NoArgs,
		RunE: a.withStore(func(ctx context.Context, st *store.Store, _ []string) error {
			return listEvidence(ctx, st, f)
		}),
	}
	addEvidenceFlags(list, &f)
	list.Flags().BoolVar(&f.show, "show", false, "print the stored output, not just a summary of it")

	var rf evidenceFlags
	rm := &cobra.Command{
		Use:     "rm",
		Aliases: []string{"forget"},
		Short:   "delete the matching stored output",
		Example: "  kryostate evidence rm --host web01\n  kryostate evidence rm --spec linux.os.version",
		Args:    cobra.NoArgs,
		RunE: a.withStore(func(ctx context.Context, st *store.Store, _ []string) error {
			filter, err := rf.filter(ctx, st)
			if err != nil {
				return err
			}
			if filter.CIID == "" && filter.SpecID == "" && filter.Attr == "" {
				return errors.New("refusing to delete every stored output without --host, --spec or --attr")
			}
			n, err := st.ForgetEvidence(ctx, filter)
			if err != nil {
				return err
			}
			fmt.Printf("%d stored outputs deleted\n", n)
			return nil
		}),
	}
	addEvidenceFlags(rm, &rf)
	cmd.AddCommand(list, rm)
	return cmd
}

type evidenceFlags struct {
	host, spec, attr string
	show             bool
}

func addEvidenceFlags(cmd *cobra.Command, f *evidenceFlags) {
	fs := cmd.Flags()
	fs.StringVar(&f.host, "host", "", "only this host (name, fqdn or id)")
	fs.StringVar(&f.spec, "spec", "", "collector id")
	fs.StringVar(&f.attr, "attr", "", "SQL LIKE pattern for attributes")
}

func (f evidenceFlags) filter(ctx context.Context, st *store.Store) (store.EvidenceFilter, error) {
	out := store.EvidenceFilter{SpecID: f.spec, Attr: f.attr}
	if f.host != "" {
		id, err := st.LookupCI(ctx, f.host)
		if err != nil {
			return out, err
		}
		out.CIID = id
	}
	return out, nil
}

func listEvidence(ctx context.Context, st *store.Store, f evidenceFlags) error {
	filter, err := f.filter(ctx, st)
	if err != nil {
		return err
	}
	ev, err := st.AllEvidence(ctx, filter)
	if err != nil {
		return err
	}
	if len(ev) == 0 {
		fmt.Println("nothing stored")
		return nil
	}
	for _, e := range ev {
		marker := ""
		if e.Truncated {
			marker = " (truncated)"
		}
		fmt.Printf("%s  %s  %s  %d bytes%s  %s\n",
			e.CIID, e.SpecID, e.Attr, len(e.Output), marker, e.At.Format(time.RFC3339))
		if f.show {
			fmt.Printf("  $ %s\n", safeText(e.Command, false))
			for _, line := range strings.Split(strings.TrimRight(safeText(e.Output, true), "\n"), "\n") {
				fmt.Printf("  | %s\n", line)
			}
		}
	}
	return nil
}
