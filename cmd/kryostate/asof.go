package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/KalebHawkins/kryostate/internal/web"
)

// showAt prints a host's facts as they stood at a past moment: for each
// attribute, the value that won among what had been reported by then, under
// the same rules that pick today's value. Reachability and connections are
// left out; they describe the host now.
func showAt(ctx context.Context, db, host, when string, asJSON bool) error {
	at, err := web.ParseAt(when)
	if err != nil {
		return err
	}
	st, err := openStore(db)
	if err != nil {
		return err
	}
	defer st.Close()
	id, err := st.LookupCI(ctx, host)
	if err != nil {
		return err
	}
	ci, err := st.CI(ctx, id)
	if err != nil {
		return err
	}
	facts, err := st.FactsAt(ctx, id, at)
	if err != nil {
		return err
	}

	if asJSON {
		type factJSON struct {
			Attr     string    `json:"attr"`
			Scope    string    `json:"scope,omitempty"`
			Value    string    `json:"value"`
			Raw      string    `json:"raw"`
			Source   string    `json:"source"`
			Kind     string    `json:"source_kind"`
			Observed time.Time `json:"observed_at"`
		}
		fj := []factJSON{}
		for _, f := range facts {
			fj = append(fj, factJSON{Attr: f.Attr, Scope: f.Scope, Value: f.Norm, Raw: f.Raw,
				Source: f.SourceName, Kind: string(f.SourceKind), Observed: f.ObservedAt})
		}
		return writeJSON(map[string]any{"id": ci.ID, "name": ci.Label, "as_of": at, "facts": fj})
	}

	fmt.Printf("%s  (id %s)\n", ci.Label, ci.ID)
	fmt.Printf("as of %s\n\n", at.Format("2 Jan 2006 15:04 MST"))
	if len(facts) == 0 {
		fmt.Println("nothing had been reported about this host by then")
		return nil
	}
	tw := newTable(os.Stdout)
	fmt.Fprintln(tw, "ATTRIBUTE\tVALUE\tSOURCE\tOBSERVED")
	for _, f := range facts {
		attr := f.Attr
		if f.Scope != "" {
			attr += " [" + f.Scope + "]"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s:%s\t%s\n", attr, factValue(f), f.SourceKind, f.SourceName, f.ObservedAt.Local().Format(time.DateOnly))
	}
	return tw.Flush()
}
