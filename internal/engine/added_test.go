package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// TestAddedCollectorRuns: a collector added to the store after the engine was
// built is collected on the next run, listed as added, and loses to the
// collectors file when both have its id.
func TestAddedCollectorRuns(t *testing.T) {
	ctx := context.Background()
	e, st := testEngine(t)
	first := collect.Parse{Kind: collect.ParseFirstLine}
	mine := collect.Spec{ID: "mine.kernel", Attr: "custom.kernel", OS: []string{"linux"}, Command: "uname -r", Parse: first}
	if err := e.CheckAdd(ctx, []collect.Spec{mine}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutUserCollector(ctx, mine, "ops"); err != nil {
		t.Fatal(err)
	}
	tr := &answers{reply: map[string]collect.Result{"uname -r": {Stdout: "6.1.0-web\n"}}}
	e.Dial = func(context.Context, store.ConnProfile) (collect.Transport, string, error) { return tr, "linux", nil }
	id := host(t, st, "web01")
	p := store.ConnProfile{CIID: id, Transport: "ssh", Address: "web01", Port: 22, Username: "ops", CredentialRef: "agent:", Enabled: true}
	if _, err := e.collectSome(ctx, "r1", p, nil, nil); err != nil {
		t.Fatal(err)
	}
	facts, err := st.Facts(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	got := ""
	for _, f := range facts {
		if f.Attr == "custom.kernel" {
			got = f.Raw
		}
	}
	if got != "6.1.0-web" {
		t.Errorf("custom.kernel = %q", got)
	}

	cs, err := e.Collectors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cs {
		if c.Key == CollectorKey("mine.kernel") {
			found = c.Added != nil && c.Added.AddedBy == "ops" && c.Kind == KindSpec
		}
	}
	if !found {
		t.Error("the added collector is not listed as added")
	}

	// The collectors file gains the same id: the file wins, and the added
	// one is not run twice.
	fromFile := mine
	fromFile.Command = "uname -rv"
	e.Specs.Specs = append(e.Specs.Specs, fromFile)
	loaded, err := e.LoadedSpecs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, sp := range loaded {
		if sp.ID == "mine.kernel" {
			n++
			if sp.Command != "uname -rv" {
				t.Errorf("the added collector won over the file: %q", sp.Command)
			}
		}
	}
	if n != 1 {
		t.Errorf("mine.kernel is loaded %d times", n)
	}
	if err := e.CheckAdd(ctx, []collect.Spec{mine}); err == nil || !strings.Contains(err.Error(), "collectors file") {
		t.Errorf("an id the file has was accepted: %v", err)
	}
}

// TestCheckAddRefuses: ids other collectors use, and mistakes a collectors
// file would only reveal at collection time.
func TestCheckAddRefuses(t *testing.T) {
	ctx := context.Background()
	e, _ := testEngine(t)
	ok := collect.Spec{ID: "mine.x", Attr: "custom.x", OS: []string{"linux"}, Command: "true", Parse: collect.Parse{Kind: collect.ParseFirstLine}}
	for want, edit := range map[string]func(*collect.Spec){
		"built-in":     func(s *collect.Spec) { s.ID = "linux.os.family" },
		"catalog":      func(s *collect.Spec) { s.ID = "cpu.cores" },
		"reserved":     func(s *collect.Spec) { s.ID = "builtin.x" },
		"os":           func(s *collect.Spec) { s.OS = []string{"plan9"} },
		"type":         func(s *collect.Spec) { s.Type = "colour" },
		"where":        func(s *collect.Spec) { s.Where = `name ==` },
		"no stack":     func(s *collect.Spec) { s.Stacks = []string{"nowhere"} },
		"no command":   func(s *collect.Spec) { s.Command = "" },
		"no attribute": func(s *collect.Spec) { s.Attr = "" },
	} {
		sp := ok
		edit(&sp)
		if err := e.CheckAdd(ctx, []collect.Spec{ok, sp}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
	if err := e.CheckAdd(ctx, []collect.Spec{ok}); err != nil {
		t.Errorf("a good spec was refused: %v", err)
	}
}
