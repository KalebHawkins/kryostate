package main

import (
	"errors"
	"flag"
	"fmt"

	"github.com/KalebHawkins/kryostate/internal/version"
)

// cmdVersion prints which build this is: the version, the commit it was built
// from, when, and with which Go.
func cmdVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print JSON instead of text")
	short := fs.Bool("short", false, "print the version alone")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: kryostate version [-json] [-short]")
	}
	in := version.Get()
	switch {
	case *asJSON:
		return writeJSON(in)
	case *short:
		fmt.Println(version.Short())
	default:
		fmt.Print(in.String())
	}
	return nil
}
