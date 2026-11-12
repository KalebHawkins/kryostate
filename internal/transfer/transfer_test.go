package transfer

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

func open(t *testing.T, name string) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// populate builds a store that exercises every table a dump carries, so the
// round trip is not proved on a store that happens to be mostly empty.
func populate(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)

	doc, err := s.SourceID(ctx, model.SourceDocument, "inventory.md")
	if err != nil {
		t.Fatal(err)
	}
	col, err := s.SourceID(ctx, model.SourceCollector, "ssh")
	if err != nil {
		t.Fatal(err)
	}

	id, _, err := s.ResolveCI(ctx, model.KindHost, "appnode1", []model.Identity{
		{Scheme: model.IdentHostname, Value: "appnode1"},
		{Scheme: model.IdentFQDN, Value: "appnode1.example.invalid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := s.ResolveCI(ctx, model.KindHost, "dbnode1", []model.Identity{
		{Scheme: model.IdentHostname, Value: "dbnode1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Assert(ctx, model.Assertion{
		CIID: id, Attr: "os.version", Raw: "RHEL 7.9", SourceID: doc, ObservedAt: at,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Assert(ctx, model.Assertion{
		CIID: id, Attr: "os.version", Raw: "Red Hat Enterprise Linux 8.6 (Ootpa)",
		SourceID: col, ObservedAt: at.Add(time.Hour),
	}, &store.Normalized{Value: "RHEL 8.6", Family: "RHEL", Major: "8", Minor: "6", RuleID: "rhel", Version: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Assert(ctx, model.Assertion{
		CIID: other, Attr: "app.version", Scope: "instance-a", Raw: "4.2.1",
		SourceID: col, ObservedAt: at,
	}, nil); err != nil {
		t.Fatal(err)
	}

	if err := s.RecordAddress(ctx, id, "198.51.100.11", "ipv4", "dns", at); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAddress(ctx, id, "198.51.100.12", "ipv4", "dns", at); err != nil {
		t.Fatal(err)
	}
	if err := s.SelectAddress(ctx, id, "198.51.100.11", "operator"); err != nil {
		t.Fatal(err)
	}

	stackID, err := s.PutStack(ctx, model.Stack{Name: "ledger-prod", Description: "example"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddToStack(ctx, stackID, id, "app"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddToStack(ctx, stackID, other, "database"); err != nil {
		t.Fatal(err)
	}

	if err := s.RecordProbe(ctx, model.Probe{
		CIID: id, Kind: model.ProbeCollectorRun, Outcome: model.OutcomeOK, At: at,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProbe(ctx, model.Probe{
		CIID: other, Kind: model.ProbeICMP, Outcome: model.OutcomeTimeout, At: at,
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.Audit(ctx, "test", "populate", "transfer", "fixture"); err != nil {
		t.Fatal(err)
	}
}

func export(t *testing.T, s *store.Store) []byte {
	t.Helper()
	d, err := Export(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := d.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestRoundTripIsByteIdentical is the property that makes an export worth
// keeping: whatever a restore reconstructs must export to the same bytes. A
// column the dump forgets, or a value whose type changes on the way through,
// shows up here as a diff rather than as a silently thinner inventory years
// later.
func TestRoundTripIsByteIdentical(t *testing.T) {
	ctx := context.Background()
	src := open(t, "source.db")
	populate(t, src)

	first := export(t, src)

	d, err := Read(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	dst := open(t, "restored.db")
	if err := Restore(ctx, dst, d); err != nil {
		t.Fatal(err)
	}
	second := export(t, dst)

	if !bytes.Equal(first, second) {
		t.Errorf("a restored store exports differently:\n%s", firstDiff(string(first), string(second)))
	}
}

// TestRoundTripKeepsLosingAssertions guards the point of the schema. The value
// that lost is the audit trail, and an export that only carried resolved
// values would quietly discard it.
func TestRoundTripKeepsLosingAssertions(t *testing.T) {
	ctx := context.Background()
	src := open(t, "source.db")
	populate(t, src)

	d, err := Read(bytes.NewReader(export(t, src)))
	if err != nil {
		t.Fatal(err)
	}
	dst := open(t, "restored.db")
	if err := Restore(ctx, dst, d); err != nil {
		t.Fatal(err)
	}

	hosts, err := dst.Hosts(ctx, store.HostFilter{Search: "appnode1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 {
		t.Fatalf("restored %d hosts named appnode1", len(hosts))
	}
	var history int
	if err := dst.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM assertion WHERE ci_id=? AND attr='os.version'`, hosts[0].CI.ID).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if history != 2 {
		t.Fatalf("restored %d assertions for os.version, want both the claim and the collected value", history)
	}

	addrs, err := dst.Addresses(ctx, hosts[0].CI.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 2 {
		t.Errorf("restored %d addresses, want both", len(addrs))
	}
	selected := 0
	for _, a := range addrs {
		if a.Selected {
			selected++
			if a.SelectedBy == "" {
				t.Error("the restored address choice lost who made it")
			}
		}
	}
	if selected != 1 {
		t.Errorf("%d addresses marked as chosen after restore, want 1", selected)
	}
}

// TestRestoreRefusesAStoreThatHoldsAnything. Merging two inventories changes
// what identity means, and a restore command must not make that call quietly.
func TestRestoreRefusesAStoreThatHoldsAnything(t *testing.T) {
	ctx := context.Background()
	src := open(t, "source.db")
	populate(t, src)
	d, err := Read(bytes.NewReader(export(t, src)))
	if err != nil {
		t.Fatal(err)
	}

	dst := open(t, "occupied.db")
	if _, _, err := dst.ResolveCI(ctx, model.KindHost, "appnode9", []model.Identity{
		{Scheme: model.IdentHostname, Value: "appnode9"},
	}); err != nil {
		t.Fatal(err)
	}
	err = Restore(ctx, dst, d)
	if err == nil {
		t.Fatal("restored over an inventory that already held hosts")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error %q does not tell the operator what to do instead", err)
	}
}

// TestAnOlderDumpStillRestores: a dump taken before disagreements were removed
// carries their table, the staleness budgets and the assertion column that
// recorded decisions. None of it is read any more, and none of it may stop the
// rest of the inventory coming back.
func TestAnOlderDumpStillRestores(t *testing.T) {
	ctx := context.Background()
	src := open(t, "source.db")
	populate(t, src)
	d, err := Read(bytes.NewReader(export(t, src)))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range d.Tables["assertion"] {
		r["decided"] = 0
	}
	d.Tables["discrepancy"] = []Row{{"id": 1, "fingerprint": "x", "kind": "value_conflict"}}
	d.Tables["freshness_budget"] = []Row{{"attr": "*", "max_age_sec": 3600}}

	dst := open(t, "restored.db")
	if err := Restore(ctx, dst, d); err != nil {
		t.Fatal(err)
	}
	if got := len(d.Tables["assertion"]); got == 0 {
		t.Fatal("fixture holds no assertions")
	}
	again, err := Export(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if again.Counts["assertion"] != len(d.Tables["assertion"]) {
		t.Errorf("restored %d assertions of %d", again.Counts["assertion"], len(d.Tables["assertion"]))
	}
}

// TestReadRejectsAnotherFormat keeps an older dump from being half-read.
func TestReadRejectsAnotherFormat(t *testing.T) {
	_, err := Read(strings.NewReader(`{"format": 99, "tables": {}}`))
	if err == nil {
		t.Fatal("accepted a dump written by a different build")
	}
	if !strings.Contains(err.Error(), "format") {
		t.Errorf("error %q does not name the problem", err)
	}
}

// firstDiff reports the first differing line with context, because a failing
// diff of a whole inventory is unreadable.
func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) && i < len(bl); i++ {
		if al[i] != bl[i] {
			return "line " + itoa(i+1) + "\n  before: " + al[i] + "\n   after: " + bl[i]
		}
	}
	return "one dump has " + itoa(len(al)) + " lines, the other " + itoa(len(bl))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
