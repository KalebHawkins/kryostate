package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/KalebHawkins/hearsay/internal/collect"
	"github.com/KalebHawkins/hearsay/internal/model"
	"github.com/KalebHawkins/hearsay/internal/store"
)

// TestCatalogCollectorsFollowTheirStacks: a collector limited to a stack runs
// only on that stack's members, and one for every host runs everywhere.
func TestCatalogCollectorsFollowTheirStacks(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	specs, _ := collect.BuiltinSpecs()
	e := &Engine{Store: st, Specs: specs}

	host := func(name string) string {
		id, _, err := st.ResolveCI(ctx, model.KindHost, name, []model.Identity{{Scheme: model.IdentHostname, Value: name}})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	fwd, plain := host("fwd01"), host("app01")
	sid, _ := st.EnsureStack(ctx, "splunk")
	if err := st.AddToStack(ctx, sid, fwd, "forwarder"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutChoice(ctx, collect.Choice{Template: "splunk.uf.version", Stacks: []string{"splunk"}}, "op"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutChoice(ctx, collect.Choice{Template: "cpu.cores"}, "op"); err != nil {
		t.Fatal(err)
	}

	ids := func(ci string) string {
		got, err := e.specsForHost(ctx, ci, "linux")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, s := range got {
			out = append(out, s.ID)
		}
		return strings.Join(out, " ")
	}
	if got := ids(fwd); !strings.Contains(got, "catalog.splunk.uf.version.unix") || !strings.Contains(got, "catalog.cpu.cores.linux") {
		t.Errorf("the forwarder runs: %s", got)
	}
	if got := ids(plain); strings.Contains(got, "splunk") || !strings.Contains(got, "catalog.cpu.cores.linux") {
		t.Errorf("a host outside the stack runs: %s", got)
	}
	// Windows hosts get the Windows variant, never the Linux command.
	win, err := e.specsForHost(ctx, fwd, "windows")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range win {
		if strings.HasPrefix(s.ID, "catalog.") && !strings.HasSuffix(s.ID, ".windows") {
			t.Errorf("windows host given %s", s.ID)
		}
	}
	// Reparse must know catalog specs, or stored output for them is orphaned.
	all, _ := e.AllSpecs(ctx)
	found := false
	for _, s := range all.Specs {
		found = found || s.ID == "catalog.cpu.cores.linux"
	}
	if !found {
		t.Error("AllSpecs leaves out switched-on catalog collectors")
	}
}
