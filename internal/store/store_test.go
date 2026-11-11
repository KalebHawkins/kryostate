package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KalebHawkins/hearsay/internal/model"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustSource(t *testing.T, s *Store, kind model.SourceKind, name string) int64 {
	t.Helper()
	id, err := s.SourceID(context.Background(), kind, name)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustHost(t *testing.T, s *Store, label string) string {
	t.Helper()
	id, _, err := s.ResolveCI(context.Background(), model.KindHost, label,
		[]model.Identity{{Scheme: model.IdentHostname, Value: label}})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func assertValue(t *testing.T, s *Store, ci, attr, raw string, src int64, at time.Time) int64 {
	t.Helper()
	id, err := s.Assert(context.Background(), model.Assertion{
		CIID: ci, Attr: attr, Raw: raw, SourceID: src, ObservedAt: at,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func factFor(t *testing.T, s *Store, ci, attr string) model.Fact {
	t.Helper()
	facts, err := s.Facts(context.Background(), ci)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range facts {
		if f.Attr == attr {
			return f
		}
	}
	t.Fatalf("no fact for %s", attr)
	return model.Fact{}
}

// TestCollectorOutranksDocument is the default the whole tool rests on: what a
// machine reports beats what a document claims about it.
func TestCollectorOutranksDocument(t *testing.T) {
	s := testStore(t)
	ci := mustHost(t, s, "node-a")
	doc := mustSource(t, s, model.SourceDocument, "inventory.md")
	col := mustSource(t, s, model.SourceCollector, "ssh")

	now := time.Now()
	// The document claim is recorded later in wall-clock time deliberately:
	// precedence must not be decided by whichever source ran most recently.
	assertValue(t, s, ci, "os.version", "RHEL 7.9", col, now.Add(-72*time.Hour))
	assertValue(t, s, ci, "os.version", "RHEL 8.6", doc, now)

	f := factFor(t, s, ci, "os.version")
	if f.Raw != "RHEL 7.9" {
		t.Errorf("document won: got %q", f.Raw)
	}
	if f.SourceKind != model.SourceCollector {
		t.Errorf("source kind: got %s", f.SourceKind)
	}
	if !f.Contested {
		t.Error("two sources disagree and the fact is not marked contested")
	}
}

// TestBarredSourceCannotWinUnopposed is the lesson from inventories whose
// environment was decoded from a naming convention: a source barred from an
// attribute must lose even when it is the only claimant, because being the
// only claimant is exactly when a wrong guess does damage.
func TestBarredSourceCannotWinUnopposed(t *testing.T) {
	s := testStore(t)
	ci := mustHost(t, s, "node-b")
	derived := mustSource(t, s, model.SourceDerived, "name decoder")

	assertValue(t, s, ci, "env", "production", derived, time.Now())
	assertValue(t, s, ci, "site", "north", derived, time.Now())
	assertValue(t, s, ci, "role", "web", derived, time.Now())

	facts, err := s.Facts(context.Background(), ci)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range facts {
		if f.Attr == "env" || f.Attr == "site" {
			t.Errorf("a derived claim became the %s of %s: %q", f.Attr, ci, f.Raw)
		}
	}
	// Barred for environment and location, allowed for role: a name may
	// suggest what a machine does, and must not be trusted for where it is or
	// whether it is production.
	if got := factFor(t, s, ci, "role").Raw; got != "web" {
		t.Errorf("role: got %q, want web", got)
	}
}

// TestHumanOutranksCollectorOnJudgement separates measurement from judgement.
// A collector can read an operating system; it cannot know that a machine
// named like production is somebody's test rig.
func TestHumanOutranksCollectorOnJudgement(t *testing.T) {
	s := testStore(t)
	ci := mustHost(t, s, "node-c")
	col := mustSource(t, s, model.SourceCollector, "ssh")
	person := mustSource(t, s, model.SourceHuman, "operator")

	assertValue(t, s, ci, "env", "production", col, time.Now())
	assertValue(t, s, ci, "env", "development", person, time.Now().Add(-time.Hour))

	if got := factFor(t, s, ci, "env").Raw; got != "development" {
		t.Errorf("env: got %q, want development", got)
	}
	if got := factFor(t, s, ci, "env").SourceKind; got != model.SourceHuman {
		t.Errorf("source: got %s", got)
	}
}

// TestConfirmationOutranksAnOlderFirstSighting pins the tie-break between two
// sources of equal rank. A repeat of a value does not create a new assertion,
// it stamps the old one as confirmed, so the row keeps its original
// observed_at. Ordering on observed_at alone therefore made a source that
// stood behind its value an hour ago lose to one that said something else last
// week, which is backwards.
func TestConfirmationOutranksAnOlderFirstSighting(t *testing.T) {
	s := testStore(t)
	ci := mustHost(t, s, "node-confirm")
	a := mustSource(t, s, model.SourceCollector, "ssh")
	b := mustSource(t, s, model.SourceCollector, "agent")

	now := time.Now()
	assertValue(t, s, ci, "os.version", "9.3", a, now.Add(-7*24*time.Hour))
	assertValue(t, s, ci, "os.version", "9.4", b, now.Add(-6*24*time.Hour))
	// The first source sees its original value again today.
	assertValue(t, s, ci, "os.version", "9.3", a, now)

	if got := factFor(t, s, ci, "os.version").Raw; got != "9.3" {
		t.Errorf("the most recently confirmed value is %q, want 9.3", got)
	}
}

// TestConfirmationNeverMovesBackwards covers replay. A reparse re-asserts a
// value with the observation time the output originally had, which can be
// older than the last time the source confirmed it. Assigning that backwards
// would make a value confirmed this morning look stale and hand the attribute
// to a source that has said nothing since.
func TestConfirmationNeverMovesBackwards(t *testing.T) {
	s := testStore(t)
	ci := mustHost(t, s, "node-replay")
	a := mustSource(t, s, model.SourceCollector, "ssh")
	b := mustSource(t, s, model.SourceCollector, "agent")

	now := time.Now()
	assertValue(t, s, ci, "os.version", "9.4", a, now.Add(-10*24*time.Hour))
	assertValue(t, s, ci, "os.version", "9.4", a, now) // tonight's collection
	assertValue(t, s, ci, "os.version", "9.3", b, now.Add(-24*time.Hour))

	// A repair replays output captured a week ago. It is the same value, so
	// it confirms rather than inserts.
	assertValue(t, s, ci, "os.version", "9.4", a, now.Add(-7*24*time.Hour))

	if got := factFor(t, s, ci, "os.version").Raw; got != "9.4" {
		t.Errorf("replaying old output demoted a value confirmed tonight: winner is %q, want 9.4", got)
	}
}

// TestRepeatedValueDoesNotGrowHistory keeps a nightly run that finds nothing
// new from burying the one night something changed.
func TestRepeatedValueDoesNotGrowHistory(t *testing.T) {
	s := testStore(t)
	ci := mustHost(t, s, "node-d")
	col := mustSource(t, s, model.SourceCollector, "ssh")

	first := assertValue(t, s, ci, "os.version", "RHEL 8.6", col, time.Now().Add(-48*time.Hour))
	again := assertValue(t, s, ci, "os.version", "RHEL 8.6", col, time.Now())
	if first != again {
		t.Errorf("an unchanged value created a second assertion (%d then %d)", first, again)
	}

	changed := assertValue(t, s, ci, "os.version", "RHEL 8.9", col, time.Now())
	if changed == first {
		t.Error("a changed value did not create a new assertion")
	}
	raws, err := s.RawAssertions(context.Background(), "os.version")
	if err != nil {
		t.Fatal(err)
	}
	if len(raws) < 2 {
		t.Errorf("history was overwritten: %d assertions retained", len(raws))
	}

	// The superseded value is history, not a live disagreement. One source
	// that changed its mind contradicts nobody.
	live, err := s.Conflicts(context.Background(), ci, "os.version", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Errorf("one source's own history counted as disagreement: %d live claims", len(live))
	}
	if factFor(t, s, ci, "os.version").Contested {
		t.Error("a host that was upgraded is reported as contested")
	}
}

// TestAmbiguousIdentityStops checks the tool refuses to guess which machine it
// is looking at. Merging two records because they share a name is the failure
// that quietly destroys an inventory.
func TestAmbiguousIdentityStops(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	a, _, err := s.ResolveCI(ctx, model.KindHost, "node-e", []model.Identity{
		{Scheme: model.IdentFQDN, Value: "node-e.one.example"},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := s.ResolveCI(ctx, model.KindHost, "node-e", []model.Identity{
		{Scheme: model.IdentFQDN, Value: "node-e.two.example"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two differently qualified names collapsed into one record")
	}

	if err := s.AddIdentity(ctx, a, model.IdentSerial, "SHARED"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddIdentity(ctx, b, model.IdentSerial, "SHARED"); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.ResolveCI(ctx, model.KindHost, "node-e", []model.Identity{
		{Scheme: model.IdentSerial, Value: "SHARED"},
	})
	if !errors.Is(err, ErrAmbiguousIdentity) {
		t.Errorf("evidence matching two records resolved anyway: %v", err)
	}
}

// TestDistributionAlwaysAccountsForEveryHost is the rule that stops a chart
// from flattering the estate: the buckets must add up to the number of hosts,
// and every host without a usable value must appear in a bucket naming why.
func TestDistributionAlwaysAccountsForEveryHost(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	col := mustSource(t, s, model.SourceCollector, "ssh")

	answered := mustHost(t, s, "node-f")
	if _, err := s.Assert(ctx, model.Assertion{
		CIID: answered, Attr: "os.version", Raw: "Red Hat Enterprise Linux 8.6 (Ootpa)",
		SourceID: col, ObservedAt: time.Now(),
	}, &Normalized{Value: "RHEL 8.6", Family: "RHEL", RuleID: "rhel-pretty", Version: 1}); err != nil {
		t.Fatal(err)
	}

	// Answered, but in a form no rule understood. This host has a value and is
	// still not countable, which is a different problem from being offline.
	unparsed := mustHost(t, s, "node-x")
	assertValue(t, s, unparsed, "os.version", "7200-05-04-2114", col, time.Now())

	failed := mustHost(t, s, "node-g")
	if err := s.RecordProbe(ctx, model.Probe{
		CIID: failed, Kind: model.ProbeCollectorRun, Outcome: model.OutcomeTimeout,
	}); err != nil {
		t.Fatal(err)
	}
	mustHost(t, s, "node-h") // never tried

	buckets, err := s.Distribution(ctx, "os.version")
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	byKey := map[string]int{}
	for _, b := range buckets {
		total += b.Count
		byKey[b.Key] = b.Count
		if b.Unknown && b.Label == b.Key {
			t.Errorf("unknown bucket %q has no readable label", b.Key)
		}
	}
	if total != 4 {
		t.Errorf("buckets account for %d hosts, there are 4: %+v", total, buckets)
	}
	if byKey["RHEL 8.6"] != 1 {
		t.Errorf("the one understood value is not its own bucket: %+v", buckets)
	}
	for _, want := range []model.Unknown{
		model.UnknownUnparsed, model.UnknownCollectionFailed, model.UnknownNeverCollected,
	} {
		if byKey[string(want)] != 1 {
			t.Errorf("%s counted %d, want 1", want, byKey[string(want)])
		}
	}
}

// TestDisposeTakesEffect checks that deciding a disagreement changes what the
// inventory says. A decision that only closes a ticket leaves the tool
// reporting the same conflict tomorrow.
func TestDisposeTakesEffect(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ci := mustHost(t, s, "node-i")
	doc := mustSource(t, s, model.SourceDocument, "inventory.md")

	assertValue(t, s, ci, "role", "web", doc, time.Now())
	if err := s.RaiseDiscrepancy(ctx, model.Discrepancy{
		CIID: ci, Attr: "role", Kind: model.DiscValueConflict,
		Claimed: "web", ClaimedBy: "inventory.md",
		Observed: "database", ObservedBy: "ssh",
	}); err != nil {
		t.Fatal(err)
	}
	list, err := s.Discrepancies(ctx, DiscrepancyFilter{CIID: ci}, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("expected one discrepancy, got %d (%v)", len(list), err)
	}
	if err := s.Dispose(ctx, list[0].ID, model.DispConfirmMachine, "operator", "checked by hand"); err != nil {
		t.Fatal(err)
	}
	if got := factFor(t, s, ci, "role").Raw; got != "database" {
		t.Errorf("after confirming the machine, role is still %q", got)
	}
}

// TestRaiseDiscrepancyKeepsDisposition stops a nightly detection pass from
// reopening something a person already decided.
func TestRaiseDiscrepancyKeepsDisposition(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ci := mustHost(t, s, "node-j")
	d := model.Discrepancy{
		CIID: ci, Attr: "os.version", Kind: model.DiscValueConflict,
		Claimed: "RHEL 7.9", ClaimedBy: "inventory.md",
		Observed: "RHEL 8.6", ObservedBy: "ssh",
	}
	if err := s.RaiseDiscrepancy(ctx, d); err != nil {
		t.Fatal(err)
	}
	list, _ := s.Discrepancies(ctx, DiscrepancyFilter{CIID: ci}, 10)
	if err := s.Dispose(ctx, list[0].ID, model.DispAccepted, "operator", "upgrade is scheduled"); err != nil {
		t.Fatal(err)
	}
	if err := s.RaiseDiscrepancy(ctx, d); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Discrepancies(ctx, DiscrepancyFilter{CIID: ci}, 10)
	if len(after) != 1 {
		t.Fatalf("re-detection created a duplicate: %d rows", len(after))
	}
	if after[0].Disposition != model.DispAccepted {
		t.Errorf("a decided disagreement reopened as %s", after[0].Disposition)
	}
}

// TestReachabilityKeepsProbeKindsApart is the reason there is no single "last
// seen" column. A host answering a ping while refusing a login is the signal
// that finds a problem, and averaging the two hides it.
func TestReachabilityKeepsProbeKindsApart(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ci := mustHost(t, s, "node-k")

	now := time.Now()
	for _, p := range []model.Probe{
		{CIID: ci, Kind: model.ProbeICMP, Outcome: model.OutcomeOK, At: now},
		{CIID: ci, Kind: model.ProbeSSHAuth, Outcome: model.OutcomeAuthFail, At: now},
	} {
		if err := s.RecordProbe(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.Reachability(ctx, ci)
	if err != nil {
		t.Fatal(err)
	}
	if r[model.ProbeICMP].Outcome != model.OutcomeOK {
		t.Error("icmp result lost")
	}
	if r[model.ProbeSSHAuth].Outcome != model.OutcomeAuthFail {
		t.Error("ssh failure lost")
	}
}

// TestFreeTextAttributeIsNotCalledUnparsed. Nobody writes a normaliser rule
// for a role or an owner, and reporting every one of them as "not understood"
// would be the tool blaming the estate for its own lack of an opinion.
func TestFreeTextAttributeIsNotCalledUnparsed(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	doc := mustSource(t, s, model.SourceDocument, "inventory.md")

	for label, role := range map[string]string{
		"node-a": "web", "node-b": "web", "node-c": "database",
	} {
		id := mustHost(t, s, label)
		assertValue(t, s, id, "role", role, doc, time.Now())
	}

	buckets, err := s.Distribution(ctx, "role")
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]int{}
	for _, b := range buckets {
		byKey[b.Key] = b.Count
		if b.Unknown && b.Count > 0 {
			t.Errorf("free text was reported as %s", b.Label)
		}
	}
	if byKey["web"] != 2 || byKey["database"] != 1 {
		t.Errorf("roles did not group by their written value: %+v", buckets)
	}

	rows, err := s.Hosts(ctx, HostFilter{Attr: "role", Value: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("filtering on a free-text value found %d hosts, want 2", len(rows))
	}
}

// TestChartableSkipsIdentifiers keeps the charts page from offering a bar
// chart of serial numbers.
func TestChartableSkipsIdentifiers(t *testing.T) {
	cases := []struct {
		distinct, hosts, total int
		want                   bool
	}{
		{1, 100, 100, false},   // one value everywhere says nothing
		{1, 20, 500, true},     // one value and 480 hosts unaccounted for says plenty
		{6, 100, 100, true},    // a handful of releases over a hundred hosts
		{100, 100, 100, false}, // one per host: an identifier, not a category
		{60, 500, 500, false},  // too many bars to read
		{3, 4, 4, true},        // a small estate still has categories in it
		{4, 4, 4, false},       // but one value per host is still an identifier
		{13, 20, 20, false},    // more values than the estate can group by
	}
	for _, c := range cases {
		if got := chartable(c.distinct, c.hosts, c.total); got != c.want {
			t.Errorf("chartable(%d, %d, %d) = %v", c.distinct, c.hosts, c.total, got)
		}
	}
}
