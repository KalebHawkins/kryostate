package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/KalebHawkins/hearsay/internal/collect"
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
