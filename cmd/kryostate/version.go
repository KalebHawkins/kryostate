package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/KalebHawkins/kryostate/internal/version"
)

// versionCmd prints which build this is: the version, the commit it was built
// from, when, and with which Go.
func (a *app) versionCmd() *cobra.Command {
	var asJSON, short bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "print the version, commit and build date",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			in := version.Get()
			switch {
			case asJSON:
				return writeJSON(in)
			case short:
				fmt.Println(version.Short())
			default:
				fmt.Print(in.String())
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON instead of text")
	cmd.Flags().BoolVar(&short, "short", false, "print the version alone")
	return cmd
}
