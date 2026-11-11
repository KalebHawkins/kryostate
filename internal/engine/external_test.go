package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/hearsay/internal/collect"
	"github.com/KalebHawkins/hearsay/internal/model"
	"github.com/KalebHawkins/hearsay/internal/normalize"
	"github.com/KalebHawkins/hearsay/internal/store"
)

// TestHelperCollector is not a test. It is the external collector the tests
// below run, re-executing this binary so the suite needs no shell and no
// second artefact to build.
func TestHelperCollector(t *testing.T) {
	mode := os.Getenv("HELPER_MODE")
	if mode == "" {
		t.Skip("helper process, not run directly")
	}
	switch mode {
	case "ok":
		json.NewEncoder(os.Stdout).Encode(collect.Envelope{
			Schema: collect.EnvelopeSchema, Collector: "helper", Status: "ok",
			Target: os.Getenv("HEARSAY_TARGET"),
			Facts: []collect.FactItem{
				{Key: "search.home", Value: "/opt/search", Type: model.TypePath},
				{Key: "search.version", Value: "9.1.2", Type: model.TypeVersion},
			},
		})
	case "failed":
		json.NewEncoder(os.Stdout).Encode(collect.Envelope{
			Schema: collect.EnvelopeSchema, Collector: "helper", Status: "failed",
			Error: "the thing was not installed",
		})
	case "garbage":
		fmt.Fprintln(os.Stdout, "this is not JSON")
	case "leak":
		// Reports what of the parent's environment it can see, so the test can
		// check the boundary rather than infer it.
		var facts []collect.FactItem
		for i, kv := range os.Environ() {
			facts = append(facts, collect.FactItem{
				Key: "env.seen", Value: kv, Scope: fmt.Sprint(i), Type: model.TypeText,
			})
		}
		json.NewEncoder(os.Stdout).Encode(collect.Envelope{
			Schema: collect.EnvelopeSchema, Collector: "helper", Status: "ok", Facts: facts,
		})
	}
	os.Exit(0)
}

func helper(mode string) collect.External {
	return collect.External{
		Name:    "helper",
		Path:    os.Args[0],
		Args:    []string{"-test.run=TestHelperCollector"},
		Env:     []string{"HELPER_MODE=" + mode},
		Timeout: 30 * time.Second,
	}
}

func testEngine(t *testing.T, ex ...collect.External) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	norm, err := normalize.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	specs, err := collect.BuiltinSpecs()
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Store: st, Specs: specs, Norm: norm, Externals: ex}
	e.Wire()
	return e, st
}

func host(t *testing.T, st *store.Store, label string) string {
	t.Helper()
	id, _, err := st.ResolveCI(context.Background(), model.KindHost, label,
		[]model.Identity{{Scheme: model.IdentHostname, Value: label}})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestExternalCollectorReachesTheStore is the claim the documentation makes:
// an operator can add a program and see its values in the inventory. Before
// this path existed the feature was described but could not be used.
func TestExternalCollectorReachesTheStore(t *testing.T) {
	ctx := context.Background()
	e, st := testEngine(t, helper("ok"))
	id := host(t, st, "node-ext")

	sum, err := e.Collect(ctx, Options{Concurrency: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 1 || sum.Failed != 0 {
		t.Fatalf("run: %d ok, %d failed", sum.Succeeded, sum.Failed)
	}

	facts, err := st.Facts(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range facts {
		got[f.Attr] = f.Raw
	}
	if got["search.version"] != "9.1.2" {
		t.Errorf("version: got %q", got["search.version"])
	}
	if got["search.home"] != "/opt/search" {
		t.Errorf("home: got %q", got["search.home"])
	}
}

// TestExternalFailureIsRecordedNotSwallowed keeps a collector that could not
// answer from reading as a host with nothing installed.
func TestExternalFailureIsRecordedNotSwallowed(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"failed", "garbage"} {
		t.Run(mode, func(t *testing.T) {
			e, st := testEngine(t, helper(mode))
			id := host(t, st, "node-"+mode)

			sum, err := e.Collect(ctx, Options{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if sum.Failed != 1 {
				t.Errorf("a collector that did not answer counted as %d failure(s)", sum.Failed)
			}
			probes, err := st.Reachability(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			p, ok := probes[model.ProbeCollectorRun]
			if !ok {
				t.Fatal("the attempt left no trace")
			}
			if p.Outcome == model.OutcomeOK {
				t.Error("a failed collection was recorded as a success")
			}
		})
	}
}

// TestExternalCollectorGetsNoInheritedEnvironment checks the boundary the
// design rests on: a contributed program must not be handed this process's
// credentials by accident.
func TestExternalCollectorGetsNoInheritedEnvironment(t *testing.T) {
	t.Setenv("HEARSAY_TEST_SECRET", "do-not-leak")
	env, err := collect.RunExternal(context.Background(), helper("leak"), "node-x", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"HEARSAY_TARGET": true, "HEARSAY_ADDRESS": true,
		"HEARSAY_SCHEMA": true, "PATH": true, "HELPER_MODE": true,
		// Go adds this on Windows when it is absent, because a process
		// without it cannot load a DLL.
		"SYSTEMROOT": true,
	}
	for _, f := range env.Facts {
		name, _, _ := strings.Cut(f.Value, "=")
		if !want[strings.ToUpper(name)] {
			t.Errorf("the collector could see %s, which it was not given", name)
		}
	}
	if len(env.Facts) == 0 {
		t.Error("the collector saw nothing at all, not even the target it was asked about")
	}
}

func TestLoadExternalsRejectsNonsense(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := map[string]string{
		"no version":    `{"externals":[{"name":"a","path":"/bin/true"}]}`,
		"no path":       `{"version":1,"externals":[{"name":"a"}]}`,
		"duplicate":     `{"version":1,"externals":[{"name":"a","path":"/bin/true"},{"name":"a","path":"/bin/true"}]}`,
		"bad duration":  `{"version":1,"externals":[{"name":"a","path":"/bin/true","timeout":"soon"}]}`,
		"bad env entry": `{"version":1,"externals":[{"name":"a","path":"/bin/true","env":["OOPS"]}]}`,
		"unknown field": `{"version":1,"externals":[{"name":"a","path":"/bin/true","shell":true}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadExternals(write(name+".json", body)); err == nil {
				t.Error("accepted a file it should have refused")
			}
		})
	}

	good := write("good.json", `{"version":1,"externals":[
		{"name":"a","path":"/bin/true","args":["-x"],"timeout":"5s","comment":"why"}]}`)
	ex, err := LoadExternals(good)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 1 || ex[0].Timeout != 5*time.Second {
		t.Errorf("got %+v", ex)
	}
}
