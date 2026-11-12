package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mkHost(t *testing.T, st *Store, label string) string {
	t.Helper()
	id, _, err := st.ResolveCI(context.Background(), model.KindHost, label,
		[]model.Identity{{Scheme: model.IdentHostname, Value: label}})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestChangesAreCollectedValuesThatMoved: a change is a collected value that
// differs from what the same source collected before it. A first value is
// not a change, a repeat is not a change, a document is not a change, and a
// claim withdrawn as a bad parse is not a change.
func TestChangesAreCollectedValuesThatMoved(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)
	a := mkHost(t, st, "alpha")
	b := mkHost(t, st, "beta")
	col, _ := st.SourceID(ctx, model.SourceCollector, "builtin")
	doc, _ := st.SourceID(ctx, model.SourceDocument, "inventory.md")
	now := time.Now()

	put := func(ci, attr, raw string, src int64, ago time.Duration, spec string) int64 {
		t.Helper()
		id, err := st.Assert(ctx, model.Assertion{CIID: ci, Attr: attr, Raw: raw, SourceID: src,
			SpecID: spec, ObservedAt: now.Add(-ago)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	put(a, "os.version", "9.3", col, 40*24*time.Hour, "linux.os.version")
	put(a, "os.version", "9.4", col, 3*24*time.Hour, "linux.os.version")
	put(a, "os.version", "9.4", col, 2*24*time.Hour, "linux.os.version") // a confirmation
	put(a, "os.version", "8.0", doc, time.Hour, "")                      // a document, not a change
	latest := put(a, "os.version", "9.5", col, 2*time.Hour, "linux.os.version")
	put(b, "os.version", "9.5", col, time.Hour, "linux.os.version") // first value: no change

	all, err := st.Changes(ctx, ChangeFilter{Since: now.Add(-30 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d changes, want 2: %+v", len(all), all)
	}
	if all[0].Old != "9.4" || all[0].New != "9.5" || all[0].Host != "alpha" || all[0].AssertionID != latest {
		t.Errorf("newest change: %+v", all[0])
	}
	if all[0].SpecID != "linux.os.version" || all[0].SourceName != "builtin" {
		t.Errorf("change not attributed: %+v", all[0])
	}
	if all[1].Old != "9.3" || all[1].New != "9.4" {
		t.Errorf("older change: %+v", all[1])
	}

	day, err := st.Changes(ctx, ChangeFilter{Since: now.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(day) != 1 || day[0].New != "9.5" {
		t.Errorf("last 24h: %+v", day)
	}
	// The window is about when the change happened; the value it changed
	// from may be much older.
	if !day[0].OldAt.Before(now.Add(-48 * time.Hour)) {
		t.Errorf("old value's time: %v", day[0].OldAt)
	}

	if none, _ := st.Changes(ctx, ChangeFilter{Since: now.Add(-30 * 24 * time.Hour), CIID: b}); len(none) != 0 {
		t.Errorf("a host's first value counted as a change: %+v", none)
	}

	if err := st.RetractAssertion(ctx, latest, time.Time{}); err != nil {
		t.Fatal(err)
	}
	after, _ := st.Changes(ctx, ChangeFilter{Since: now.Add(-24 * time.Hour)})
	if len(after) != 0 {
		t.Errorf("a retracted claim still shows as a change: %+v", after)
	}
}

func TestSchedulesAndState(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)
	if err := st.PutSchedule(ctx, Schedule{Collector: "builtin.os", Every: 6 * time.Hour}, "kaleb"); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSchedule(ctx, Schedule{Collector: "builtin.os", Every: 24 * time.Hour, At: "02:00"}, "kaleb"); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSchedule(ctx, Schedule{Collector: "catalog.cpu.cores"}, "kaleb"); err != nil {
		t.Fatal(err)
	}
	m, err := st.Schedules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := m["builtin.os"]; got.Every != 24*time.Hour || got.At != "02:00" || got.UpdatedBy != "kaleb" {
		t.Errorf("schedule not replaced: %+v", got)
	}
	if !m["catalog.cpu.cores"].Manual() {
		t.Error("manual schedule not kept")
	}
	if err := st.ResetSchedule(ctx, "builtin.os", "kaleb"); err != nil {
		t.Fatal(err)
	}
	if m, _ = st.Schedules(ctx); len(m) != 1 {
		t.Errorf("reset did not remove the row: %+v", m)
	}

	at := time.Now().Add(-time.Minute)
	if err := st.MarkCollectorsStarted(ctx, []string{"a", "b"}, at, "run1"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkCollectorFinished(ctx, "a", time.Now(), "partial", "1 ok, 1 failed"); err != nil {
		t.Fatal(err)
	}
	s, err := st.CollectorStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s["a"].Outcome != "partial" || s["a"].RunID != "run1" || s["a"].LastStarted.Sub(at).Abs() > time.Millisecond {
		t.Errorf("state a: %+v", s["a"])
	}
	if s["b"].Outcome != "running" {
		t.Errorf("state b: %+v", s["b"])
	}
}

func TestEstateHealthAndFailingHosts(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)
	ok := mkHost(t, st, "ok1")
	bad := mkHost(t, st, "bad1")
	old := mkHost(t, st, "old1")
	mkHost(t, st, "never1")
	for _, id := range []string{ok, bad, old} {
		if err := st.PutConnProfile(ctx, ConnProfile{CIID: id, Transport: "ssh", Address: "x", Port: 22, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	probe := func(id string, out model.Outcome, ago time.Duration, detail string) {
		t.Helper()
		if err := st.RecordProbe(ctx, model.Probe{CIID: id, Kind: model.ProbeCollectorRun, Outcome: out,
			Detail: detail, At: time.Now().Add(-ago)}); err != nil {
			t.Fatal(err)
		}
	}
	probe(ok, model.OutcomeOK, time.Hour, "")
	probe(bad, model.OutcomeOK, 3*time.Hour, "")
	probe(bad, model.OutcomeRefused, time.Hour, "connection refused")
	probe(old, model.OutcomePartial, 10*24*time.Hour, "")

	h, err := st.EstateHealth(ctx, 2*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	want := Health{Hosts: 4, OK: 2, Failing: 1, Never: 1, NoConnection: 1, Stale: 1}
	if h != want {
		t.Errorf("health %+v, want %+v", h, want)
	}
	f, err := st.FailingHosts(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(f) != 1 || f[0].Label != "bad1" || f[0].Detail != "connection refused" || f[0].Outcome != "refused" {
		t.Errorf("failing hosts: %+v", f)
	}
}
