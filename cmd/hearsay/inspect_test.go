package main

import (
	"flag"
	"slices"
	"testing"
)

func TestFlagsMayFollowTheHost(t *testing.T) {
	for _, args := range [][]string{
		{"web01", "-json", "-reason", "why", "env=prod"},
		{"-json", "web01", "-reason", "why", "env=prod"},
		{"-json", "-reason", "why", "web01", "env=prod"},
	} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "")
		reason := fs.String("reason", "", "")
		pos := parseInterspersed(fs, args)
		if !*asJSON || *reason != "why" || !slices.Equal(pos, []string{"web01", "env=prod"}) {
			t.Errorf("%v: json=%v reason=%q pos=%v", args, *asJSON, *reason, pos)
		}
	}
}
