package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/KalebHawkins/kryostate/internal/store"
)

type changesOpts struct {
	since string
	attr  string
	limit int
	json  bool
}

func (a *app) changesCmd() *cobra.Command {
	var o changesOpts
	cmd := &cobra.Command{
		Use:   "changes [host]",
		Short: "list values that changed, newest first, as on the Changes page",
		Long: `List values that changed, newest first: what each was before, what it is
now, and when. Only collected values count; a host's first value is not a
change. Give a host to see only its changes.`,
		Example: `  kryostate changes
  kryostate changes web01 --since 30d
  kryostate changes --attr os.version --since 7d
  kryostate changes --json`,
		Args: cobra.MaximumNArgs(1),
		RunE: a.withStore(func(ctx context.Context, st *store.Store, args []string) error {
			return listChanges(ctx, st, args, o)
		}),
	}
	fs := cmd.Flags()
	fs.StringVar(&o.since, "since", "24h", "how far back: hours (24h), days (7d) or a duration such as 90m")
	fs.StringVar(&o.attr, "attr", "", "only this attribute, such as os.version")
	fs.IntVar(&o.limit, "limit", 1000, "at most this many changes")
	fs.BoolVar(&o.json, "json", false, "print JSON")
	return cmd
}

// parseSince reads a look-back period. Days are accepted because a Go
// duration has no unit longer than an hour.
func parseSince(s string) (time.Duration, error) {
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("--since %q: give a number of days, such as 7d", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("--since %q: give hours (24h), days (7d) or a duration such as 90m", s)
	}
	return d, nil
}

func listChanges(ctx context.Context, st *store.Store, args []string, o changesOpts) error {
	since, err := parseSince(o.since)
	if err != nil {
		return err
	}
	f := store.ChangeFilter{Since: time.Now().Add(-since), Attr: o.attr, Limit: o.limit}
	if len(args) == 1 {
		if f.CIID, err = st.LookupCI(ctx, args[0]); err != nil {
			return err
		}
	}
	cs, err := st.Changes(ctx, f)
	if err != nil {
		return err
	}
	if o.json {
		type changeJSON struct {
			Host   string    `json:"host"`
			HostID string    `json:"host_id"`
			Attr   string    `json:"attr"`
			Scope  string    `json:"scope,omitempty"`
			Old    string    `json:"old"`
			New    string    `json:"new"`
			OldAt  time.Time `json:"old_at"`
			At     time.Time `json:"at"`
			Source string    `json:"source"`
			Spec   string    `json:"spec,omitempty"`
		}
		out := make([]changeJSON, 0, len(cs))
		for _, c := range cs {
			out = append(out, changeJSON{c.Host, c.CIID, c.Attr, c.Scope, c.Old, c.New, c.OldAt, c.At, c.SourceName, c.SpecID})
		}
		return writeJSON(out)
	}
	if len(cs) == 0 {
		fmt.Printf("no changes in the last %s\n", o.since)
		return nil
	}
	tw := newTable(os.Stdout)
	fmt.Fprintln(tw, "WHEN\tHOST\tATTRIBUTE\tWAS\tNOW\tSOURCE")
	for _, c := range cs {
		attr := c.Attr
		if c.Scope != "" {
			attr += " [" + c.Scope + "]"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", c.At.Local().Format("2006-01-02 15:04"), c.Host, attr,
			dash(oneLine(c.Old, 40)), dash(oneLine(c.New, 40)), c.SourceName)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	more := ""
	if len(cs) >= o.limit {
		more = " (the limit; raise it with --limit)"
	}
	fmt.Printf("%d change%s in the last %s%s.\n", len(cs), pluralS(len(cs)), o.since, more)
	return nil
}
