package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/KalebHawkins/kryostate/internal/query"
)

// Rules track a rollout. A rule names the hosts it covers with a scope query
// and what each of them should report with an expect query, such as
// nginx.version >= "1.24". It is evaluated against current values whenever it
// is looked at, so as hosts are changed and collected again they move from
// Not yet to Done by themselves. Nothing about a rule is stated about a host:
// the counts come only from what hosts report.
//
// Both queries are evaluated in Go by package query, never turned into SQL,
// so a rule can do nothing but read.

// Rule is a rollout being tracked.
type Rule struct {
	ID          int64
	Name        string
	Description string
	Owner       string
	// Scope chooses the hosts the rule covers; blank means every host.
	Scope string
	// Expect is what each host in scope should match.
	Expect    string
	CreatedBy string
	Created   time.Time
	Updated   time.Time
}

// ErrRuleNotFound is returned for a rule id or name that does not exist.
var ErrRuleNotFound = errors.New("no such rule")

// RuleQueryError is a rule query that does not parse. Field is "scope" or
// "expect"; Err says where and why.
type RuleQueryError struct {
	Field string
	Err   *query.Error
}

func (e *RuleQueryError) Error() string { return e.Field + ", " + e.Err.Error() }
func (e *RuleQueryError) Unwrap() error { return e.Err }

// CheckRule refuses a rule without a name or an expect query, or with a query
// that does not parse.
func CheckRule(r Rule, resolve query.Resolver) error {
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("name the rule")
	}
	if len(r.Name) > 200 {
		return errors.New("the name is too long")
	}
	if strings.TrimSpace(r.Expect) == "" {
		return errors.New("say what each host should match, such as nginx.version >= \"1.24\"")
	}
	for _, q := range []struct{ field, src string }{{"scope", r.Scope}, {"expect", r.Expect}} {
		if strings.TrimSpace(q.src) == "" {
			continue
		}
		if _, err := query.Compile(q.src, resolve); err != nil {
			var qe *query.Error
			if errors.As(err, &qe) {
				return &RuleQueryError{Field: q.field, Err: qe}
			}
			return fmt.Errorf("%s: %w", q.field, err)
		}
	}
	return nil
}

const ruleCols = `id, name, description, owner, scope, expect, created_by, created_at, updated_at`

func scanRule(sc interface{ Scan(...any) error }) (Rule, error) {
	var r Rule
	var c, u string
	err := sc.Scan(&r.ID, &r.Name, &r.Description, &r.Owner, &r.Scope, &r.Expect, &r.CreatedBy, &c, &u)
	r.Created, r.Updated = parseTS(c), parseTS(u)
	return r, err
}

// Rules lists every rule by name.
func (s *Store) Rules(ctx context.Context) ([]Rule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+ruleCols+` FROM rule ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Rule loads one rule by id.
func (s *Store) Rule(ctx context.Context, id int64) (Rule, error) {
	r, err := scanRule(s.db.QueryRowContext(ctx, `SELECT `+ruleCols+` FROM rule WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrRuleNotFound
	}
	return r, err
}

// RuleByName loads one rule by its name, ignoring case.
func (s *Store) RuleByName(ctx context.Context, name string) (Rule, error) {
	r, err := scanRule(s.db.QueryRowContext(ctx,
		`SELECT `+ruleCols+` FROM rule WHERE name=? COLLATE NOCASE`, strings.TrimSpace(name)))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrRuleNotFound
	}
	return r, err
}

