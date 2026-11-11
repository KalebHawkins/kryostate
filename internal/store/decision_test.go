package store

import (
	"context"
	"testing"
	"time"

	"github.com/KalebHawkins/hearsay/internal/model"
)

// TestKeepingTheDocumentActuallyKeepsIt is the workflow the tool exists for.
// An operator who decides a collector misread something and then watches the
// page go on showing the value they rejected has been told their decision does
// not matter.
func TestKeepingTheDocumentActuallyKeepsIt(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ci := mustHost(t, s, "node-keep")
	col := mustSource(t, s, model.SourceCollector, "ssh")
	doc := mustSource(t, s, model.SourceDocument, "inventory.md")

	assertValue(t, s, ci, "os.version", "RHEL 8.6", doc, time.Now().Add(-time.Hour))
	assertValue(t, s, ci, "os.version", "RHEL 9.2", col, time.Now())

	if got := factFor(t, s, ci, "os.version").Raw; got != "RHEL 9.2" {
		t.Fatalf("before any decision the collector should win, got %q", got)
	}

	if err := s.RaiseDiscrepancy(ctx, model.Discrepancy{
		CIID: ci, Attr: "os.version", Kind: model.DiscValueConflict,
		Claimed: "RHEL 8.6", ClaimedBy: "document",
		Observed: "RHEL 9.2", ObservedBy: "collector",
	}); err != nil {
		t.Fatal(err)
	}
	open, err := s.Discrepancies(ctx, DiscrepancyFilter{Disposition: model.DispOpen}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("want one open disagreement, got %d", len(open))
	}

	if err := s.Dispose(ctx, open[0].ID, model.DispKeepDocument, "kh", "the reader was wrong"); err != nil {
		t.Fatal(err)
	}

	f := factFor(t, s, ci, "os.version")
	if f.Raw != "RHEL 8.6" {
		t.Errorf("after keeping the document the current value is %q", f.Raw)
	}
	if !f.Decided {
		t.Error("the winning value is not marked as decided")
	}
	if f.SourceKind != model.SourceHuman {
		t.Errorf("the decision is attributed to %s", f.SourceKind)
	}
}

