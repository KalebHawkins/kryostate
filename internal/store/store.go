// Package store is the only code that talks SQL. It applies the schema, seeds
// the resolution policy, and exposes queries in terms of the model types.
//
// Assertions are append-only. Nothing in this package updates a value in
// place: the history of what each source said, and when, is kept so that
// changes over time can be shown.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/KalebHawkins/kryostate/internal/model"
)

//go:embed schema.sql
var schemaSQL string

// Store wraps a SQLite database. SQLite is a deliberate ceiling: an estate of
// a few thousand hosts fits comfortably, and a single file is restorable by
// anyone without a database administrator.
type Store struct {
	db   *sql.DB
	norm Normalizer

	// attrCache holds attrCounts' answer for the fact_now generation
	// attrCacheGen. Callers get the slice itself and must not change it.
	cacheMu      sync.Mutex
	attrCache    []AttrInfo
	attrCacheGen int64
	attrCacheOK  bool
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
		{"evidence", "assertion_id", "INTEGER"},
		{"assertion", "spec_id", "TEXT NOT NULL DEFAULT ''"},
		{"run", "trigger", "TEXT NOT NULL DEFAULT ''"},
		{"collector_choice", "target", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := s.ensureColumn(ctx, c.table, c.col, c.decl); err != nil {
			return err
		}
	}
	// Indexes on columns an older database only has once ensureColumn has
	// added them, so they cannot be in the schema text.
	if _, err := s.db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS evidence_assertion_idx ON evidence (assertion_id)`); err != nil {
		return err
	}
	if err := s.migrateDisabled(ctx); err != nil {
		return err
	}
	if err := s.migrateChoiceStacks(ctx); err != nil {
		return err
	}
	if err := s.migrateCoordination(ctx); err != nil {
		return err
	}
	if err := s.migrateNotify(ctx); err != nil {
		return err
	}
	if err := s.seedPolicy(ctx); err != nil {
		return err
	}
	return s.migrateFactNow(ctx)
}

func (s *Store) ensureColumn(ctx context.Context, table, col, decl string) error {
	has, err := s.hasColumn(ctx, table, col)
	if err != nil || has {
		return err
	}
	// table, col and decl are package constants, never caller input.
	_, err = s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col, decl))
	return err
}

func (s *Store) hasColumn(ctx context.Context, table, col string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
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
		// Default order among the sources that are not the machine: a person
		// outranks a document, which outranks a guess from a hostname. What a
		// host reports outranks all of them on every attribute; the view
		// enforces that whatever this table says, so an edited or older table
		// cannot put a person above the machine.
		{"*", model.SourceCollector, 10, true},
		{"*", model.SourceHuman, 20, true},
		{"*", model.SourceDNS, 30, true},
		{"*", model.SourceDocument, 40, true},
		{"*", model.SourceDerived, 90, true},

		// Environment and site cannot be read off a hostname. Ten hosts named
		// for production can be a test rig, and two plants can produce names
		// that slice identically.
		{"env", model.SourceDerived, 99, false},
		{"site", model.SourceDerived, 99, false},
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
	return tx.Commit()
}

// SourceID returns the id of a source, creating it if new.
func (s *Store) SourceID(ctx context.Context, kind model.SourceKind, name string) (int64, error) {
	// INSERT OR IGNORE then SELECT, rather than SELECT then INSERT: hosts are
	// collected in parallel, and on a new database every worker finds the
	// source missing at once. Only one insert can win; the others must read
	// its row rather than fail the host on a UNIQUE constraint.
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO source (kind, name, created_at) VALUES (?,?,?)`,
		string(kind), name, ts(time.Now())); err != nil {
		return 0, err
	}
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM source WHERE kind=? AND name=?`, string(kind), name).Scan(&id)
	return id, err
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
	// A good reading ends any failure to read the same thing, whether or
	// not the value changed.
	if err := s.clearReadFailure(ctx, a.CIID, a.Attr, a.Scope, a.SourceID, a.ObservedAt); err != nil {
		return 0, err
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

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO assertion (ci_id, attr, raw_value, value_type, source_id, scope, run_id, spec_id, observed_at, received_at, confirmed_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		a.CIID, a.Attr, a.Raw, string(a.ValueType), a.SourceID, a.Scope, a.RunID, a.SpecID,
		ts(a.ObservedAt), ts(time.Now()), ts(a.ObservedAt))
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
	err := s.db.QueryRowContext(ctx,
		`SELECT id, ci_id, attr, raw_value, value_type, source_id, scope, run_id,
		        observed_at, received_at, retracted_at
		 FROM assertion WHERE id=?`, id).
		Scan(&a.ID, &a.CIID, &a.Attr, &a.Raw, &vt, &a.SourceID, &scope, &runID,
			&observed, &received, &retracted)
	if err != nil {
		return model.Assertion{}, err
	}
	a.ValueType = model.ValueType(vt)
	a.Scope = scope.String
	a.RunID = runID.String
	a.ObservedAt = parseTS(observed)
	a.ReceivedAt = parseTS(received)
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
		        source_kind, source_name, observed_at, confirmed_at, normalizer_version
		 FROM fact_now WHERE ci_id=? ORDER BY attr, scope`, ciID)
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
	return out, nil
}

