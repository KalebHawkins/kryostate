package engine

import (
	"context"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// osRelease is the sort of output a parse rule gets wrong: the value wanted is
// on one line among several, in quotes, and a rule that reaches for the wrong
// line produces something that looks plausible.
const osRelease = `NAME="Example Linux"
VERSION="9.4 (Codename)"
ID=example
PRETTY_NAME="Example Linux 9.4 (Codename)"
VERSION_ID="9.4"
`

// TestABadParseRuleIsFixedWithoutTouchingAHost is the claim ADR 0003 makes and
// the reason collectors are forbidden from interpreting anything. A rule that
// took the wrong line is corrected against the database, and the host is never
// contacted — which matters most when the host is gone.
func TestABadParseRuleIsFixedWithoutTouchingAHost(t *testing.T) {
	ctx := context.Background()
	e, st := testEngine(t)
	id := host(t, st, "node-reparse")

	// The wrong rule: NAME is the first quoted field in the file, and a rule
	// aimed at it reports the distribution's name where its version belongs.
	wrong := collect.Spec{
		ID: "os.version.example", Attr: "os.version", Type: model.TypeVersion,
		Command: "cat /etc/os-release",
		Parse:   collect.Parse{Kind: collect.ParseKV, Key: "NAME"},
	}
	e.Specs.Specs = append(e.Specs.Specs, wrong)

	if err := st.RecordEvidence(ctx, store.Evidence{
		CIID: id, SpecID: wrong.ID, Attr: wrong.Attr,
		Command: wrong.Command, Output: osRelease, At: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Collection happened once, through the wrong rule.
	srcID, err := st.SourceID(ctx, model.SourceCollector, "builtin")
	if err != nil {
		t.Fatal(err)
	}
	bad, err := collect.Extract(wrong.Parse, osRelease)
	if err != nil {
		t.Fatal(err)
	}
	aid, err := e.assertID(ctx, id, collect.Reading{
		SpecID: wrong.ID, Attr: wrong.Attr, Value: bad, Type: wrong.Type,
		ObservedAt: time.Now(),
	}, srcID, "run-wrong")
	if err != nil {
		t.Fatal(err)
	}
	// Collection links output to the claim it produced; the repair relies on
	// that link to find its own earlier work rather than guessing.
	if err := st.LinkEvidence(ctx, id, wrong.ID, "", aid); err != nil {
		t.Fatal(err)
	}
	if got := factValue(t, st, id, "os.version"); got != "Example Linux" {
		t.Fatalf("setup did not store the wrong value: got %q", got)
	}

	// Somebody notices and corrects the rule. No host is reachable here; the
	// test has no network and no transport, which is the point.
	e.Specs.Specs[len(e.Specs.Specs)-1].Parse = collect.Parse{Kind: collect.ParseKV, Key: "VERSION_ID"}

	sum, err := e.Reparse(ctx, store.EvidenceFilter{Attr: "os.version"})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Considered != 1 || sum.Changed != 1 {
		t.Fatalf("reparse did not correct the value: %+v", sum)
	}
	if got := factValue(t, st, id, "os.version"); got != "9.4" {
		t.Errorf("after reparse os.version is %q, want 9.4", got)
	}

	// The withdrawn reading must not come back as the collector arguing with
	// itself. A disagreement the tool manufactured is worse than none.
	conflicts, err := st.Conflicts(ctx, id, "os.version", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) > 1 {
		t.Errorf("reparse left %d competing values for one attribute: %+v", len(conflicts), conflicts)
	}
}

// TestReparseLeavesCorrectValuesAlone guards against a repair that rewrites
// the whole estate. An assertion per host per run would make the history
// useless for working out when something actually changed.
func TestReparseLeavesCorrectValuesAlone(t *testing.T) {
	ctx := context.Background()
	e, st := testEngine(t)
	id := host(t, st, "node-stable")

	sp := collect.Spec{
		ID: "os.version.example", Attr: "os.version", Type: model.TypeVersion,
		Command: "cat /etc/os-release",
		Parse:   collect.Parse{Kind: collect.ParseKV, Key: "VERSION_ID"},
	}
	e.Specs.Specs = append(e.Specs.Specs, sp)
	if err := st.RecordEvidence(ctx, store.Evidence{
		CIID: id, SpecID: sp.ID, Attr: sp.Attr, Command: sp.Command, Output: osRelease,
	}); err != nil {
		t.Fatal(err)
	}

	first, err := e.Reparse(ctx, store.EvidenceFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Recovered != 1 {
		t.Fatalf("the first reparse should have produced a value from nothing: %+v", first)
	}
	second, err := e.Reparse(ctx, store.EvidenceFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed != 0 || second.Recovered != 0 {
		t.Errorf("reparsing unchanged output asserted something: %+v", second)
	}

	raws, err := st.RawAssertions(ctx, "os.version")
	if err != nil {
		t.Fatal(err)
	}
	if len(raws) != 1 {
		t.Errorf("%d assertions for one unchanged value, want 1", len(raws))
	}
}

// TestEvidenceKeepsOnlyTheLatestOutput checks the retention rule. The history
// of what a host claimed is the assertion table's job; evidence is a repair
// kit, and storing every identical copy of a file would be a slow leak.
func TestEvidenceKeepsOnlyTheLatestOutput(t *testing.T) {
	ctx := context.Background()
	_, st := testEngine(t)
	id := host(t, st, "node-eviction")

	for _, out := range []string{"VERSION_ID=\"9.3\"\n", "VERSION_ID=\"9.4\"\n"} {
		if err := st.RecordEvidence(ctx, store.Evidence{
			CIID: id, SpecID: "s", Attr: "os.version", Output: out,
		}); err != nil {
			t.Fatal(err)
		}
	}
	ev, err := st.AllEvidence(ctx, store.EvidenceFilter{CIID: id})
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 1 {
		t.Fatalf("%d stored outputs for one spec, want 1", len(ev))
	}
	if ev[0].Output != "VERSION_ID=\"9.4\"\n" {
		t.Errorf("the older output survived: %q", ev[0].Output)
	}

	n, err := st.ForgetEvidence(ctx, store.EvidenceFilter{CIID: id})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("forgetting removed %d rows, want 1", n)
	}
}

func factValue(t *testing.T, st *store.Store, ciID, attr string) string {
	t.Helper()
	facts, err := st.Facts(context.Background(), ciID)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range facts {
		if f.Attr == attr {
			return f.Raw
		}
	}
	return ""
}

// TestTruncatedOutputIsNotReplayed covers the case where repair would do
// damage. Output cut off at the size limit may end mid-value, so a rule read
// over it can produce a prefix of the real answer — "9" for "9.4". Replacing a
// correct value with that is worse than leaving the repair undone, so the
// output is skipped and counted where somebody will see it.
func TestTruncatedOutputIsNotReplayed(t *testing.T) {
	ctx := context.Background()
	e, st := testEngine(t)
	id := host(t, st, "node-truncated")

	sp := collect.Spec{
		ID: "os.version.example", Attr: "os.version", Type: model.TypeVersion,
		Command: "cat /etc/os-release",
		Parse:   collect.Parse{Kind: collect.ParseKV, Key: "VERSION_ID"},
	}
	e.Specs.Specs = append(e.Specs.Specs, sp)
	if err := st.RecordEvidence(ctx, store.Evidence{
		CIID: id, SpecID: sp.ID, Attr: sp.Attr, Command: sp.Command,
		Output: "VERSION_ID=\"9", Truncated: true,
	}); err != nil {
		t.Fatal(err)
	}

	sum, err := e.Reparse(ctx, store.EvidenceFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Skipped != 1 || sum.Recovered != 0 || sum.Changed != 0 {
		t.Fatalf("truncated output was replayed: %+v", sum)
	}
	if got := factValue(t, st, id, "os.version"); got != "" {
		t.Errorf("a value was derived from truncated output: %q", got)
	}
}

// TestRepairDoesNotWithdrawAnotherSpecsClaim covers assertions two specs share.
// Identical values from one source are stored once, so the row a repair wants
// to withdraw may also be the only thing holding up a second spec's claim.
// Withdrawing it would delete a value nobody asked to change.
func TestRepairDoesNotWithdrawAnotherSpecsClaim(t *testing.T) {
	ctx := context.Background()
	e, st := testEngine(t)
	id := host(t, st, "node-shared")

	srcID, err := st.SourceID(ctx, model.SourceCollector, "builtin")
	if err != nil {
		t.Fatal(err)
	}
	aid, err := e.assertID(ctx, id, collect.Reading{
		SpecID: "spec-a", Attr: "os.version", Value: "9.4",
		Type: model.TypeVersion, ObservedAt: time.Now(),
	}, srcID, "run-1")
	if err != nil {
		t.Fatal(err)
	}

	// Two specs, both credited with the one assertion their equal values
	// collapsed into.
	for _, spec := range []string{"spec-a", "spec-b"} {
		if err := st.RecordEvidence(ctx, store.Evidence{
			CIID: id, SpecID: spec, Attr: "os.version",
			Output: "VERSION_ID=\"9.4\"\n", AssertionID: aid,
		}); err != nil {
			t.Fatal(err)
		}
	}
	e.Specs.Specs = append(e.Specs.Specs, collect.Spec{
		ID: "spec-a", Attr: "os.version", Type: model.TypeVersion,
		Parse: collect.Parse{Kind: collect.ParseKV, Key: "NAME"},
	})
	if err := st.RecordEvidence(ctx, store.Evidence{
		CIID: id, SpecID: "spec-a", Attr: "os.version",
		Output: osRelease, AssertionID: aid,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := e.Reparse(ctx, store.EvidenceFilter{SpecID: "spec-a"}); err != nil {
		t.Fatal(err)
	}
	shared, err := st.Assertion(ctx, aid)
	if err != nil {
		t.Fatal(err)
	}
	if shared.Retracted != nil {
		t.Error("repairing one spec withdrew the assertion another spec's output also produced, " +
			"taking a claim nobody asked to change with it")
	}
	if got := factValue(t, st, id, "os.version"); got == "" {
		t.Error("the attribute was left with no value at all")
	}
}