// PutRule creates a rule, or with an ID updates that one, after checking its
// queries parse. Changing either query starts its history again, since counts
// taken against another question would draw a misleading line. A snapshot of
// the counts now is taken either way, so the history starts at once.
func (s *Store) PutRule(ctx context.Context, r Rule, actor string) (int64, error) {
	r.Name = strings.TrimSpace(r.Name)
	r.Description = strings.TrimSpace(r.Description)
	r.Owner = strings.TrimSpace(r.Owner)
	r.Scope = strings.TrimSpace(r.Scope)
	r.Expect = strings.TrimSpace(r.Expect)
	if err := CheckRule(r, s.QueryResolver(ctx)); err != nil {
		return 0, err
	}
	if other, err := s.RuleByName(ctx, r.Name); err == nil && other.ID != r.ID {
		return 0, fmt.Errorf("there is already a rule named %q", other.Name)
	} else if err != nil && !errors.Is(err, ErrRuleNotFound) {
		return 0, err
	}
	now := ts(time.Now())
	if r.ID == 0 {
		res, err := s.db.ExecContext(ctx,
			`INSERT INTO rule (name, description, owner, scope, expect, created_by, created_at, updated_at)
			 VALUES (?,?,?,?,?,?,?,?)`, r.Name, r.Description, r.Owner, r.Scope, r.Expect, actor, now, now)
		if err != nil {
			return 0, err
		}
		if r.ID, err = res.LastInsertId(); err != nil {
			return 0, err
		}
		_ = s.Audit(ctx, actor, "rule.create", r.Name, ruleDetail(r))
	} else {
		old, err := s.Rule(ctx, r.ID)
		if err != nil {
			return 0, err
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE rule SET name=?, description=?, owner=?, scope=?, expect=?, updated_at=? WHERE id=?`,
			r.Name, r.Description, r.Owner, r.Scope, r.Expect, now, r.ID); err != nil {
			return 0, err
		}
		if old.Scope != r.Scope || old.Expect != r.Expect {
			if _, err := s.db.ExecContext(ctx, `DELETE FROM rule_snapshot WHERE rule_id=?`, r.ID); err != nil {
				return 0, err
			}
		}
		_ = s.Audit(ctx, actor, "rule.update", r.Name, ruleDetail(r))
	}
	if _, err := s.snapshotRules(ctx, time.Now(), r.ID); err != nil {
		return r.ID, err
	}
	return r.ID, nil
}

func ruleDetail(r Rule) string {
	scope := r.Scope
	if scope == "" {
		scope = "every host"
	}
	return "scope: " + scope + "; expect: " + r.Expect
}

// DeleteRule removes a rule and its history.
func (s *Store) DeleteRule(ctx context.Context, id int64, actor string) error {
	r, err := s.Rule(ctx, id)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM rule_snapshot WHERE rule_id=?`, id); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM rule WHERE id=?`, id); err != nil {
		return err
	}
	_ = s.Audit(ctx, actor, "rule.delete", r.Name, ruleDetail(r))
	return nil
}

// RuleHost is one host in a rule's scope.
type RuleHost struct {
	ID   string
	Name string
	// Missing are the attributes the expect query reads that the host has
	// no value for. Set for hosts the rule cannot decide.
	Missing []string
	// Values are the host's values for the attributes the expect query
	// reads, for saying what it has instead.
	Values map[string]string
}

// RuleEval is a rule measured against the hosts there are now.
type RuleEval struct {
	Rule Rule
	// Attrs are the attributes the expect query reads.
	Attrs   []string
	InScope int
	Done    []RuleHost
	NotYet  []RuleHost
	Unknown []RuleHost
	// Err is set when a query no longer parses, for instance because the
	// collector it names left the catalog. Such a rule counts no hosts.
	Err error
}

// Counts returns the sizes of the three buckets.
func (e RuleEval) Counts() RuleCounts {
	return RuleCounts{InScope: e.InScope, Done: len(e.Done), NotYet: len(e.NotYet), Unknown: len(e.Unknown)}
}

// RuleCounts is how far a rollout has got.
type RuleCounts struct {
	InScope int
	Done    int
	NotYet  int
	Unknown int
}

// AllDone is true when every host in scope is done. A rule whose scope
// matches no host is not complete: there is nothing to have rolled out to.
func (c RuleCounts) AllDone() bool { return c.InScope > 0 && c.NotYet == 0 && c.Unknown == 0 }

// Pct is the share of hosts in scope that are done, rounded down.
func (c RuleCounts) Pct() int {
	if c.InScope == 0 {
		return 0
	}
	return c.Done * 100 / c.InScope
}

// EvaluateRule sorts the hosts a rule's scope chooses into Done, Not yet and
// Unknown. A host is Unknown when its expect query hangs on a value it has
// not reported, as when the collector recording it is not switched on or has
// not run there yet. A nil resolver names parameterised attributes the
// default way.
func EvaluateRule(r Rule, hosts []query.Host, resolve query.Resolver) RuleEval {
	ev := RuleEval{Rule: r}
	var scope *query.Expr
	if strings.TrimSpace(r.Scope) != "" {
		var err error
		if scope, err = query.Compile(r.Scope, resolve); err != nil {
			ev.Err = &RuleQueryError{Field: "scope", Err: asQueryErr(err)}
			return ev
		}
	}
	expect, err := query.Compile(r.Expect, resolve)
	if err != nil {
		ev.Err = &RuleQueryError{Field: "expect", Err: asQueryErr(err)}
		return ev
	}
	ev.Attrs = expect.Attrs()
	for _, h := range hosts {
		if scope != nil && !scope.Match(h) {
			continue
		}
		ev.InScope++
		// The values the rule reads are kept in every bucket: they are what put
		// the host there, and what a tool reading the API checks.
		rh := RuleHost{ID: h.ID, Name: h.Name, Values: map[string]string{}}
		for _, a := range ev.Attrs {
			if v := h.Values[a]; v != "" {
				rh.Values[a] = v
			}
		}
		switch expect.Decide(h) {
		case query.Yes:
			ev.Done = append(ev.Done, rh)
		case query.No:
			ev.NotYet = append(ev.NotYet, rh)
		default:
			rh.Missing = expect.Missing(h)
			ev.Unknown = append(ev.Unknown, rh)
		}
	}
	return ev
}

func asQueryErr(err error) *query.Error {
	var qe *query.Error
	if errors.As(err, &qe) {
		return qe
	}
	return &query.Error{Col: 1, Msg: err.Error()}
}

// EvaluateRules measures every rule against current values, loading the
// hosts once.
func (s *Store) EvaluateRules(ctx context.Context) ([]RuleEval, error) {
	rules, err := s.Rules(ctx)
	if err != nil || len(rules) == 0 {
		return nil, err
	}
	hosts, err := s.QueryHosts(ctx)
	if err != nil {
		return nil, err
	}
	resolve := s.QueryResolver(ctx)
	out := make([]RuleEval, 0, len(rules))
	for _, r := range rules {
		out = append(out, EvaluateRule(r, hosts, resolve))
	}
	return out, nil
}

// EvaluateRuleNow measures one rule against current values.
func (s *Store) EvaluateRuleNow(ctx context.Context, r Rule) (RuleEval, error) {
	hosts, err := s.QueryHosts(ctx)
	if err != nil {
		return RuleEval{}, err
	}
	return EvaluateRule(r, hosts, s.QueryResolver(ctx)), nil
}

// RuleSnapshot is a rule's counts at one time.
type RuleSnapshot struct {
	At time.Time
	RuleCounts
	// Changed is true when the counts differ from the snapshot before.
	Changed bool
}

// RuleHistory lists a rule's snapshots, oldest first.
func (s *Store) RuleHistory(ctx context.Context, id int64) ([]RuleSnapshot, error) {
	m, err := s.ruleHistories(ctx, id)
	return m[id], err
}

// RuleHistories lists every rule's snapshots, by rule id, oldest first.
func (s *Store) RuleHistories(ctx context.Context) (map[int64][]RuleSnapshot, error) {
	return s.ruleHistories(ctx, 0)
}

// ruleHistories loads the snapshots of one rule, or of every rule when id is
// zero, oldest first.
func (s *Store) ruleHistories(ctx context.Context, id int64) (map[int64][]RuleSnapshot, error) {
	q := `SELECT rule_id, at, in_scope, done, not_yet, unknown, changed FROM rule_snapshot`
	var args []any
	if id != 0 {
		q += ` WHERE rule_id=?`
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY rule_id, at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]RuleSnapshot{}
	for rows.Next() {
		var rid int64
		var at string
		var sn RuleSnapshot
		if err := rows.Scan(&rid, &at, &sn.InScope, &sn.Done, &sn.NotYet, &sn.Unknown, &sn.Changed); err != nil {
			return nil, err
		}
		sn.At = parseTS(at)
		out[rid] = append(out[rid], sn)
	}
	return out, rows.Err()
}

// SnapshotQuiet is how long a rule's counts may stay the same before another
// snapshot of them is taken anyway, so the history has a point a day.
const SnapshotQuiet = 24 * time.Hour

// SnapshotRules records every rule's counts where they changed since its last
// snapshot, or where its last snapshot is SnapshotQuiet old. It is called
// when a collection run finishes, and returns how many it took.
func (s *Store) SnapshotRules(ctx context.Context, now time.Time) (int, error) {
	return s.snapshotRules(ctx, now, 0)
}

// snapshotRules is SnapshotRules for one rule, or for all when id is zero.
func (s *Store) snapshotRules(ctx context.Context, now time.Time, id int64) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rule`).Scan(&n); err != nil || n == 0 {
		return 0, err
	}
	var evals []RuleEval
	if id != 0 {
		r, err := s.Rule(ctx, id)
		if err != nil {
			return 0, err
		}
		ev, err := s.EvaluateRuleNow(ctx, r)
		if err != nil {
			return 0, err
		}
		evals = []RuleEval{ev}
	} else {
		var err error
		if evals, err = s.EvaluateRules(ctx); err != nil {
			return 0, err
		}
	}
	taken := 0
	for _, ev := range evals {
		c := ev.Counts()
		var at string
		var last RuleCounts
		err := s.db.QueryRowContext(ctx,
			`SELECT at, in_scope, done, not_yet, unknown FROM rule_snapshot WHERE rule_id=? ORDER BY at DESC, id DESC LIMIT 1`,
			ev.Rule.ID).Scan(&at, &last.InScope, &last.Done, &last.NotYet, &last.Unknown)
		first := errors.Is(err, sql.ErrNoRows)
		if err != nil && !first {
			return taken, err
		}
		changed := first || c != last
		if !changed && now.Sub(parseTS(at)) < SnapshotQuiet {
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO rule_snapshot (rule_id, at, in_scope, done, not_yet, unknown, changed) VALUES (?,?,?,?,?,?,?)`,
			ev.Rule.ID, ts(now), c.InScope, c.Done, c.NotYet, c.Unknown, changed); err != nil {
			return taken, err
		}
		taken++
	}
	return taken, nil
}

// afterRun is called when a collection run finishes. Rule snapshots are a
// view of the run's results, so a failure to take one never fails the run.
func (s *Store) afterRun(ctx context.Context) {
	_, _ = s.SnapshotRules(ctx, time.Now())
}

// RuleResult is a rule's progress in a few numbers, for lists, summaries and
// notifications.
type RuleResult struct {
	Rule Rule
	RuleCounts
	// Complete is true when every host in scope is done; CompleteSince is
	// when it got there, as far as the snapshots tell, and zero when they do
	// not say yet.
	Complete      bool
	CompleteSince time.Time
	// LastChange is when the counts last moved, or the rule was created.
	LastChange time.Time
	// Err says why the rule cannot be evaluated, or is empty.
	Err string
}

// RuleStatus is every rule's progress against current values, by name. It
// is what a summary of open rollouts reads: open ones are those with
// Complete false.
func (s *Store) RuleStatus(ctx context.Context) ([]RuleResult, error) {
	evals, err := s.EvaluateRules(ctx)
	if err != nil || len(evals) == 0 {
		return nil, err
	}
	hist, err := s.ruleHistories(ctx, 0)
	if err != nil {
		return nil, err
	}
	out := make([]RuleResult, 0, len(evals))
	for _, ev := range evals {
		out = append(out, ResultOf(ev, hist[ev.Rule.ID]))
	}
	return out, nil
}

// ResultOf sums up an evaluation and the rule's history.
func ResultOf(ev RuleEval, hist []RuleSnapshot) RuleResult {
	res := RuleResult{Rule: ev.Rule, RuleCounts: ev.Counts(), LastChange: ev.Rule.Created}
	if ev.Err != nil {
		res.Err = ev.Err.Error()
	}
	res.Complete = ev.Err == nil && res.RuleCounts.AllDone()
	for _, sn := range hist {
		if sn.Changed {
			res.LastChange = sn.At
		}
	}
	// The latest snapshot must agree with now for the history to date it.
	if res.Complete && len(hist) > 0 && hist[len(hist)-1].RuleCounts == res.RuleCounts {
		for i := len(hist) - 1; i >= 0 && hist[i].RuleCounts.AllDone(); i-- {
			res.CompleteSince = hist[i].At
		}
	}
	return res
}