// TestADecisionIsScopedToItsInstance keeps a decision about one instance from
// settling a question about another.
func TestADecisionIsScopedToItsInstance(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ci := mustHost(t, s, "node-scope")

	one := model.Discrepancy{
		CIID: ci, Attr: "app.version", Scope: "instance-a", Kind: model.DiscValueConflict,
		Claimed: "4.2.1", Observed: "5.0.0",
	}
	two := one
	two.Scope = "instance-b"
	if err := s.RaiseDiscrepancy(ctx, one); err != nil {
		t.Fatal(err)
	}
	if err := s.RaiseDiscrepancy(ctx, two); err != nil {
		t.Fatal(err)
	}

	open, err := s.Discrepancies(ctx, DiscrepancyFilter{Disposition: model.DispOpen}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 {
		t.Fatalf("two instances disagreeing identically collapsed into %d finding(s)", len(open))
	}
}

// TestALapsedFindingIsNotAnAcceptedOne keeps the detector's bookkeeping out of
// the record of what people decided, and reopens a finding that comes back.
func TestALapsedFindingIsNotAnAcceptedOne(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ci := mustHost(t, s, "node-lapse")

	d := model.Discrepancy{
		CIID: ci, Attr: "os.version", Kind: model.DiscValueConflict,
		Claimed: "8.6", Observed: "9.2", DetectedAt: time.Now().Add(-time.Hour),
	}
	if err := s.RaiseDiscrepancy(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveStale(ctx, model.DiscValueConflict, time.Now()); err != nil {
		t.Fatal(err)
	}

	lapsed, err := s.Discrepancies(ctx, DiscrepancyFilter{Disposition: model.DispLapsed}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(lapsed) != 1 {
		t.Fatalf("want one lapsed finding, got %d", len(lapsed))
	}
	if accepted, _ := s.Discrepancies(ctx, DiscrepancyFilter{Disposition: model.DispAccepted}, 0); len(accepted) != 0 {
		t.Error("the detector wrote a disposition that reads as a person's decision")
	}

	d.DetectedAt = time.Now()
	if err := s.RaiseDiscrepancy(ctx, d); err != nil {
		t.Fatal(err)
	}
	open, err := s.Discrepancies(ctx, DiscrepancyFilter{Disposition: model.DispOpen}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Errorf("a finding that came back was not reopened: %d open", len(open))
	}
}

// TestAHumanDecisionIsNotReopened is the other half: a person's decision
// survives re-detection of the same facts.
func TestAHumanDecisionIsNotReopened(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ci := mustHost(t, s, "node-stands")

	d := model.Discrepancy{
		CIID: ci, Attr: "role", Kind: model.DiscValueConflict,
		Claimed: "web", Observed: "app",
	}
	if err := s.RaiseDiscrepancy(ctx, d); err != nil {
		t.Fatal(err)
	}
	open, _ := s.Discrepancies(ctx, DiscrepancyFilter{Disposition: model.DispOpen}, 0)
	if err := s.Dispose(ctx, open[0].ID, model.DispAccepted, "kh", "known"); err != nil {
		t.Fatal(err)
	}
	if err := s.RaiseDiscrepancy(ctx, d); err != nil {
		t.Fatal(err)
	}
	again, err := s.Discrepancies(ctx, DiscrepancyFilter{Disposition: model.DispAccepted}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 {
		t.Errorf("re-detecting the same facts undid a decision: %d still accepted", len(again))
	}
}

// TestConfirmingAValueKeepsItFresh covers the case that would otherwise fill
// the stale queue with hosts that are collected every night and never change.
func TestConfirmingAValueKeepsItFresh(t *testing.T) {
	s := testStore(t)
	ci := mustHost(t, s, "node-fresh")
	col := mustSource(t, s, model.SourceCollector, "ssh")

	old := time.Now().Add(-30 * 24 * time.Hour)
	assertValue(t, s, ci, "os.version", "RHEL 8.6", col, old)
	assertValue(t, s, ci, "os.version", "RHEL 8.6", col, time.Now())

	f := factFor(t, s, ci, "os.version")
	if age := f.Age(time.Now()); age > time.Hour {
		t.Errorf("a value confirmed just now is reported as %s old", age.Round(time.Hour))
	}
	if f.ObservedAt.After(old.Add(time.Minute)) {
		t.Error("the original claim time was overwritten by a confirmation")
	}
}

// TestTimestampsSortChronologically guards the string comparison every
// resolution query depends on.
func TestTimestampsSortChronologically(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	whole := ts(base)
	frac := ts(base.Add(500 * time.Millisecond))
	if !(whole < frac) {
		t.Errorf("a whole second (%s) does not sort before a fraction of it (%s)", whole, frac)
	}
	if got := parseTS(whole); !got.Equal(base) {
		t.Errorf("round trip: got %s, want %s", got, base)
	}
	// Rows written by an earlier build are still readable.
	if got := parseTS(base.Format(time.RFC3339Nano)); !got.Equal(base) {
		t.Errorf("an older timestamp no longer parses: %s", got)
	}
}

// TestChartCountsEachHostOnce covers a host running two instances: it holds
// two values for one attribute and is still one host.
func TestChartCountsEachHostOnce(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	col := mustSource(t, s, model.SourceCollector, "ssh")

	two := mustHost(t, s, "node-two-instances")
	if _, err := s.Assert(ctx, model.Assertion{
		CIID: two, Attr: "app.version", Raw: "1.0", Scope: "a", SourceID: col, ObservedAt: time.Now(),
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Assert(ctx, model.Assertion{
		CIID: two, Attr: "app.version", Raw: "2.0", Scope: "b", SourceID: col, ObservedAt: time.Now(),
	}, nil); err != nil {
		t.Fatal(err)
	}
	mustHost(t, s, "node-silent")

	buckets, err := s.Distribution(ctx, "app.version")
	if err != nil {
		t.Fatal(err)
	}
	sum := 0
	for _, b := range buckets {
		sum += b.Count
	}
	if sum != 2 {
		t.Errorf("two hosts charted as %d: %v", sum, buckets)
	}
}

// TestRetiredHostsAreNotCharted keeps the bars and the denominator counting
// the same population.
func TestRetiredHostsAreNotCharted(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	col := mustSource(t, s, model.SourceCollector, "ssh")

	live := mustHost(t, s, "node-live")
	gone := mustHost(t, s, "node-gone")
	assertValue(t, s, live, "os.version", "RHEL 8.6", col, time.Now())
	assertValue(t, s, gone, "os.version", "RHEL 7.9", col, time.Now())
	if err := s.Retire(ctx, gone, "kh", "decommissioned"); err != nil {
		t.Fatal(err)
	}

	buckets, err := s.Distribution(ctx, "os.version")
	if err != nil {
		t.Fatal(err)
	}
	sum := 0
	for _, b := range buckets {
		sum += b.Count
		if b.Key == "RHEL 7.9" {
			t.Error("a retired host is still on the chart")
		}
	}
	if sum != 1 {
		t.Errorf("one live host charted as %d: %v", sum, buckets)
	}
}
