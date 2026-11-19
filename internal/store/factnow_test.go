package store

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
)

// projectionDiff lists rows where fact_now and the current_fact view disagree,
// in either direction. The view is the definition; fact_now must be it.
func projectionDiff(t *testing.T, s *Store) []string {
	t.Helper()
	const cols = `assertion_id, ci_id, attr, scope, raw_value, value_type, norm_value, family,
		normalizer_version, source_kind, source_name, observed_at, confirmed_at`
	var out []string
	for _, q := range []struct{ name, sql string }{
		{"only in current_fact", `SELECT ` + cols + ` FROM current_fact EXCEPT SELECT ` + cols + ` FROM fact_now`},
		{"only in fact_now", `SELECT ` + cols + ` FROM fact_now EXCEPT SELECT ` + cols + ` FROM current_fact`},
	} {
		rows, err := s.DB().Query(q.sql)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, nv int64
			var ci, attr, scope, raw, vt, norm, fam, sk, sn, obs, conf string
			if err := rows.Scan(&id, &ci, &attr, &scope, &raw, &vt, &norm, &fam, &nv, &sk, &sn, &obs, &conf); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("%s: #%d %s %s[%s]=%q norm=%q v%d %s/%s obs=%s conf=%s",
				q.name, id, ci, attr, scope, raw, norm, nv, sk, sn, obs, conf))
		}
		rows.Close()
	}
	var a, b int
	if err := s.DB().QueryRow(`SELECT (SELECT COUNT(*) FROM current_fact), (SELECT COUNT(*) FROM fact_now)`).Scan(&a, &b); err != nil {
		t.Fatal(err)
	}
	if a != b {
		out = append(out, fmt.Sprintf("current_fact has %d rows, fact_now %d", a, b))
	}
	return out
}

func checkProjection(t *testing.T, s *Store, step string) {
	t.Helper()
	if d := projectionDiff(t, s); len(d) > 0 {
		t.Fatalf("after %s fact_now differs from current_fact:\n%v", step, d)
	}
}

