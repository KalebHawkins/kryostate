package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/KalebHawkins/kryostate/internal/collect"
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
