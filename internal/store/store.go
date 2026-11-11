// Package store is the only code that talks SQL. It applies the schema, seeds
// the resolution policy, and exposes queries in terms of the model types.
//
// Assertions are append-only. Nothing in this package updates a value in
// place, because the disagreement between two values is the thing the tool
// exists to show.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/KalebHawkins/hearsay/internal/model"
)

//go:embed schema.sql
var schemaSQL string

// Store wraps a SQLite database. SQLite is a deliberate ceiling: an estate of
// a few thousand hosts fits comfortably, and a single file is restorable by
// anyone without a database administrator.
type Store struct {
	db   *sql.DB
	norm Normalizer
}

// Timestamps are compared lexically in SQL, so the format has to be
// fixed-width. RFC3339Nano drops trailing zeros, which puts a whole second
// after every fraction of it ('Z' > '.') and lets an older assertion win the
// ORDER BY that decides the current value.
const tsLayout = "2006-01-02T15:04:05.000000000Z"

func ts(t time.Time) string { return t.UTC().Format(tsLayout) }

func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(tsLayout, s)
	if err != nil {
		if t2, err2 := time.Parse(time.RFC3339Nano, s); err2 == nil {
			return t2
		}
		return time.Time{}
	}
	return t
}

// Open connects to the database at path, applies the schema and seeds default
// policy. Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		dsn = path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite tolerates one writer. Collection fans out over the network and
	// funnels writes through here, so the pool is held at one connection to
	// keep writes serialised rather than contending and timing out.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for the few callers that need their own transaction.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	// CREATE TABLE IF NOT EXISTS leaves an older database without columns
	// added since it was created, so they are added separately.
	for _, c := range []struct{ table, col, decl string }{
		{"conn_profile", "bastion_host_key", "TEXT NOT NULL DEFAULT ''"},
		{"assertion", "confirmed_at", "TEXT NOT NULL DEFAULT ''"},
		{"assertion", "decided", "INTEGER NOT NULL DEFAULT 0"},
		{"discrepancy", "scope", "TEXT NOT NULL DEFAULT ''"},
		{"evidence", "assertion_id", "INTEGER"},
	} {
		if err := s.ensureColumn(ctx, c.table, c.col, c.decl); err != nil {
			return err
		}
	}
	return s.seedPolicy(ctx)
}

func (s *Store) ensureColumn(ctx context.Context, table, col, decl string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == col {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// table, col and decl are package constants, never caller input.
	_, err = s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col, decl))
	return err
}

