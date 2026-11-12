package engine

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// TestChecksRecordWhatTheyFound covers the heartbeat the inventory is read
// against: a host that answers on its service port, one that does not, and a
// health endpoint that returns the wrong status. All three are different
// answers and must not collapse into one.
func TestChecksRecordWhatTheyFound(t *testing.T) {
	ctx := context.Background()
	e, st := testEngine(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer healthy.Close()
	sick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer sick.Close()

	up := host(t, st, "node-up")
	down := host(t, st, "node-down")
	ill := host(t, st, "node-ill")

	put := func(ci, name, kind, target string) {
		t.Helper()
		if err := st.PutCheck(ctx, store.Check{
			CIID: ci, Name: name, Kind: kind, Target: target,
			Timeout: 5 * time.Second, Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	put(up, "port", "tcp", ln.Addr().String())
	put(up, "health", "http", healthy.URL)
	// Port 1 on the loopback: nothing is listening and the refusal is immediate.
	put(down, "port", "tcp", "127.0.0.1:1")
	put(ill, "health", "http", sick.URL)

	results, err := e.RunChecks(ctx, nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 4 {
		t.Fatalf("ran %d checks, defined 4", len(results))
	}

	outcome := func(ci string, kind model.ProbeKind) model.Probe {
		t.Helper()
		r, err := st.Reachability(ctx, ci)
		if err != nil {
			t.Fatal(err)
		}
		return r[kind]
	}
	if got := outcome(up, model.ProbeTCP).Outcome; got != model.OutcomeOK {
		t.Errorf("a listening port was recorded as %s", got)
	}
	if got := outcome(up, model.ProbeHealth).Outcome; got != model.OutcomeOK {
		t.Errorf("a 204 did not satisfy the default 2xx: %s", got)
	}
	if got := outcome(down, model.ProbeTCP).Outcome; got == model.OutcomeOK {
		t.Error("a closed port was recorded as reachable")
	}
	ip := outcome(ill, model.ProbeHealth)
	if ip.Outcome == model.OutcomeOK {
		t.Error("a 503 was recorded as healthy")
	}
	if ip.Detail == "" {
		t.Error("the status it actually returned was not recorded")
	}
}

// TestCheckDefinitionsAreCheckedWhenTyped keeps a typo from becoming a
// permanently failing check nobody can explain.
func TestCheckDefinitionsAreCheckedWhenTyped(t *testing.T) {
	ctx := context.Background()
	_, st := testEngine(t)
	ci := host(t, st, "node-typo")

	bad := []store.Check{
		{CIID: ci, Name: "a", Kind: "tcp", Target: "nohost"},
		{CIID: ci, Name: "a", Kind: "http", Target: "ssh://x/y"},
		{CIID: ci, Name: "a", Kind: "http", Target: "http://x/y", Expect: "healthy"},
		{CIID: ci, Name: "a", Kind: "ping", Target: "x"},
		{CIID: ci, Name: "", Kind: "tcp", Target: "x:1"},
	}
	for i, c := range bad {
		if err := st.PutCheck(ctx, c); err == nil {
			t.Errorf("case %d: accepted a check that cannot work", i)
		}
	}
	good := store.Check{CIID: ci, Name: "port", Kind: "tcp", Target: "example.invalid:443", Enabled: true}
	if err := st.PutCheck(ctx, good); err != nil {
		t.Fatal(err)
	}
	cs, err := st.Checks(ctx, ci)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Timeout == 0 {
		t.Errorf("got %+v", cs)
	}
}

// TestUndocumentedIsRaisedOnlyForAttributesAnyoneTracks is the difference
// between a useful finding and a page of noise.
func TestUndocumentedIsRaisedOnlyForAttributesAnyoneTracks(t *testing.T) {
	ctx := context.Background()
	e, st := testEngine(t)

	doc, err := st.SourceID(ctx, model.SourceDocument, "inventory.md")
	if err != nil {
		t.Fatal(err)
	}
	col, err := st.SourceID(ctx, model.SourceCollector, "builtin")
	if err != nil {
		t.Fatal(err)
	}
	assert := func(ci, attr, raw string, src int64) {
		t.Helper()
		if _, err := st.Assert(ctx, model.Assertion{
			CIID: ci, Attr: attr, Raw: raw, SourceID: src, ObservedAt: time.Now(),
		}, nil); err != nil {
			t.Fatal(err)
		}
	}

	// One host is written down, so app.name is an attribute this estate
	// tracks. The second runs the same kind of thing and nobody recorded it.
	known := host(t, st, "node-known")
	assert(known, "app.name", "alpha", doc)
	assert(known, "app.name", "alpha", col)

	quiet := host(t, st, "node-quiet")
	assert(quiet, "app.name", "beta", col)
	assert(quiet, "os.kernel", "6.1.0", col) // nobody documents kernels here

	if _, err := e.Detect(ctx, 0); err != nil {
		t.Fatal(err)
	}
	ds, err := st.Discrepancies(ctx, store.DiscrepancyFilter{
		Disposition: model.DispOpen, Kind: model.DiscUndocumented,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 {
		t.Fatalf("want one undocumented finding, got %d: %+v", len(ds), ds)
	}
	if ds[0].CIID != quiet || ds[0].Attr != "app.name" {
		t.Errorf("raised against the wrong thing: %+v", ds[0])
	}
}
