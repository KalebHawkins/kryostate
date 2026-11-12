package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
)

// TestAnOlderDatabaseStillOpens: a database written before disagreements were
// removed has their table and a decided column on assertions. It opens, takes
// new values, and a decision recorded back then no longer beats the host.
func TestAnOlderDatabaseStillOpens(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`ALTER TABLE assertion ADD COLUMN decided INTEGER NOT NULL DEFAULT 0`,
		`CREATE TABLE discrepancy (id INTEGER PRIMARY KEY, fingerprint TEXT NOT NULL UNIQUE,
			ci_id TEXT NOT NULL, kind TEXT NOT NULL, detected_at TEXT NOT NULL,
			disposition TEXT NOT NULL DEFAULT 'open')`,
		`CREATE TABLE freshness_budget (attr TEXT PRIMARY KEY, max_age_sec INTEGER NOT NULL)`,
		`INSERT INTO precedence (attr, source_kind, rank, allowed) VALUES ('owner', 'human', 1, 1)`,
	} {
		if _, err := s.DB().ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	ci := mustHost(t, s, "node-old")
	person := mustSource(t, s, model.SourceHuman, "operator")
	decided := assertValue(t, s, ci, "owner", "payments", person, time.Now())
	if _, err := s.DB().ExecContext(ctx, `UPDATE assertion SET decided=1 WHERE id=?`, decided); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatalf("an older database no longer opens: %v", err)
	}
	defer s.Close()
	col := mustSource(t, s, model.SourceCollector, "ssh")
	assertValue(t, s, ci, "owner", "ledger", col, time.Now().Add(-time.Hour))
	if got := factFor(t, s, ci, "owner").Raw; got != "ledger" {
		t.Errorf("owner is %q; an old decision beat what the host reports", got)
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