// TestFactNowFollowsEveryWrite runs the writes that change which claim wins
// and checks after each that fact_now is the current_fact view row for row.
func TestFactNowFollowsEveryWrite(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	col := mustSource(t, s, model.SourceCollector, "ssh")
	col2 := mustSource(t, s, model.SourceCollector, "winrm")
	doc := mustSource(t, s, model.SourceDocument, "inventory.md")
	der := mustSource(t, s, model.SourceDerived, "hostname")
	web := mustHost(t, s, "web01")
	db := mustHost(t, s, "db01")
	base := time.Now().Add(-48 * time.Hour)
	at := func(h int) time.Time { return base.Add(time.Duration(h) * time.Hour) }
	norm := func(v string) *Normalized { return &Normalized{Value: v, Family: "f", Version: 1} }

	steps := []struct {
		name string
		do   func() error
	}{
		{"a first claim", func() error {
			_, err := s.Assert(ctx, model.Assertion{CIID: web, Attr: "os.version", Raw: "RHEL 8.6", SourceID: col, ObservedAt: at(1)}, norm("8.6"))
			return err
		}},
		{"a document claim that loses to the host", func() error {
			_, err := s.Assert(ctx, model.Assertion{CIID: web, Attr: "os.version", Raw: "RHEL 7", SourceID: doc, ObservedAt: at(5)}, nil)
			return err
		}},
		{"a document-only attribute", func() error {
			_, err := s.Assert(ctx, model.Assertion{CIID: db, Attr: "owner", Raw: "payments", SourceID: doc, ObservedAt: at(2)}, nil)
			return err
		}},
		{"a barred derived claim", func() error {
			_, err := s.Assert(ctx, model.Assertion{CIID: db, Attr: "env", Raw: "prod", SourceID: der, ObservedAt: at(2)}, nil)
			return err
		}},
		{"a confirmation", func() error {
			_, err := s.Assert(ctx, model.Assertion{CIID: web, Attr: "os.version", Raw: "RHEL 8.6", SourceID: col, ObservedAt: at(10)}, nil)
			return err
		}},
		{"a replayed confirmation from the past", func() error {
			_, err := s.Assert(ctx, model.Assertion{CIID: web, Attr: "os.version", Raw: "RHEL 8.6", SourceID: col, ObservedAt: at(3)}, nil)
			return err
		}},
		{"a second collector disagreeing", func() error {
			_, err := s.Assert(ctx, model.Assertion{CIID: web, Attr: "os.version", Raw: "RHEL 8.8", SourceID: col2, ObservedAt: at(11)}, norm("8.8"))
			return err
		}},
		{"the first collector confirming and winning back", func() error {
			_, err := s.Assert(ctx, model.Assertion{CIID: web, Attr: "os.version", Raw: "RHEL 8.6", SourceID: col, ObservedAt: at(12)}, nil)
			return err
		}},
		{"scoped values", func() error {
			for i, sc := range []string{"a", "b"} {
				if _, err := s.Assert(ctx, model.Assertion{CIID: web, Attr: "app.version", Scope: sc, Raw: fmt.Sprint(i), SourceID: col, ObservedAt: at(4)}, nil); err != nil {
					return err
				}
			}
			return nil
		}},
		{"a read failure", func() error {
			return s.RecordReadFailure(ctx, ReadFailure{CIID: web, Attr: "os.version", SourceID: col, Error: "exit 1", LastAt: at(13)})
		}},
		{"the read failure extended", func() error {
			return s.RecordReadFailure(ctx, ReadFailure{CIID: web, Attr: "os.version", SourceID: col, Error: "exit 2", LastAt: at(14)})
		}},
		{"a good reading clearing it", func() error {
			_, err := s.Assert(ctx, model.Assertion{CIID: web, Attr: "os.version", Raw: "RHEL 9.0", SourceID: col, ObservedAt: at(15)}, norm("9.0"))
			return err
		}},
		{"a retraction", func() error {
			var id int64
			if err := s.DB().QueryRow(`SELECT id FROM assertion WHERE raw_value='RHEL 9.0'`).Scan(&id); err != nil {
				return err
			}
			return s.RetractAssertion(ctx, id, at(16))
		}},
		{"renormalising", func() error {
			raws, err := s.RawAssertions(ctx, "%")
			if err != nil {
				return err
			}
			for id, r := range raws {
				if r.Attr == "owner" {
					if err := s.ClearNormalized(ctx, id); err != nil {
						return err
					}
					continue
				}
				if err := s.SetNormalized(ctx, id, Normalized{Value: "n:" + r.Value, Version: 2}); err != nil {
					return err
				}
			}
			return nil
		}},
		{"a normalisation cleared", func() error {
			raws, err := s.RawAssertions(ctx, "os.version")
			if err != nil {
				return err
			}
			for id := range raws {
				if err := s.ClearNormalized(ctx, id); err != nil {
					return err
				}
			}
			return nil
		}},
		{"a stated application", func() error { return s.SetApplication(ctx, db, "ledger", "operator", "test") }},
		{"precedence for one attribute", func() error {
			_, err := s.DB().Exec(`INSERT INTO precedence (attr, source_kind, rank, allowed) VALUES ('owner', 'derived', 1, 1)`)
			return err
		}},
		{"precedence barring a source", func() error {
			_, err := s.DB().Exec(`UPDATE precedence SET allowed=0 WHERE attr='owner' AND source_kind='derived'`)
			return err
		}},
		{"the default precedence changed", func() error {
			_, err := s.DB().Exec(`UPDATE precedence SET rank=5 WHERE attr='*' AND source_kind='derived'`)
			return err
		}},
		{"a precedence row deleted", func() error {
			_, err := s.DB().Exec(`DELETE FROM precedence WHERE attr='env'`)
			return err
		}},
		{"a source renamed", func() error {
			_, err := s.DB().Exec(`UPDATE source SET name='inventory-v2.md' WHERE id=?`, doc)
			return err
		}},
		{"a confirmation stamp moved backwards by hand", func() error {
			_, err := s.DB().Exec(`UPDATE assertion SET confirmed_at=observed_at WHERE source_id=?`, col)
			return err
		}},
		{"a host disabled", func() error { _, err := s.DisableHost(ctx, db, "op", "t"); return err }},
		{"a host deleted", func() error { _, err := s.DeleteHost(ctx, web, "op", "t"); return err }},
	}
	for _, st := range steps {
		if err := st.do(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		checkProjection(t, s, st.name)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM fact_now WHERE ci_id=?`, web); n != 0 {
		t.Errorf("a deleted host still has %d current values", n)
	}
}

func countRows(t *testing.T, s *Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestFactNowRandomWrites throws a long random sequence of writes at a small
// key space, where collisions between sources, scopes, failures and
// retractions are frequent, and compares with the view after every one.
func TestFactNowRandomWrites(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	rng := rand.New(rand.NewSource(7))
	srcs := []int64{
		mustSource(t, s, model.SourceCollector, "ssh"),
		mustSource(t, s, model.SourceCollector, "winrm"),
		mustSource(t, s, model.SourceHuman, "operator"),
		mustSource(t, s, model.SourceDocument, "inventory.md"),
		mustSource(t, s, model.SourceDerived, "hostname"),
	}
	var hosts []string
	for i := 0; i < 3; i++ {
		hosts = append(hosts, mustHost(t, s, fmt.Sprintf("node%02d", i)))
	}
	attrs := []string{"os.version", "env", "owner"}
	scopes := []string{"", "", "x"}
	base := time.Now().Add(-1000 * time.Hour)
	for i := 0; i < 600; i++ {
		ci := hosts[rng.Intn(len(hosts))]
		attr, scope := attrs[rng.Intn(len(attrs))], scopes[rng.Intn(len(scopes))]
		src := srcs[rng.Intn(len(srcs))]
		at := base.Add(time.Duration(rng.Intn(1000)) * time.Hour)
		var err error
		op := rng.Intn(10)
		switch op {
		case 0, 1, 2, 3:
			var n *Normalized
			if rng.Intn(2) == 0 {
				n = &Normalized{Value: fmt.Sprint("n", rng.Intn(3)), Version: 1}
			}
			_, err = s.Assert(ctx, model.Assertion{CIID: ci, Attr: attr, Scope: scope, Raw: fmt.Sprint("v", rng.Intn(3)),
				SourceID: src, ObservedAt: at}, n)
		case 4:
			err = s.RecordReadFailure(ctx, ReadFailure{CIID: ci, Attr: attr, Scope: scope, SourceID: src, LastAt: at})
		case 5:
			var id int64
			if s.DB().QueryRow(`SELECT id FROM assertion ORDER BY random() LIMIT 1`).Scan(&id) == nil {
				err = s.RetractAssertion(ctx, id, at)
			}
		case 6:
			var id int64
			if s.DB().QueryRow(`SELECT id FROM assertion ORDER BY random() LIMIT 1`).Scan(&id) == nil {
				if rng.Intn(2) == 0 {
					err = s.ClearNormalized(ctx, id)
				} else {
					err = s.SetNormalized(ctx, id, Normalized{Value: fmt.Sprint("r", rng.Intn(3)), Version: 2})
				}
			}
		case 7:
			kinds := []string{"collector", "human", "document", "derived"}
			attr := []string{"*", attrs[rng.Intn(len(attrs))]}[rng.Intn(2)]
			allowed := 1
			if rng.Intn(4) == 0 {
				allowed = 0
			}
			_, err = s.DB().Exec(`INSERT INTO precedence (attr, source_kind, rank, allowed) VALUES (?,?,?,?)
				ON CONFLICT(attr, source_kind) DO UPDATE SET rank=excluded.rank, allowed=excluded.allowed`,
				attr, kinds[rng.Intn(len(kinds))], rng.Intn(100), allowed)
		case 8:
			_, err = s.DB().Exec(`UPDATE read_failure SET cleared_at=? WHERE id IN (SELECT id FROM read_failure ORDER BY random() LIMIT 1)`, ts(at))
		case 9:
			if rng.Intn(20) == 0 {
				_, err = s.DeleteHost(ctx, ci, "op", "")
				if err == nil {
					hosts = append(hosts, mustHost(t, s, fmt.Sprintf("node%02d", 10+i)))
					for j, h := range hosts {
						if h == ci {
							hosts = append(hosts[:j], hosts[j+1:]...)
							break
						}
					}
				}
			} else {
				_, err = s.DB().Exec(`UPDATE assertion SET confirmed_at=? WHERE id IN (SELECT id FROM assertion ORDER BY random() LIMIT 1)`, ts(at))
			}
		}
		if err != nil {
			t.Fatalf("step %d (op %d): %v", i, op, err)
		}
		checkProjection(t, s, fmt.Sprintf("step %d (op %d)", i, op))
	}
	if countRows(t, s, `SELECT COUNT(*) FROM fact_now`) == 0 {
		t.Fatal("the random sequence left nothing to compare")
	}
}

// TestFactNowBackfilledOnUpgrade: a database written before fact_now existed
// gets it filled from its claims when it is opened, and one whose projection
// was built by other rules gets it rebuilt.
func TestFactNowBackfilledOnUpgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	col := mustSource(t, s, model.SourceCollector, "ssh")
	doc := mustSource(t, s, model.SourceDocument, "inventory.md")
	ci := mustHost(t, s, "web01")
	assertValue(t, s, ci, "os.version", "RHEL 8.6", col, time.Now().Add(-time.Hour))
	assertValue(t, s, ci, "owner", "payments", doc, time.Now())
	// As an older build left it: no projection, no triggers.
	for _, q := range []string{`DROP TABLE fact_now`, `DROP TABLE projection`} {
		if _, err := s.DB().ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	checkProjection(t, s, "opening an older database")
	if n := countRows(t, s, `SELECT COUNT(*) FROM fact_now`); n != 2 {
		t.Fatalf("backfilled %d rows, want 2", n)
	}
	// A projection that went wrong some other way is rebuilt when the rules
	// that keep it change.
	if _, err := s.DB().Exec(`DELETE FROM fact_now; UPDATE projection SET version=0`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	checkProjection(t, s, "reopening with a stale version")
}

// TestAttrsSeeWritesFromAnotherProcess: the attribute counts are kept between
// calls, and a value written through another handle on the same file, as by
// the command line while the web server runs, is still counted at once.
func TestAttrsSeeWritesFromAnotherProcess(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")
	web, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer web.Close()
	cli, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	col := mustSource(t, cli, model.SourceCollector, "ssh")
	a := mustHost(t, cli, "web01")
	b := mustHost(t, cli, "web02")
	assertValue(t, cli, a, "os.version", "RHEL 8", col, time.Now().Add(-time.Hour))

	distinct := func() (int, int) {
		t.Helper()
		attrs, err := web.Attrs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range attrs {
			if x.Attr == "os.version" {
				return x.Distinct, x.CICount
			}
		}
		return 0, 0
	}
	if d, n := distinct(); d != 1 || n != 1 {
		t.Fatalf("got %d values on %d hosts", d, n)
	}
	assertValue(t, cli, b, "os.version", "RHEL 9", col, time.Now())
	if d, n := distinct(); d != 2 || n != 2 {
		t.Errorf("after another process wrote a value: %d values on %d hosts", d, n)
	}
	if _, err := cli.DeleteHost(ctx, b, "op", ""); err != nil {
		t.Fatal(err)
	}
	if d, n := distinct(); d != 1 || n != 1 {
		t.Errorf("after another process deleted a host: %d values on %d hosts", d, n)
	}
}

// windowedChanges is the query Changes used before it read predecessors off
// the series index: LAG over each touched series. It is kept here as the
// definition the faster query must agree with.
const windowedChanges = `
WITH touched AS (
  SELECT DISTINCT a.ci_id, a.attr, a.scope, a.source_id
  FROM assertion a JOIN source s ON s.id=a.source_id
  WHERE s.kind='collector' AND a.retracted_at IS NULL AND a.observed_at >= ?
),
seq AS (
  SELECT a.id, a.ci_id, a.attr, a.scope, a.raw_value, a.observed_at, a.source_id,
         LAG(a.raw_value)   OVER w AS prev_raw,
         LAG(a.observed_at) OVER w AS prev_at
  FROM assertion a
  JOIN touched t ON t.ci_id=a.ci_id AND t.attr=a.attr AND t.scope=a.scope AND t.source_id=a.source_id
  WHERE a.retracted_at IS NULL
  WINDOW w AS (PARTITION BY a.ci_id, a.attr, a.scope, a.source_id ORDER BY a.observed_at, a.id)
)
SELECT q.id, q.prev_raw, q.raw_value, q.prev_at
FROM seq q JOIN ci c ON c.id=q.ci_id
WHERE q.prev_raw IS NOT NULL AND q.prev_raw <> q.raw_value AND q.observed_at >= ?
ORDER BY q.observed_at DESC, q.id DESC`

// TestChangesMatchesTheWindowedDefinition compares Changes with the LAG
// query it replaced over random series with repeated timestamps, several
// sources, retractions and confirmations.
func TestChangesMatchesTheWindowedDefinition(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	rng := rand.New(rand.NewSource(11))
	srcs := []int64{
		mustSource(t, s, model.SourceCollector, "ssh"),
		mustSource(t, s, model.SourceCollector, "winrm"),
		mustSource(t, s, model.SourceDocument, "inventory.md"),
	}
	var hosts []string
	for i := 0; i < 4; i++ {
		hosts = append(hosts, mustHost(t, s, fmt.Sprintf("node%02d", i)))
	}
	base := time.Now().Add(-60 * time.Hour)
	for i := 0; i < 800; i++ {
		at := base.Add(time.Duration(rng.Intn(60)) * time.Hour)
		if _, err := s.Assert(ctx, model.Assertion{CIID: hosts[rng.Intn(len(hosts))],
			Attr: []string{"os.version", "env"}[rng.Intn(2)], Scope: []string{"", "x"}[rng.Intn(2)],
			Raw: fmt.Sprint("v", rng.Intn(3)), SourceID: srcs[rng.Intn(len(srcs))], ObservedAt: at}, nil); err != nil {
			t.Fatal(err)
		}
		if rng.Intn(15) == 0 {
			if _, err := s.DB().Exec(`UPDATE assertion SET retracted_at=? WHERE id IN (SELECT id FROM assertion ORDER BY random() LIMIT 1)`, ts(at)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, back := range []time.Duration{time.Hour, 12 * time.Hour, 30 * time.Hour, 100 * time.Hour} {
		since := time.Now().Add(-back)
		rows, err := s.DB().Query(windowedChanges, ts(since), ts(since))
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for rows.Next() {
			var id int64
			var prev, cur, prevAt string
			if err := rows.Scan(&id, &prev, &cur, &prevAt); err != nil {
				t.Fatal(err)
			}
			want = append(want, fmt.Sprintf("#%d %s->%s %s", id, prev, cur, parseTS(prevAt).UTC()))
		}
		rows.Close()
		cs, err := s.Changes(ctx, ChangeFilter{Since: since, Limit: 100000})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, c := range cs {
			got = append(got, fmt.Sprintf("#%d %s->%s %s", c.AssertionID, c.Old, c.New, c.OldAt.UTC()))
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("since %s: %d changes, the windowed query gives %d\n got %v\nwant %v", back, len(got), len(want), got, want)
		}
		if back == 100*time.Hour && len(want) < 50 {
			t.Fatalf("only %d changes to compare", len(want))
		}
	}
}