// seedPolicy writes the default resolution rules. They are data rather than
// code so an operator can change them without a rebuild, and so the reasoning
// behind each one is inspectable in the database.
func (s *Store) seedPolicy(ctx context.Context) error {
	type row struct {
		attr    string
		kind    model.SourceKind
		rank    int
		allowed bool
	}
	rows := []row{
		// Default order: the machine outranks a person, who outranks a
		// document, which outranks a guess from a hostname.
		{"*", model.SourceCollector, 10, true},
		{"*", model.SourceHuman, 20, true},
		{"*", model.SourceDNS, 30, true},
		{"*", model.SourceDocument, 40, true},
		{"*", model.SourceDerived, 90, true},

		// Environment and site cannot be read off a hostname. Ten hosts named
		// for production can be a test rig, and two plants can produce names
		// that slice identically. A decoder may suggest a role; it may not
		// decide where a machine is or what it is for.
		{"env", model.SourceHuman, 10, true},
		{"env", model.SourceCollector, 20, true},
		{"env", model.SourceDocument, 30, true},
		{"env", model.SourceDerived, 99, false},
		{"site", model.SourceHuman, 10, true},
		{"site", model.SourceDocument, 20, true},
		{"site", model.SourceCollector, 30, true},
		{"site", model.SourceDerived, 99, false},

		// Ownership and purpose are not observable from a machine.
		{"owner", model.SourceHuman, 10, true},
		{"owner", model.SourceDocument, 20, true},
		{"owner", model.SourceCollector, 80, true},
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.PrepareContext(ctx,
		`INSERT INTO precedence (attr, source_kind, rank, allowed) VALUES (?,?,?,?)
		 ON CONFLICT(attr, source_kind) DO NOTHING`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, r := range rows {
		a := 0
		if r.allowed {
			a = 1
		}
		if _, err := st.ExecContext(ctx, r.attr, string(r.kind), r.rank, a); err != nil {
			return err
		}
	}
	budgets := map[string]int{
		"*":          14 * 24 * 3600,
		"os.version": 7 * 24 * 3600,
	}
	for attr, sec := range budgets {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO freshness_budget (attr, max_age_sec) VALUES (?,?)
			 ON CONFLICT(attr) DO NOTHING`, attr, sec); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SourceID returns the id of a source, creating it if new.
func (s *Store) SourceID(ctx context.Context, kind model.SourceKind, name string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM source WHERE kind=? AND name=?`, string(kind), name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO source (kind, name, created_at) VALUES (?,?,?)`,
		string(kind), name, ts(time.Now()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ErrAmbiguousIdentity is returned when identity evidence matches more than
// one CI. Merging on a name collision is how a CMDB quietly fuses two plants,
// so this stops for a person instead.
var ErrAmbiguousIdentity = errors.New("identity matches more than one configuration item")

// ResolveCI finds the CI matching any of the supplied identity evidence, in
// the order given, and creates one if nothing matches. The caller supplies
// evidence strongest-first; hostname should always be last.
func (s *Store) ResolveCI(ctx context.Context, kind model.CIKind, label string, evidence []model.Identity) (string, bool, error) {
	now := time.Now()
	for _, ev := range evidence {
		if ev.Value == "" {
			continue
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT DISTINCT ci_id FROM identity WHERE scheme=? AND value=? AND valid_to IS NULL`,
			string(ev.Scheme), ev.Value)
		if err != nil {
			return "", false, err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return "", false, err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return "", false, err
		}
		switch len(ids) {
		case 0:
			continue
		case 1:
			if err := s.touchIdentity(ctx, ids[0], ev, now); err != nil {
				return "", false, err
			}
			return ids[0], false, nil
		default:
			return "", false, fmt.Errorf("%w: %s=%s matches %d items",
				ErrAmbiguousIdentity, ev.Scheme, ev.Value, len(ids))
		}
	}

	id := newID()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ci (id, kind, label, created_at) VALUES (?,?,?,?)`,
		id, string(kind), label, ts(now)); err != nil {
		return "", false, err
	}
	for _, ev := range evidence {
		if ev.Value == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO identity (ci_id, scheme, value, first_seen, last_seen) VALUES (?,?,?,?,?)`,
			id, string(ev.Scheme), ev.Value, ts(now), ts(now)); err != nil {
			return "", false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	return id, true, nil
}

func (s *Store) touchIdentity(ctx context.Context, ciID string, ev model.Identity, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE identity SET last_seen=? WHERE ci_id=? AND scheme=? AND value=? AND valid_to IS NULL`,
		ts(now), ciID, string(ev.Scheme), ev.Value)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO identity (ci_id, scheme, value, first_seen, last_seen) VALUES (?,?,?,?,?)`,
			ciID, string(ev.Scheme), ev.Value, ts(now), ts(now))
	}
	return err
}

// AddIdentity records further evidence for an existing CI.
func (s *Store) AddIdentity(ctx context.Context, ciID string, scheme model.IdentityScheme, value string) error {
	return s.touchIdentity(ctx, ciID, model.Identity{Scheme: scheme, Value: value}, time.Now())
}

// Normalized is the derived form of a raw value, produced by the normaliser
// and stored beside the assertion it came from.
type Normalized struct {
	Value   string
	Family  string
	Major   string
	Minor   string
	Patch   string
	RuleID  string
	Version int
}

