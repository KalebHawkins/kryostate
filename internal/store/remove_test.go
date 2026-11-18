package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
)

// seedHost gives a host a row in every table that holds something about a
// host, so the removal tests can see what each path keeps.
func seedHost(t *testing.T, s *Store, label string) string {
	t.Helper()
	ctx := context.Background()
	id := mustHost(t, s, label)
	src := mustSource(t, s, model.SourceCollector, "ssh")
	now := time.Now()
	aid := assertValue(t, s, id, "os.version", "9.4", src, now)
	stack, err := s.EnsureStack(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		s.AddToStack(ctx, stack, id, "app"),
		s.PutConnProfile(ctx, ConnProfile{CIID: id, Transport: "ssh", Address: label, Port: 22,
			Username: "ops", CredentialRef: "agent:", HostKey: "ssh-ed25519 AAAA", Enabled: true}),
		s.PutCheck(ctx, Check{CIID: id, Name: "web", Kind: "tcp", Target: label + ":80", Timeout: time.Second, Enabled: true}),
		s.PutHostBackoff(ctx, HostBackoff{CIID: id, Fails: 3, Until: now.Add(time.Hour)}),
		s.RecordProbe(ctx, model.Probe{CIID: id, Kind: model.ProbeCollectorRun, Outcome: model.OutcomeOK, At: now}),
		s.RecordEvidence(ctx, Evidence{CIID: id, SpecID: "linux.os.version", Attr: "os.version",
			Output: "9.4\n", AssertionID: aid, At: now}),
		s.RecordReadFailure(ctx, ReadFailure{CIID: id, Attr: "cpu.cores", SourceID: src, Error: "x", Since: now, LastAt: now}),
		s.RecordAddress(ctx, id, "10.0.0.1", "ipv4", "", now),
		s.Audit(ctx, "kim", "set application", id, "ledger"),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// rowsFor counts the rows naming a CI in every table with a ci_id column,
// found from the schema so a table added later is covered without editing
// this test.
func rowsFor(t *testing.T, s *Store, id string) map[string]int {
	t.Helper()
	ctx := context.Background()
	rows, err := s.db.QueryContext(ctx, `SELECT m.name FROM sqlite_master m
		JOIN pragma_table_info(m.name) c ON c.name='ci_id' WHERE m.type='table'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	rows.Close()
	out := map[string]int{}
	for _, tb := range append(tables, "ci") {
		col := "ci_id"
		if tb == "ci" {
			col = "id"
		}
		var n int
		// tb comes from sqlite_master, not from input.
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+tb+` WHERE `+col+`=?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[tb] = n
	}
	var n int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM assertion_norm WHERE assertion_id NOT IN (SELECT id FROM assertion)`).Scan(&n)
	out["assertion_norm orphans"] = n
	return out
}

func TestRemoveHostKeepsHistory(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	id := seedHost(t, s, "web01")
	other := seedHost(t, s, "web02")

	r, err := s.RemoveHost(ctx, id, "kim", "decommissioned")
	if err != nil {
		t.Fatal(err)
	}
	if r.AlreadyRetired || r.Profiles != 1 || r.Memberships != 1 || r.Checks != 1 || r.Backoff != 1 {
		t.Errorf("removal %+v", r)
	}
	got := rowsFor(t, s, id)
	for tb, want := range map[string]int{"conn_profile": 0, "stack_member": 0, "check_def": 0, "host_backoff": 0,
		"ci": 1, "assertion": 1, "probe": 1, "evidence": 1, "read_failure": 1, "identity": 1, "address": 1} {
		if got[tb] != want {
			t.Errorf("%s: %d rows, want %d", tb, got[tb], want)
		}
	}
	c, err := s.CI(ctx, id)
	if err != nil || c.RetiredAt == nil {
		t.Errorf("not retired: %+v %v", c, err)
	}
	if rowsFor(t, s, other)["conn_profile"] != 1 {
		t.Error("another host's connection went too")
	}
	// Removing again is harmless, and it can be brought back.
	if r, err := s.RemoveHost(ctx, id, "kim", ""); err != nil || !r.AlreadyRetired {
		t.Errorf("second removal: %+v %v", r, err)
	}
	if err := s.Unretire(ctx, id, "kim", "mistake"); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE subject=? AND action='remove host'`, id).Scan(&n)
	if n != 2 {
		t.Errorf("%d removal audit rows", n)
	}
	if _, err := s.RemoveHost(ctx, "nope", "kim", ""); !errors.Is(err, ErrNoSuchHost) {
		t.Errorf("unknown host: %v", err)
	}
}

func TestPurgeCILeavesNothing(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	id := seedHost(t, s, "web01")
	other := seedHost(t, s, "web02")

	if err := s.PurgeCI(ctx, id, "kim", "test host"); err != nil {
		t.Fatal(err)
	}
	for tb, n := range rowsFor(t, s, id) {
		if n != 0 {
			t.Errorf("%s: %d rows left", tb, n)
		}
	}
	rows, err := s.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Error("foreign key violations after purge")
	}
	rows.Close()
	var action, detail string
	if err := s.db.QueryRowContext(ctx, `SELECT action, detail FROM audit WHERE subject=?`, id).Scan(&action, &detail); err != nil ||
		action != "purge host" || detail != "web01: test host" {
		t.Errorf("audit: %q %q %v", action, detail, err)
	}
	for tb, n := range rowsFor(t, s, other) {
		if n == 0 && tb != "assertion_norm orphans" {
			t.Errorf("the other host lost its %s rows", tb)
		}
	}
	if _, err := s.LookupCI(ctx, "web01"); !errors.Is(err, ErrNoSuchHost) {
		t.Errorf("purged host still found: %v", err)
	}
	if err := s.PurgeCI(ctx, id, "kim", ""); !errors.Is(err, ErrNoSuchHost) {
		t.Errorf("second purge: %v", err)
	}
}