type scanner interface{ Scan(...any) error }

func scanFact(rows scanner) (model.Fact, error) {
	var f model.Fact
	var vt, sk, obs, conf string
	if err := rows.Scan(&f.AssertionID, &f.CIID, &f.Attr, &f.Scope, &f.Raw, &vt,
		&f.Norm, &sk, &f.SourceName, &obs, &conf, &f.Normalizer); err != nil {
		return f, err
	}
	f.ValueType = model.ValueType(vt)
	f.SourceKind = model.SourceKind(sk)
	f.ObservedAt = parseTS(obs)
	f.ConfirmedAt = parseTS(conf)
	if f.ConfirmedAt.IsZero() {
		f.ConfirmedAt = f.ObservedAt
	}
	return f, nil
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
	// Restricted to the same population the total is counted from: a CI that
	// is not a host must not appear in a bar and then be missing from the
	// denominator. Disabled hosts are in both: what they last reported is
	// still what is known about them.
	rows, err := s.db.QueryContext(ctx,
		`SELECT f.ci_id, f.scope, f.norm_value, f.raw_value
		 FROM fact_now f JOIN ci c ON c.id=f.ci_id
		 WHERE f.attr=? AND c.kind='host'`, attr)
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
	// owner. Nobody wrote a rule because none is wanted, so every value is a
	// bucket of its own. An attribute where some values normalised and others
	// did not is the real case: a rule exists and these values escaped it,
	// which is a bar nobody can act on and a rule somebody should write.
	//
	// The test is over every row, not over the values that reached counts. A
	// host-wide value shadows a scoped one, and a host whose instances
	// disagree is diverted to "varies", so counts can be empty while a rule
	// exists. FreeTextAttrs asks the same question for the host page, and the
	// two must agree or the same value is a bucket on one page and "not
	// understood" on the other.
	freeText, err := s.freeTextAttr(ctx, attr)
	if err != nil {
		return nil, err
	}
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
		`SELECT COUNT(*) FROM ci WHERE kind='host'`).Scan(&total); err != nil {
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
// might have none, because each one needs a different response. The four are
// disjoint by construction: a host is counted once.
//
//   - unparsed: the machine answered and no rule understood it. Write a rule.
//   - collection failed: we tried and could not get an answer. Fix access.
//   - never collected: nobody has ever tried. Add a connection profile.
//   - host disabled: somebody switched the host off. Nothing to fix.
//
// freshness is when a source last stood behind its value: the later of first
// sighting and most recent confirmation. A repeat of an unchanged value
// confirms the existing row rather than inserting a new one, so observed_at
// alone dates the first sighting and would rank a value confirmed this morning
// below one abandoned last week.
//
// Ranking and the repeat check in Assert must both use this. If one of them
// ranks on observed_at instead, the two can disagree about what a single
// source currently claims.
const freshness = `MAX(observed_at, COALESCE(NULLIF(confirmed_at, ''), observed_at))`

// lastCollectorRunFailed is true for a host whose most recent collector run
// did not succeed. One row is selected, by id as well as time, because two
// probes can share a timestamp and a comparison against MAX(at) would then
// match both — letting an earlier failure outvote the later success.
//
// The same predicate drives the chart bucket and the filter the bucket links
// to. Keep them on this one constant: two copies can diverge, and a bar that
// counts hosts the linked page does not list is worse than no bar.
const lastCollectorRunFailed = `COALESCE((
    SELECT q.outcome FROM probe q
    WHERE q.ci_id = c.id AND q.kind = 'collector_run'
    ORDER BY q.at DESC, q.id DESC LIMIT 1) <> 'ok', 0)`

func (s *Store) unknownBreakdown(ctx context.Context, attr string, missing, unparsed int) []Bucket {
	failed, never, disabled := 0, 0, 0
	if missing > 0 {
		// Only the collector run counts here. A name that fails to resolve is
		// a different finding with its own probe; folding it in
		// would make "collection failed" mean two things.
		err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(CASE WHEN c.disabled_at IS NULL AND `+lastCollectorRunFailed+` THEN 1 ELSE 0 END), 0),
			        COALESCE(SUM(CASE WHEN c.disabled_at IS NOT NULL THEN 1 ELSE 0 END), 0)
			 FROM ci c
			 WHERE c.kind='host'
			   AND NOT EXISTS (SELECT 1 FROM fact_now f WHERE f.ci_id=c.id AND f.attr=?)`,
			attr).Scan(&failed, &disabled)
		if err != nil {
			failed, disabled = 0, 0
		}
		never = missing - failed - disabled
		if never < 0 {
			never = 0
		}
	}
	return []Bucket{
		{Key: string(model.UnknownCollectionFailed), Label: "collection failed", Count: failed, Unknown: true},
		{Key: string(model.UnknownUnparsed), Label: "not understood", Count: unparsed, Unknown: true},
		{Key: string(model.UnknownNeverCollected), Label: "never collected", Count: never, Unknown: true},
		{Key: string(model.UnknownHostDisabled), Label: "host disabled", Count: disabled, Unknown: true},
	}
}

// RecordProbe appends a probe event. Probes are never updated: a row saying
// SSH was refused stays there after the next success.
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
	LastCollect  time.Time
	CollectOK    bool
	HasCollected bool
	Stacks       []string
}

// HostFilter narrows the inventory listing. Every field maps to a URL
// parameter so a filtered view can be pasted into a chat message and open the
// same thing for the next person.
type HostFilter struct {
	Attr    string // attribute to match on
	Value   string // required normalised value of Attr
	Stack   string
	Search  string
	Unknown string // attribute that must have no value at all
	// Because narrows Unknown to one of the three causes: never_collected,
	// collection_failed, or unparsed.
	Because  string
	Unparsed string // attribute whose value no rule understood
	// Health narrows to one of the overview's problem buckets: failing,
	// stale, never or noconn. StaleBefore is the cutoff for stale; zero uses
	// fourteen days.
	Health      string
	StaleBefore time.Time
	// Disabled lists only the hosts that are disabled, so they can be found
	// together and switched back on.
	Disabled bool
}

// Hosts returns the inventory rows matching a filter, with the listed
// attributes resolved for each.
func (s *Store) Hosts(ctx context.Context, f HostFilter) ([]HostRow, error) {
	rows, err := s.HostList(ctx, f)
	if err != nil {
		return nil, err
	}
	return rows, s.FillHosts(ctx, rows)
}

// HostList returns the hosts matching a filter, in name order, with only
// what the ci table holds: Values is empty and nothing about collection or
// stacks is filled in. FillHosts adds those, so a caller that shows one page
// of a long list fills in only that page.
func (s *Store) HostList(ctx context.Context, f HostFilter) ([]HostRow, error) {
	where := []string{"c.kind='host'"}
	var args []any
	if f.Disabled {
		where = append(where, "c.disabled_at IS NOT NULL")
	}

	if f.Search != "" {
		where = append(where, `(c.label LIKE ? OR EXISTS (SELECT 1 FROM identity i WHERE i.ci_id=c.id AND i.value LIKE ?))`)
		args = append(args, "%"+f.Search+"%", "%"+f.Search+"%")
	}
	if f.Attr != "" && f.Value != "" {
		// Free-text attributes have no normalised form, and a filter that
		// only looked at norm_value would silently match nothing for them.
		where = append(where, `EXISTS (SELECT 1 FROM fact_now f WHERE f.ci_id=c.id AND f.attr=?
			AND (CASE WHEN f.norm_value<>'' THEN f.norm_value ELSE f.raw_value END)=?)`)
		args = append(args, f.Attr, f.Value)
	}
	if f.Unparsed != "" {
		where = append(where, `EXISTS (SELECT 1 FROM fact_now f WHERE f.ci_id=c.id AND f.attr=?
			AND f.norm_value='')`)
		args = append(args, f.Unparsed)
	}
	if f.Unknown != "" {
		where = append(where, `NOT EXISTS (SELECT 1 FROM fact_now f WHERE f.ci_id=c.id AND f.attr=?)`)
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
			where = append(where, `c.disabled_at IS NULL`, `NOT `+lastCollectorRunFailed)
		case model.UnknownCollectionFailed:
			where = append(where, `c.disabled_at IS NULL`, lastCollectorRunFailed)
		case model.UnknownHostDisabled:
			where = append(where, `c.disabled_at IS NOT NULL`)
		}
	}
	if f.Health != "" {
		// The overview's problem buckets count enabled hosts only: a host
		// nobody is meant to contact is not failing or out of date.
		w, a := healthWhere(f.Health, f.StaleBefore)
		where = append(where, `c.disabled_at IS NULL`, w)
		args = append(args, a...)
	}
	if f.Stack != "" {
		where = append(where, `EXISTS (SELECT 1 FROM stack_member m JOIN stack s ON s.id=m.stack_id
			WHERE m.ci_id=c.id AND s.name=?)`)
		args = append(args, f.Stack)
	}

	q := `SELECT c.id, c.kind, c.label, c.created_at, c.disabled_at FROM ci c WHERE ` +
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
		var disabled sql.NullString
		if err := rows.Scan(&r.CI.ID, &kind, &r.CI.Label, &created, &disabled); err != nil {
			return nil, err
		}
		r.CI.Kind = model.CIKind(kind)
		r.CI.CreatedAt = parseTS(created)
		if disabled.Valid {
			t := parseTS(disabled.String)
			r.CI.DisabledAt = &t
		}
		r.Values = map[string]string{}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// FillHosts fills in each row's host-wide values, latest collection and
// stacks, in three statements however many rows there are. The rows must be
// distinct hosts.
func (s *Store) FillHosts(ctx context.Context, rows []HostRow) error {
	if len(rows) == 0 {
		return nil
	}
	at := make(map[string]int, len(rows))
	ids := make([]string, len(rows))
	for i := range rows {
		at[rows[i].CI.ID] = i
		ids[i] = rows[i].CI.ID
		if rows[i].Values == nil {
			rows[i].Values = map[string]string{}
		}
	}
	idJSON, err := json.Marshal(ids)
	if err != nil {
		return err
	}

	// For most of the estate, reading every host-wide value in table order
	// and dropping the hosts not asked for is cheaper than looking up each
	// asked-for host's values one by one.
	var hosts int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ci WHERE kind='host'`).Scan(&hosts); err != nil {
		return err
	}
	const value = `CASE WHEN f.norm_value<>'' THEN f.norm_value ELSE f.raw_value END`
	var vals *sql.Rows
	if len(rows)*4 >= hosts {
		vals, err = s.db.QueryContext(ctx, `SELECT f.ci_id, f.attr, `+value+` FROM fact_now f WHERE f.scope='' `+inTableOrder)
	} else {
		vals, err = s.db.QueryContext(ctx,
			`SELECT f.ci_id, f.attr, `+value+`
			 FROM json_each(?) j JOIN fact_now f ON f.ci_id=j.value AND f.scope=''`, string(idJSON))
	}
	if err != nil {
		return err
	}
	for vals.Next() {
		var id, attr, v string
		if err := vals.Scan(&id, &attr, &v); err != nil {
			vals.Close()
			return err
		}
		if i, ok := at[id]; ok {
			rows[i].Values[attr] = v
		}
	}
	vals.Close()
	if err := vals.Err(); err != nil {
		return err
	}

	// The latest collector run of each host, by the same rule as
	// lastCollectorRunFailed: the newest, and of two at the same moment the
	// one recorded last.
	probes, err := s.db.QueryContext(ctx,
		`SELECT j.value, p.outcome, p.at FROM json_each(?) j
		 JOIN probe p ON p.id = (SELECT q.id FROM probe q WHERE q.ci_id=j.value AND q.kind=?
		                         ORDER BY q.at DESC, q.id DESC LIMIT 1)`,
		string(idJSON), string(model.ProbeCollectorRun))
	if err != nil {
		return err
	}
	for probes.Next() {
		var id, outcome, when string
		if err := probes.Scan(&id, &outcome, &when); err != nil {
			probes.Close()
			return err
		}
		if i, ok := at[id]; ok {
			rows[i].HasCollected = true
			rows[i].CollectOK = model.Outcome(outcome) == model.OutcomeOK
			rows[i].LastCollect = parseTS(when)
		}
	}
	probes.Close()
	if err := probes.Err(); err != nil {
		return err
	}

	stacks, err := s.db.QueryContext(ctx,
		`SELECT m.ci_id, s.name FROM json_each(?) j
		 JOIN stack_member m ON m.ci_id=j.value JOIN stack s ON s.id=m.stack_id
		 ORDER BY s.name`, string(idJSON))
	if err != nil {
		return err
	}
	defer stacks.Close()
	for stacks.Next() {
		var id, name string
		if err := stacks.Scan(&id, &name); err != nil {
			return err
		}
		if i, ok := at[id]; ok {
			rows[i].Stacks = append(rows[i].Stacks, name)
		}
	}
	return stacks.Err()
}

