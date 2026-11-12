package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/engine"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// TestStarterFileWorks: the file -init writes is the first thing a new user
// sees, so it must load and its example must read a value.
func TestStarterFileWorks(t *testing.T) {
	set, err := collect.LoadSpecs([]byte(starterCollectors))
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Specs) != 1 {
		t.Fatalf("%d specs", len(set.Specs))
	}
	v, err := collect.Extract(set.Specs[0].Parse, "UTC\n")
	if err != nil || v != "UTC" {
		t.Errorf("got %q, %v", v, err)
	}
	b, _ := collect.BuiltinSpecs()
	if err := b.Merge([]byte(starterCollectors)); err != nil {
		t.Errorf("the starter clashes with a built-in collector: %v", err)
	}
}

// TestFilesNextToTheDatabaseAreFound: a collectors.json beside the database
// is used without a flag, and a flag still wins.
func TestFilesNextToTheDatabaseAreFound(t *testing.T) {
	dir := t.TempDir()
	old := dataDir
	dataDir = dir
	t.Cleanup(func() { dataDir = old })

	if got := (engineFiles{}).resolved(); got.Collectors != "" {
		t.Errorf("found %q in an empty folder", got.Collectors)
	}
	p := filepath.Join(dir, defaultCollectorsFile)
	if err := os.WriteFile(p, []byte(starterCollectors), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := (engineFiles{}).resolved(); got.Collectors != p {
		t.Errorf("got %q, want %q", got.Collectors, p)
	}
	if got := (engineFiles{Collectors: "other.json"}).resolved(); got.Collectors != "other.json" {
		t.Errorf("a flag was overridden: %q", got.Collectors)
	}
}

// TestEnableTarget: -stacks becomes a query, -where is checked before
// anything is saved, and both together must both hold.
func TestEnableTarget(t *testing.T) {
	for _, c := range []struct{ stacks, where, want, err string }{
		{"", "", "", ""},
		{"web", "", `stack == "web"`, ""},
		{"web, db", "", `stack in ("web", "db")`, ""},
		{"", `name in ("web01", "web02")`, `name in ("web01", "web02")`, ""},
		{"web", `os.family == "Linux" || name == "x"`, `stack == "web" && (os.family == "Linux" || name == "x")`, ""},
		{"", `name in ("web01"`, "", "column"},
	} {
		got, err := enableTarget(c.stacks, c.where)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), "^") {
				t.Errorf("%q: got %v, want an error with a caret", c.where, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%q %q: got %q %v, want %q", c.stacks, c.where, got, err, c.want)
		}
	}
}

func TestTryAndDisableFindCatalogCollectors(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	eng, err := buildEngine(st, engineFiles{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A catalog entry can be tried before it is switched on.
	specs, err := specsToTry(ctx, eng, "memory.total", "")
	if err != nil || len(specs) != 3 {
		t.Fatalf("got %d specs, %v", len(specs), err)
	}
	if _, err := specsToTry(ctx, eng, "file.exists", ""); err == nil {
		t.Error("a template was tried with no value")
	}
	if _, err := specsToTry(ctx, eng, "linux.os.version", ""); err != nil {
		t.Errorf("a built-in spec id: %v", err)
	}

	a, _ := st.PutChoice(ctx, collect.Choice{Template: "file.exists", Param: "/etc/a"}, "op")
	st.PutChoice(ctx, collect.Choice{Template: "file.exists", Param: "/etc/b"}, "op")
	if _, err := findChoice(ctx, st, "file.exists", ""); err == nil || !strings.Contains(err.Error(), "-param") {
		t.Errorf("an ambiguous name gave %v", err)
	}
	if c, err := findChoice(ctx, st, "file.exists", "/etc/a"); err != nil || c.ID != a.ID {
		t.Errorf("by name and value: %v", err)
	}
	if c, err := findChoice(ctx, st, strconv.FormatInt(a.ID, 10), ""); err != nil || c.ID != a.ID {
		t.Errorf("by number: %v", err)
	}
}

// TestListShowsARemovedCatalogEntry: a collector switched on from a catalog
// entry this build no longer has is listed, marked, with how to remove it,
// and -disable finds it by number.
func TestListShowsARemovedCatalogEntry(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	res, err := st.DB().ExecContext(ctx,
		`INSERT INTO collector_choice (template, param, attr, target, created_at)
		 VALUES ('vendor.gone.version', '', 'vendor.gone.version', '', '2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	eng, err := buildEngine(st, engineFiles{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	lerr := listCollectors(ctx, eng, st, "")
	os.Stdout = stdout
	w.Close()
	out, _ := io.ReadAll(r)
	if lerr != nil {
		t.Fatal(lerr)
	}
	for _, want := range []string{"vendor.gone.version (no longer in the catalog, skipped)",
		"kryostate collectors -disable " + strconv.FormatInt(id, 10)} {
		if !strings.Contains(string(out), want) {
			t.Errorf("listing lacks %q:\n%s", want, out)
		}
	}
	c, err := findChoice(ctx, st, strconv.FormatInt(id, 10), "")
	if err != nil || c.Template != "vendor.gone.version" {
		t.Fatalf("-disable by number: %+v, %v", c, err)
	}
	if err := st.DeleteChoice(ctx, c.ID, "test"); err != nil {
		t.Fatal(err)
	}
}

// TestShowPrintsALoadableDefinition: -show is the command-line way to see a
// collector written out, and what it prints must load as a collectors file.
func TestShowPrintsALoadableDefinition(t *testing.T) {
	specs, err := collect.BuiltinSpecs()
	if err != nil {
		t.Fatal(err)
	}
	eng := &engine.Engine{Specs: specs}
	for _, c := range []struct{ ref, param, want string }{
		{"cpu.cores", "", `"id": "cpu.cores.linux"`},
		{"file.exists", "/etc/hosts", `'/etc/hosts'`},
		{"builtin.os.version", "", `"id": "aix.os.version"`},
		{"linux.os.family", "", `"command": "uname -s"`},
	} {
		var out strings.Builder
		if err := showDefinition(&out, eng, c.ref, c.param); err != nil {
			t.Errorf("%s: %v", c.ref, err)
			continue
		}
		if !strings.Contains(out.String(), c.want) {
			t.Errorf("%s lacks %s:\n%s", c.ref, c.want, out.String())
		}
		if _, err := collect.LoadSpecs([]byte(out.String())); err != nil {
			t.Errorf("%s does not load: %v", c.ref, err)
		}
	}
	if err := showDefinition(io.Discard, eng, "made.up", ""); err == nil {
		t.Error("an unknown collector was shown")
	}
}

// TestServeSurvivesABrokenCollectorsFile: the check serve makes before
// deciding to run without the file.
func TestServeSurvivesABrokenCollectorsFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "collectors.json")
	os.WriteFile(p, []byte(`{"version":1,"specs":[{"id":"x","optinal":true}]}`), 0o600)
	if err := checkCollectorsFile(p); err == nil || !strings.Contains(err.Error(), "optinal") {
		t.Errorf("a broken file gave %v", err)
	}
	os.WriteFile(p, []byte(starterCollectors), 0o600)
	if err := checkCollectorsFile(p); err != nil {
		t.Errorf("the starter file gave %v", err)
	}
}