// Assert records a claim. It returns the new assertion id. An identical claim
// from the same source is not re-inserted: repeating an unchanged value every
// night would bury the history that matters under noise.
func (s *Store) Assert(ctx context.Context, a model.Assertion, n *Normalized) (int64, error) {
	if a.CIID == "" || a.Attr == "" {
		return 0, errors.New("assertion needs a ci and an attribute")
	}
	if a.ObservedAt.IsZero() {
		a.ObservedAt = time.Now()
	}
	if a.ValueType == "" {
		a.ValueType = model.TypeText
	}

	var lastRaw string
	var lastID int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, raw_value FROM assertion
		 WHERE ci_id=? AND attr=? AND scope=? AND source_id=? AND retracted_at IS NULL
		 ORDER BY `+freshness+` DESC, observed_at DESC, id DESC LIMIT 1`,
		a.CIID, a.Attr, a.Scope, a.SourceID).Scan(&lastID, &lastRaw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if err == nil && lastRaw == a.Raw {
		// The same source repeating itself is a confirmation, not a new
		// claim. Recording it as one would bury the history that matters,
		// but discarding it entirely would make a value that is collected
		// nightly and never changes look abandoned.
		//
		// The stamp only ever moves forward. Replaying stored output arrives
		// with its original observation time, and assigning that would make a
		// value confirmed this morning look a week stale, handing the
		// attribute to a source that has said nothing since.
		if _, err := s.db.ExecContext(ctx,
			`UPDATE assertion SET confirmed_at=MAX(confirmed_at, ?) WHERE id=?`,
			ts(a.ObservedAt), lastID); err != nil {
			return 0, err
		}
		return lastID, nil
	}

	dec := 0
	if a.Decided {
		dec = 1
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO assertion (ci_id, attr, raw_value, value_type, source_id, scope, run_id, observed_at, received_at, confirmed_at, decided)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		a.CIID, a.Attr, a.Raw, string(a.ValueType), a.SourceID, a.Scope, a.RunID,
		ts(a.ObservedAt), ts(time.Now()), ts(a.ObservedAt), dec)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if n != nil {
		if err := s.SetNormalized(ctx, id, *n); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// SetNormalized writes or replaces the derived form of an assertion. Replacing
// it is safe precisely because the raw value is never touched.
func (s *Store) SetNormalized(ctx context.Context, assertionID int64, n Normalized) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO assertion_norm (assertion_id, norm_value, family, major, minor, patch, rule_id, normalizer_version)
		 VALUES (?,?,?,?,?,?,?,?)
		 ON CONFLICT(assertion_id) DO UPDATE SET
		   norm_value=excluded.norm_value, family=excluded.family, major=excluded.major,
		   minor=excluded.minor, patch=excluded.patch, rule_id=excluded.rule_id,
		   normalizer_version=excluded.normalizer_version`,
		assertionID, n.Value, n.Family, n.Major, n.Minor, n.Patch, n.RuleID, n.Version)
	return err
}

// ClearNormalized removes the derived form of an assertion, for when a rule
// that used to match it no longer does.
func (s *Store) ClearNormalized(ctx context.Context, assertionID int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM assertion_norm WHERE assertion_id=?`, assertionID)
	return err
}

// RawAssertions streams every live assertion for an attribute pattern, used to
// re-run the normaliser without re-collecting. attrLike accepts SQL LIKE
// syntax; "%" means all.
func (s *Store) RawAssertions(ctx context.Context, attrLike string) (map[int64]Raw, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, attr, raw_value FROM assertion WHERE attr LIKE ? AND retracted_at IS NULL`, attrLike)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]Raw{}
	for rows.Next() {
		var id int64
		var r Raw
		if err := rows.Scan(&id, &r.Attr, &r.Value); err != nil {
			return nil, err
		}
		out[id] = r
	}
	return out, rows.Err()
}

// Raw is an assertion's attribute and untouched value.
type Raw struct {
	Attr  string
	Value string
}

// Assertion returns one claim by id, retracted or not. Reparse uses it to
// compare a re-derived value against the claim that same command output
// produced last time, rather than against whichever claim currently wins.
func (s *Store) Assertion(ctx context.Context, id int64) (model.Assertion, error) {
	var a model.Assertion
	var vt, observed, received string
	var scope, runID sql.NullString
	var retracted sql.NullString
	var decided int
	err := s.db.QueryRowContext(ctx,
		`SELECT id, ci_id, attr, raw_value, value_type, source_id, scope, run_id,
		        observed_at, received_at, decided, retracted_at
		 FROM assertion WHERE id=?`, id).
		Scan(&a.ID, &a.CIID, &a.Attr, &a.Raw, &vt, &a.SourceID, &scope, &runID,
			&observed, &received, &decided, &retracted)
	if err != nil {
		return model.Assertion{}, err
	}
	a.ValueType = model.ValueType(vt)
	a.Scope = scope.String
	a.RunID = runID.String
	a.ObservedAt = parseTS(observed)
	a.ReceivedAt = parseTS(received)
	a.Decided = decided != 0
	if retracted.Valid && retracted.String != "" {
		t := parseTS(retracted.String)
		a.Retracted = &t
	}
	return a, nil
}

