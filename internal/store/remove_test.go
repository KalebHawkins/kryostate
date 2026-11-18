package store

import (
	"context"
	"errors"
	"strings"
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

func TestDisableHostKeepsEverything(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	id := seedHost(t, s, "web01")
	other := seedHost(t, s, "web02")
	before := rowsFor(t, s, id)

	if changed, err := s.DisableHost(ctx, id, "kim", "powered off"); err != nil || !changed {
		t.Fatalf("disable: %v %v", changed, err)
	}
	if changed, err := s.DisableHost(ctx, id, "kim", ""); err != nil || changed {
		t.Errorf("disabling again: %v %v", changed, err)
	}
	after := rowsFor(t, s, id)
	for tb, n := range before {
		if after[tb] != n {
			t.Errorf("%s: %d rows after disabling, %d before", tb, after[tb], n)
		}
	}
	c, err := s.CI(ctx, id)
	if err != nil || c.DisabledAt == nil || c.DisabledBy != "kim" || c.DisabledReason != "powered off" {
		t.Errorf("not disabled: %+v %v", c, err)
	}

	// Listed and queryable, but not contacted and not counted as a problem.
	rows, _ := s.Hosts(ctx, HostFilter{})
	if len(rows) != 2 {
		t.Errorf("%d hosts listed, want both", len(rows))
	}
	if rows, _ := s.Hosts(ctx, HostFilter{Disabled: true}); len(rows) != 1 || rows[0].CI.ID != id || rows[0].CI.DisabledAt == nil {
		t.Errorf("disabled filter: %+v", rows)
	}
	if hs, _ := s.QueryHosts(ctx); len(hs) != 2 || !hs[0].Disabled || hs[1].Disabled {
		t.Errorf("query hosts: %+v", hs)
	}
	if ts, _ := s.CollectTargets(ctx, ""); len(ts) != 1 || ts[0].CIID != other {
		t.Errorf("collection targets: %+v", ts)
	}
	if cs, _ := s.AllChecks(ctx); len(cs) != 1 || cs[0].CIID != other {
		t.Errorf("checks: %+v", cs)
	}
	if h, _ := s.EstateHealth(ctx, time.Hour); h.Hosts != 1 || h.Disabled != 1 {
		t.Errorf("health: %+v", h)
	}
	if rows, _ := s.Hosts(ctx, HostFilter{Health: HealthNoConn}); len(rows) != 0 {
		t.Errorf("a disabled host is listed as a problem: %+v", rows)
	}

	if changed, err := s.EnableHost(ctx, id, "kim", ""); err != nil || !changed {
		t.Fatalf("enable: %v %v", changed, err)
	}
	if ts, _ := s.CollectTargets(ctx, ""); len(ts) != 2 {
		t.Errorf("after enable, %d targets, want 2", len(ts))
	}
	var n int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE subject=? AND action IN ('disable host','enable host')`, id).Scan(&n)
	if n != 2 {
		t.Errorf("%d audit rows, want one for each change", n)
	}
	if _, err := s.DisableHost(ctx, "nope", "kim", ""); !errors.Is(err, ErrNoSuchHost) {
		t.Errorf("unknown host: %v", err)
	}
}

func TestDeleteHostLeavesOnlyTheAuditTrail(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	id := seedHost(t, s, "web01")
	other := seedHost(t, s, "web02")

	if _, err := s.DeleteHost(ctx, id, "kim", "test host"); err != nil {
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
		t.Error("foreign key violations after delete")
	}
	rows.Close()
	// What was recorded before stays, and the deletion names the host.
	var kept int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE subject=? AND action='set application'`, id).Scan(&kept)
	if kept != 1 {
		t.Errorf("the host's earlier audit rows were deleted: %d left", kept)
	}
	var actor, detail string
	if err := s.db.QueryRowContext(ctx, `SELECT actor, detail FROM audit WHERE subject=? AND action='delete host'`, id).Scan(&actor, &detail); err != nil ||
		actor != "kim" || detail != "web01: test host" {
		t.Errorf("audit: %q %q %v", actor, detail, err)
	}
	for tb, n := range rowsFor(t, s, other) {
		if n == 0 && tb != "assertion_norm orphans" {
			t.Errorf("the other host lost its %s rows", tb)
		}
	}
	if _, err := s.LookupCI(ctx, "web01"); !errors.Is(err, ErrNoSuchHost) {
		t.Errorf("deleted host still found: %v", err)
	}
	if _, err := s.DeleteHost(ctx, id, "kim", ""); !errors.Is(err, ErrNoSuchHost) {
		t.Errorf("second delete: %v", err)
	}
}

// TestRetiredHostsBecomeDisabled opens a database written before hosts could
// be disabled, when they were retired instead.
func TestRetiredHostsBecomeDisabled(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	gone := mustHost(t, s, "old01")
	live := mustHost(t, s, "web01")
	for _, q := range []string{
		`ALTER TABLE ci DROP COLUMN disabled_at`,
		`ALTER TABLE ci ADD COLUMN retired_at TEXT`,
		`UPDATE ci SET retired_at='2026-03-01T10:00:00.000000000Z' WHERE label='old01'`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := s.CI(ctx, gone)
	if err != nil || c.DisabledAt == nil || c.DisabledAt.Format("2006-01-02") != "2026-03-01" || !strings.Contains(c.DisabledReason, "retired") {
		t.Errorf("a retired host was not disabled: %+v %v", c, err)
	}
	if c, _ := s.CI(ctx, live); c.DisabledAt != nil {
		t.Error("a live host was disabled")
	}
	if has, _ := s.hasColumn(ctx, "ci", "retired_at"); has {
		t.Error("retired_at was not dropped")
	}
	// Running it again on the migrated database changes nothing.
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
}