// CI returns one configuration item. For a disabled host it also says who
// disabled it and why, from the audit entry that recorded it.
func (s *Store) CI(ctx context.Context, id string) (model.CI, error) {
	var c model.CI
	var kind, created string
	var disabled sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, kind, label, created_at, disabled_at FROM ci WHERE id=?`, id).
		Scan(&c.ID, &kind, &c.Label, &created, &disabled)
	if err != nil {
		return c, err
	}
	c.Kind = model.CIKind(kind)
	c.CreatedAt = parseTS(created)
	if disabled.Valid {
		t := parseTS(disabled.String)
		c.DisabledAt = &t
		_ = s.db.QueryRowContext(ctx,
			`SELECT actor, detail FROM audit WHERE subject=? AND action='disable host' ORDER BY at DESC, id DESC LIMIT 1`,
			id).Scan(&c.DisabledBy, &c.DisabledReason)
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

// FreeTextAttrs names the attributes for which no value anywhere normalised.
//
// Distribution applies the same rule when it builds a chart: an attribute
// where nothing normalised is free text, such as a role or an owner, and
// nobody wrote a rule because none is wanted. The host page needs the same
// answer, or it labels every role "not understood" while the chart of the same
// attribute shows the roles grouped correctly. Both read freeTextAttr's
// predicate; this one answers for every attribute in one query.
func (s *Store) FreeTextAttrs(ctx context.Context) (map[string]bool, error) {
	// Each attribute is visited once by seeking to the next one in the
	// index, rather than by reading every current value: the two tests
	// below each stop at the first row that settles them.
	rows, err := s.db.QueryContext(ctx,
		`WITH RECURSIVE attrs(attr) AS (
		   SELECT MIN(attr) FROM fact_now
		   UNION ALL
		   SELECT (SELECT MIN(f.attr) FROM fact_now f WHERE f.attr > attrs.attr)
		   FROM attrs WHERE attrs.attr IS NOT NULL
		 )
		 SELECT attr FROM attrs
		 WHERE attr IS NOT NULL
		   AND EXISTS (SELECT 1 FROM fact_now f JOIN ci c ON c.id=f.ci_id
		               WHERE f.attr=attrs.attr AND c.kind='host')
		   AND NOT `+normalisedHostValue)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var attr string
		if err := rows.Scan(&attr); err != nil {
			return nil, err
		}
		out[attr] = true
	}
	return out, rows.Err()
}

// normalisedHostValue is true when some host's current value of attrs.attr
// normalised. It is the one predicate FreeTextAttrs and freeTextAttr share.
// norm_value > ” is norm_value <> ” for text, written so that it reads a
// range of the index.
const normalisedHostValue = `EXISTS (SELECT 1 FROM fact_now f JOIN ci c ON c.id=f.ci_id
	WHERE f.attr=attrs.attr AND f.norm_value > '' AND c.kind='host')`

// freeTextAttr answers FreeTextAttrs' question for one attribute.
//
// The predicate is the same: over live hosts, no value of this attribute
// normalised. Distribution needs the single-attribute form, and the two must
// not drift apart.
func (s *Store) freeTextAttr(ctx context.Context, attr string) (bool, error) {
	var normalised bool
	err := s.db.QueryRowContext(ctx,
		`SELECT `+normalisedHostValue+` FROM (SELECT ? AS attr) attrs`, attr).Scan(&normalised)
	if err != nil {
		return false, err
	}
	return !normalised, nil
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
		`SELECT COUNT(*) FROM ci WHERE kind='host'`).Scan(&total); err != nil {
		return nil, err
	}
	counts, err := s.attrCounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]AttrInfo, len(counts))
	for i, a := range counts {
		a.Chartable = chartable(a.Distinct, a.CICount, total)
		out[i] = a
	}

	type meta struct {
		override sql.NullBool
		label    sql.NullString
	}
	metas := map[string]meta{}
	mrows, err := s.db.QueryContext(ctx, `SELECT attr, chartable, label FROM fact_meta`)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var attr string
		var m meta
		if err := mrows.Scan(&attr, &m.override, &m.label); err != nil {
			return nil, err
		}
		metas[attr] = m
	}
	if err := mrows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		m, ok := metas[out[i].Attr]
		if !ok {
			continue
		}
		if m.override.Valid {
			out[i].Chartable = m.override.Bool
		}
		if m.label.Valid {
			out[i].Label = m.label.String
		}
	}
	return out, nil
}

// inTableOrder makes a read of every current value walk the table itself.
// Left to choose, SQLite walks the narrowest index and looks each row up in
// the table from there, which is several times slower.
const inTableOrder = `ORDER BY f.attr, f.ci_id, f.scope`

// attrCounts is each attribute with how many distinct values and how many
// hosts hold it. Counting means reading every current value, so the answer
// is kept until a current value changes, in this process or another.
func (s *Store) attrCounts(ctx context.Context) ([]AttrInfo, error) {
	gen, genErr := s.factGen(ctx)
	if genErr == nil {
		s.cacheMu.Lock()
		cached, ok := s.attrCache, s.attrCacheOK && s.attrCacheGen == gen
		s.cacheMu.Unlock()
		if ok {
			return cached, nil
		}
	}
	// MATERIALIZED keeps the read in table order; flattened, SQLite counts
	// through an index and looks every value up from there.
	rows, err := s.db.QueryContext(ctx,
		`WITH v AS MATERIALIZED (
		   SELECT f.attr, f.ci_id,
		          CASE WHEN f.norm_value<>'' THEN f.norm_value ELSE f.raw_value END AS value
		   FROM fact_now f JOIN ci c ON c.id=f.ci_id
		   WHERE c.kind='host' `+inTableOrder+`
		 )
		 SELECT attr, COUNT(DISTINCT value), COUNT(DISTINCT ci_id)
		 FROM v GROUP BY attr ORDER BY attr`)
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
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Stored under the generation read before counting: a write in between
	// makes the next call count again rather than trust these.
	if genErr == nil {
		s.cacheMu.Lock()
		s.attrCache, s.attrCacheGen, s.attrCacheOK = out, gen, true
		s.cacheMu.Unlock()
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