// RetractAssertion withdraws a claim without deleting it.
//
// This is for a derivation that turned out to be wrong, not for a value that
// changed. A host whose version genuinely moved gets a second assertion and
// keeps the first; a value this tool extracted incorrectly in the first place
// was never the source's claim, and leaving it to compete with the corrected
// reading would show a disagreement between one source and itself.
func (s *Store) RetractAssertion(ctx context.Context, id int64, at time.Time) error {
	if at.IsZero() {
		at = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE assertion SET retracted_at=? WHERE id=? AND retracted_at IS NULL`, ts(at), id)
	return err
}

// Facts returns the resolved current view for one CI.
func (s *Store) Facts(ctx context.Context, ciID string) ([]model.Fact, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT assertion_id, ci_id, attr, scope, raw_value, value_type, norm_value,
		        source_kind, source_name, observed_at, confirmed_at, decided, normalizer_version
		 FROM current_fact WHERE ci_id=? ORDER BY attr, scope`, ciID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Fact
	for rows.Next() {
		f, err := scanFact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.markContested(ctx, out)
}

type scanner interface{ Scan(...any) error }

func scanFact(rows scanner) (model.Fact, error) {
	var f model.Fact
	var vt, sk, obs, conf string
	var decided int
	if err := rows.Scan(&f.AssertionID, &f.CIID, &f.Attr, &f.Scope, &f.Raw, &vt,
		&f.Norm, &sk, &f.SourceName, &obs, &conf, &decided, &f.Normalizer); err != nil {
		return f, err
	}
	f.ValueType = model.ValueType(vt)
	f.SourceKind = model.SourceKind(sk)
	f.ObservedAt = parseTS(obs)
	f.ConfirmedAt = parseTS(conf)
	if f.ConfirmedAt.IsZero() {
		f.ConfirmedAt = f.ObservedAt
	}
	f.Decided = decided == 1
	return f, nil
}

// markContested flags facts where another source's current claim differs.
//
// Only each source's latest claim counts. A collector that read 7.9 last year
// and 8.6 today has not contradicted anyone: that is an upgrade, and treating
// history as a live disagreement would fill the queue with every change the
// estate has ever made.
func (s *Store) markContested(ctx context.Context, facts []model.Fact) ([]model.Fact, error) {
	for i, f := range facts {
		var n int
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM (
			   SELECT raw_value, ROW_NUMBER() OVER (
			     PARTITION BY source_id ORDER BY `+freshness+` DESC, observed_at DESC, assertion_id DESC) AS sn
			   FROM ranked_assertion WHERE ci_id=? AND attr=? AND scope=?
			 ) WHERE sn=1 AND raw_value<>?`,
			f.CIID, f.Attr, f.Scope, f.Raw).Scan(&n)
		if err != nil {
			return nil, err
		}
		facts[i].Contested = n > 0
	}
	return facts, nil
}

// Conflicts returns what each source currently says about one attribute of one
// CI, winner first. Superseded claims from the same source are left out: they
// are history, available through the assertion table, not disagreement.
func (s *Store) Conflicts(ctx context.Context, ciID, attr, scope string) ([]model.Fact, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT assertion_id, ci_id, attr, scope, raw_value, value_type, norm_value,
		        source_kind, source_name, observed_at, confirmed_at, decided, normalizer_version
		 FROM (
		   SELECT *, ROW_NUMBER() OVER (
		     PARTITION BY source_id ORDER BY `+freshness+` DESC, observed_at DESC, assertion_id DESC) AS sn
		   FROM ranked_assertion WHERE ci_id=? AND attr=? AND scope=?
		 ) WHERE sn=1 ORDER BY rn`,
		ciID, attr, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Fact
	for rows.Next() {
		f, err := scanFact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Bucket is one bar of a chart: a value, how many items hold it, and whether
// it represents an absence rather than an answer.
type Bucket struct {
	Key     string
	Label   string
	Count   int
	Unknown bool
}

// Distribution counts configuration items by the normalised value of an
// attribute, including a bucket for every item that has no answer. Items with
// nothing to say are the ones worth chasing, so they are never dropped.
func (s *Store) Distribution(ctx context.Context, attr string) ([]Bucket, error) {
	// Restricted to the same population the total is counted from: a retired
	// host or a CI that is not a host must not appear in a bar and then be
	// missing from the denominator.
	rows, err := s.db.QueryContext(ctx,
		`SELECT f.ci_id, f.scope, f.norm_value, f.raw_value
		 FROM current_fact f JOIN ci c ON c.id=f.ci_id
		 WHERE f.attr=? AND c.kind='host' AND c.retired_at IS NULL`, attr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// A host with several instances holds several values for one attribute.
	// Each host is still counted once, because the chart's denominator is
	// hosts: the host-wide value wins, instances that agree collapse to the
	// value they agree on, and instances that disagree get their own bar
	// rather than being silently attributed to one of them.
	type held struct {
		hostWide string
		scoped   map[string]bool
	}
	byHost := map[string]*held{}
	for rows.Next() {
		var ciID, scope, norm, raw string
		if err := rows.Scan(&ciID, &scope, &norm, &raw); err != nil {
			return nil, err
		}
		h := byHost[ciID]
		if h == nil {
			h = &held{scoped: map[string]bool{}}
			byHost[ciID] = h
		}
		v := norm
		if v == "" {
			v = "\x00raw:" + raw
		}
		if scope == "" {
			h.hostWide = v
			continue
		}
		h.scoped[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	counts := map[string]int{}
	var unnormalised []string
	varies := 0
	answered := 0
	for _, h := range byHost {
		answered++
		v := h.hostWide
		if v == "" {
			switch len(h.scoped) {
			case 0:
				answered--
				continue
			case 1:
				for k := range h.scoped {
					v = k
				}
			default:
				varies++
				continue
			}
		}
		if raw, ok := strings.CutPrefix(v, "\x00raw:"); ok {
			unnormalised = append(unnormalised, raw)
			continue
		}
		counts[v]++
	}

	// An attribute where nothing normalised is free text: a role, a site, an
	// owner. Nobody wrote a rule because none is wanted, and calling every
	// value "not understood" would be this tool blaming the estate for its own
	// lack of an opinion. An attribute where some values normalised and others
	// did not is the real case: a rule exists and these values escaped it,
	// which is a bar nobody can act on and a rule somebody should write.
	freeText := len(counts) == 0
	unparsed := 0
	for _, raw := range unnormalised {
		if freeText {
			counts[raw]++
			continue
		}
		unparsed++
	}

	out := make([]Bucket, 0, len(counts)+4)
	for v, n := range counts {
		out = append(out, Bucket{Key: v, Label: v, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	if varies > 0 {
		out = append(out, Bucket{Key: "\x00varies", Label: "varies by instance", Count: varies})
	}

	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ci WHERE retired_at IS NULL AND kind='host'`).Scan(&total); err != nil {
		return nil, err
	}
	for _, u := range s.unknownBreakdown(ctx, attr, total-answered, unparsed) {
		if u.Count > 0 {
			out = append(out, u)
		}
	}
	return out, nil
}

// unknownBreakdown splits the items with no usable value into the reasons they
// might have none, because each one needs a different response. The three are
// disjoint by construction: a host is counted once.
//
//   - unparsed: the machine answered and no rule understood it. Write a rule.
//   - collection failed: we tried and could not get an answer. Fix access.
//   - never collected: nobody has ever tried. Add a connection profile.
//
// freshness is when a source last stood behind its value: the later of first
// sighting and most recent confirmation. A repeat of an unchanged value
// confirms the existing row rather than inserting a new one, so observed_at
// alone dates the first sighting and would rank a value confirmed this morning
// below one abandoned last week.
//
// Ranking, per-source deduplication and the conflict queries all use this. Two
// of them once used observed_at, which let the winning value and the list of
// competing values disagree about what a single source currently claims —
// showing a source contradicting itself.
const freshness = `MAX(observed_at, COALESCE(NULLIF(confirmed_at, ''), observed_at))`

// lastCollectorRunFailed is true for a host whose most recent collector run
// did not succeed. One row is selected, by id as well as time, because two
// probes can share a timestamp and a comparison against MAX(at) would then
// match both — letting an earlier failure outvote the later success.
//
// The same predicate drives the chart bucket and the filter the bucket links
// to. Written twice they drifted, and a bar counting eleven hosts opened a
// page listing none.
const lastCollectorRunFailed = `EXISTS (
    SELECT 1 FROM probe p
    WHERE p.ci_id = c.id AND p.kind = 'collector_run' AND p.outcome <> 'ok'
      AND p.id = (SELECT q.id FROM probe q
                  WHERE q.ci_id = c.id AND q.kind = 'collector_run'
                  ORDER BY q.at DESC, q.id DESC LIMIT 1))`

func (s *Store) unknownBreakdown(ctx context.Context, attr string, missing, unparsed int) []Bucket {
	failed, never := 0, 0
	if missing > 0 {
		// Only the collector run counts here. A name that fails to resolve is
		// a different finding and has its own discrepancy; folding it in
		// would make "collection failed" mean two things.
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM ci c
			 WHERE c.retired_at IS NULL AND c.kind='host'
			   AND NOT EXISTS (SELECT 1 FROM current_fact f WHERE f.ci_id=c.id AND f.attr=?)
			   AND `+lastCollectorRunFailed,
			attr).Scan(&failed)
		if err != nil {
			failed = 0
		}
		never = missing - failed
		if never < 0 {
			never = 0
		}
	}
	return []Bucket{
		{Key: string(model.UnknownCollectionFailed), Label: "collection failed", Count: failed, Unknown: true},
		{Key: string(model.UnknownUnparsed), Label: "not understood", Count: unparsed, Unknown: true},
		{Key: string(model.UnknownNeverCollected), Label: "never collected", Count: never, Unknown: true},
	}
}

// RecordProbe appends a probe event. Probes are never updated: the row saying
// SSH was refused on Tuesday stays there after Wednesday's success.
func (s *Store) RecordProbe(ctx context.Context, p model.Probe) error {
	if p.At.IsZero() {
		p.At = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO probe (ci_id, kind, outcome, target, detail, at) VALUES (?,?,?,?,?,?)`,
		p.CIID, string(p.Kind), string(p.Outcome), p.Target, p.Detail, ts(p.At))
	return err
}

// Reachability is the latest result of each kind of probe for a CI. It is
// returned as a set rather than a single timestamp because the four questions
// have different answers and merging them hides the interesting case.
type Reachability map[model.ProbeKind]model.Probe

// Reachability returns the most recent probe of each kind for a CI.
func (s *Store) Reachability(ctx context.Context, ciID string) (Reachability, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, outcome, target, detail, at FROM probe p
		 WHERE ci_id=? AND at = (SELECT MAX(at) FROM probe q WHERE q.ci_id=p.ci_id AND q.kind=p.kind)`,
		ciID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := Reachability{}
	for rows.Next() {
		var p model.Probe
		var kind, outcome, at string
		if err := rows.Scan(&kind, &outcome, &p.Target, &p.Detail, &at); err != nil {
			return nil, err
		}
		p.CIID = ciID
		p.Kind = model.ProbeKind(kind)
		p.Outcome = model.Outcome(outcome)
		p.At = parseTS(at)
		out[p.Kind] = p
	}
	return out, rows.Err()
}

// HostRow is a line in the inventory table.
type HostRow struct {
	CI           model.CI
	Values       map[string]string
	Contested    map[string]bool
	LastCollect  time.Time
	CollectOK    bool
	HasCollected bool
	Stacks       []string
	OpenDiscreps int
}

// HostFilter narrows the inventory listing. Every field maps to a URL
// parameter so a filtered view can be pasted into a chat message and open the
// same thing for the next person.
type HostFilter struct {
	Attr      string // attribute to match on
	Value     string // required normalised value of Attr
	Stack     string
	Search    string
	Contested bool
	Unknown   string // attribute that must have no value at all
	// Because narrows Unknown to one of the three causes: never_collected,
	// collection_failed, or unparsed.
	Because  string
	Unparsed string // attribute whose value no rule understood
}

// Hosts returns the inventory rows matching a filter, with the listed
// attributes resolved for each.
func (s *Store) Hosts(ctx context.Context, f HostFilter) ([]HostRow, error) {
	var where []string
	var args []any
	where = append(where, "c.retired_at IS NULL", "c.kind='host'")

	if f.Search != "" {
		where = append(where, `(c.label LIKE ? OR EXISTS (SELECT 1 FROM identity i WHERE i.ci_id=c.id AND i.value LIKE ?))`)
		args = append(args, "%"+f.Search+"%", "%"+f.Search+"%")
	}
	if f.Attr != "" && f.Value != "" {
		// Free-text attributes have no normalised form, and a filter that
		// only looked at norm_value would silently match nothing for them.
		where = append(where, `EXISTS (SELECT 1 FROM current_fact f WHERE f.ci_id=c.id AND f.attr=?
			AND (CASE WHEN f.norm_value<>'' THEN f.norm_value ELSE f.raw_value END)=?)`)
		args = append(args, f.Attr, f.Value)
	}
	if f.Unparsed != "" {
		where = append(where, `EXISTS (SELECT 1 FROM current_fact f WHERE f.ci_id=c.id AND f.attr=?
			AND f.norm_value='')`)
		args = append(args, f.Unparsed)
	}
	if f.Unknown != "" {
		where = append(where, `NOT EXISTS (SELECT 1 FROM current_fact f WHERE f.ci_id=c.id AND f.attr=?)`)
		args = append(args, f.Unknown)
		// The three causes have three different remediations, so the chart
		// links to three different lists rather than one undifferentiated
		// pile of hosts with no value.
		switch model.Unknown(f.Because) {
		case model.UnknownNeverCollected:
			// The complement of the failed predicate, not "no probe at all":
			// a host collected successfully but never asked for this
			// attribute belongs in this bucket too, and the chart counts it
			// there.
			where = append(where, `NOT `+lastCollectorRunFailed)
		case model.UnknownCollectionFailed:
			where = append(where, lastCollectorRunFailed)
		}
	}
	if f.Stack != "" {
		where = append(where, `EXISTS (SELECT 1 FROM stack_member m JOIN stack s ON s.id=m.stack_id
			WHERE m.ci_id=c.id AND s.name=?)`)
		args = append(args, f.Stack)
	}

	q := `SELECT c.id, c.kind, c.label, c.created_at FROM ci c WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY c.label`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []HostRow
	for rows.Next() {
		var r HostRow
		var kind, created string
		if err := rows.Scan(&r.CI.ID, &kind, &r.CI.Label, &created); err != nil {
			return nil, err
		}
		r.CI.Kind = model.CIKind(kind)
		r.CI.CreatedAt = parseTS(created)
		r.Values = map[string]string{}
		r.Contested = map[string]bool{}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		facts, err := s.Facts(ctx, out[i].CI.ID)
		if err != nil {
			return nil, err
		}
		for _, fct := range facts {
			if fct.Scope != "" {
				continue
			}
			v := fct.Norm
			if v == "" {
				v = fct.Raw
			}
			out[i].Values[fct.Attr] = v
			out[i].Contested[fct.Attr] = fct.Contested
		}
		if err := s.fillCollection(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	if f.Contested {
		kept := out[:0]
		for _, r := range out {
			for _, c := range r.Contested {
				if c {
					kept = append(kept, r)
					break
				}
			}
		}
		out = kept
	}
	return out, nil
}

func (s *Store) fillCollection(ctx context.Context, r *HostRow) error {
	var outcome, at string
	err := s.db.QueryRowContext(ctx,
		`SELECT outcome, at FROM probe WHERE ci_id=? AND kind=? ORDER BY at DESC LIMIT 1`,
		r.CI.ID, string(model.ProbeCollectorRun)).Scan(&outcome, &at)
	if err == nil {
		r.HasCollected = true
		r.CollectOK = model.Outcome(outcome) == model.OutcomeOK
		r.LastCollect = parseTS(at)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.name FROM stack s JOIN stack_member m ON m.stack_id=s.id WHERE m.ci_id=? ORDER BY s.name`,
		r.CI.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		r.Stacks = append(r.Stacks, n)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM discrepancy WHERE ci_id=? AND disposition='open'`,
		r.CI.ID).Scan(&r.OpenDiscreps)
}

// DocumentedAttrs returns the attributes some document asserts somewhere in
// the estate, and, per CI, the attribute and scope pairs a document covers.
//
// The pair answers "is this attribute one we track, and did anyone write this
// host's value down". Without the first half every collected attribute would
// be undocumented and the finding would be noise.
func (s *Store) DocumentedAttrs(ctx context.Context) (tracked map[string]bool, byCI map[string]map[string]bool, err error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT a.ci_id, a.attr, a.scope
		 FROM assertion a JOIN source s ON s.id=a.source_id
		 WHERE s.kind='document' AND a.retracted_at IS NULL`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	tracked = map[string]bool{}
	byCI = map[string]map[string]bool{}
	for rows.Next() {
		var ci, attr, scope string
		if err := rows.Scan(&ci, &attr, &scope); err != nil {
			return nil, nil, err
		}
		tracked[attr] = true
		if byCI[ci] == nil {
			byCI[ci] = map[string]bool{}
		}
		byCI[ci][attr+"\x00"+scope] = true
	}
	return tracked, byCI, rows.Err()
}

// Retire marks a CI as gone without deleting it. The history stays: a machine
// that was decommissioned last March still explains why a document from
// February said what it said. Retired hosts are excluded from charts, lists,
// collection and coverage counts.
func (s *Store) Retire(ctx context.Context, id, actor, reason string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE ci SET retired_at=? WHERE id=? AND retired_at IS NULL`, ts(time.Now()), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("retire %s: no such live configuration item", id)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE conn_profile SET enabled=0 WHERE ci_id=?`, id); err != nil {
		return err
	}
	return s.Audit(ctx, actor, "retire", id, reason)
}

// Unretire reverses Retire for the case where a machine was marked gone and
// was not.
func (s *Store) Unretire(ctx context.Context, id, actor, reason string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE ci SET retired_at=NULL WHERE id=? AND retired_at IS NOT NULL`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("unretire %s: no such retired configuration item", id)
	}
	return s.Audit(ctx, actor, "unretire", id, reason)
}

// CI returns one configuration item.
func (s *Store) CI(ctx context.Context, id string) (model.CI, error) {
	var c model.CI
	var kind, created string
	var retired sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, kind, label, created_at, retired_at FROM ci WHERE id=?`, id).
		Scan(&c.ID, &kind, &c.Label, &created, &retired)
	if err != nil {
		return c, err
	}
	c.Kind = model.CIKind(kind)
	c.CreatedAt = parseTS(created)
	if retired.Valid {
		t := parseTS(retired.String)
		c.RetiredAt = &t
	}
	return c, nil
}

// Identities returns the evidence recorded for a CI.
func (s *Store) Identities(ctx context.Context, ciID string) ([]model.Identity, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT scheme, value, first_seen, last_seen FROM identity WHERE ci_id=? AND valid_to IS NULL ORDER BY scheme`,
		ciID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Identity
	for rows.Next() {
		var i model.Identity
		var scheme, fs, ls string
		if err := rows.Scan(&scheme, &i.Value, &fs, &ls); err != nil {
			return nil, err
		}
		i.CIID = ciID
		i.Scheme = model.IdentityScheme(scheme)
		i.FirstSeen, i.LastSeen = parseTS(fs), parseTS(ls)
		out = append(out, i)
	}
	return out, rows.Err()
}

// Attrs lists every attribute present, with the counts used to decide whether
// it can be charted.
type AttrInfo struct {
	Attr      string
	Distinct  int
	CICount   int
	Chartable bool
	Label     string
}

// Attrs reports each attribute and whether grouping by it produces a usable
// chart. An attribute where nearly every host has its own value, such as an
// install path, is excluded by default: a bar chart with four hundred bars of
// height one answers nothing.
func (s *Store) Attrs(ctx context.Context) ([]AttrInfo, error) {
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ci WHERE retired_at IS NULL AND kind='host'`).Scan(&total); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT f.attr,
		        COUNT(DISTINCT CASE WHEN f.norm_value<>'' THEN f.norm_value ELSE f.raw_value END),
		        COUNT(DISTINCT f.ci_id)
		 FROM current_fact f JOIN ci c ON c.id=f.ci_id
		 WHERE c.retired_at IS NULL AND c.kind='host'
		 GROUP BY f.attr ORDER BY f.attr`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AttrInfo
	for rows.Next() {
		var a AttrInfo
		if err := rows.Scan(&a.Attr, &a.Distinct, &a.CICount); err != nil {
			return nil, err
		}
		a.Chartable = chartable(a.Distinct, a.CICount, total)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		var override sql.NullBool
		var label sql.NullString
		err := s.db.QueryRowContext(ctx, `SELECT chartable, label FROM fact_meta WHERE attr=?`, out[i].Attr).
			Scan(&override, &label)
		if err == nil {
			if override.Valid {
				out[i].Chartable = override.Bool
			}
			if label.Valid {
				out[i].Label = label.String
			}
		}
	}
	return out, nil
}

// chartable decides whether grouping by an attribute is informative.
//
// The thresholds are judgements, not discoveries. Two rules do the work: an
// attribute with roughly one value per host is an identifier and a bar chart
// of it is a list with extra steps, and more than forty bars is unreadable at
// any estate size. Between those, a small absolute number of values is worth
// charting even in a small estate, where a ratio against the host count means
// very little.
//
// A single value is charted when some hosts are missing it. "Twenty on this
// release and four hundred and eighty we have never asked" is one bar and one
// hatched bar, and it is the most useful thing on the page. Only when every
// host agrees is the chart genuinely a single bar saying nothing.
func chartable(distinct, ciCount, totalHosts int) bool {
	switch {
	case distinct == 0 || ciCount == 0:
		return false
	case distinct == 1:
		// One host holding one value is not evidence of a shared value; it is
		// a sample of one, and an install path collected from a single
		// machine would otherwise pass as a category.
		return ciCount >= 2 && ciCount < totalHosts
	case distinct > maxBars:
		return false
	case ciCount > 2 && distinct >= ciCount:
		return false
	case distinct <= smallVocabulary:
		return true
	default:
		return float64(distinct)/float64(ciCount) < 0.2
	}
}

const (
	// maxBars is where a chart stops being readable.
	maxBars = 40
	// smallVocabulary is a count of distinct values low enough to be a
	// category rather than an identifier, whatever the estate size.
	smallVocabulary = 12
)

// SetChartable records an operator's override for how an attribute is treated.
func (s *Store) SetChartable(ctx context.Context, attr string, chartable bool) error {
	v := 0
	if chartable {
		v = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO fact_meta (attr, chartable, updated_at) VALUES (?,?,?)
		 ON CONFLICT(attr) DO UPDATE SET chartable=excluded.chartable, updated_at=excluded.updated_at`,
		attr, v, ts(time.Now()))
	return err
}

// Audit appends an audit record.
func (s *Store) Audit(ctx context.Context, actor, action, subject, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit (at, actor, action, subject, detail) VALUES (?,?,?,?,?)`,
		ts(time.Now()), actor, action, subject, detail)
	return err
}
