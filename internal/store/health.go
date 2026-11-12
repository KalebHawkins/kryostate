package store

import (
	"context"
	"fmt"
	"time"
)

// The overview's problem buckets as predicates over a live host aliased c.
// They count exactly what EstateHealth counts, so a number on the overview and
// the inventory it links to always agree.
const (
	hostFailingSQL = `EXISTS (
	  SELECT 1 FROM probe p
	  WHERE p.ci_id = c.id AND p.kind = 'collector_run' AND p.outcome NOT IN ('ok','partial')
	    AND p.id = (SELECT q.id FROM probe q WHERE q.ci_id = c.id AND q.kind = 'collector_run'
	                ORDER BY q.at DESC, q.id DESC LIMIT 1))`
	hostNeverSQL  = `NOT EXISTS (SELECT 1 FROM probe p WHERE p.ci_id = c.id AND p.kind = 'collector_run')`
	hostNoConnSQL = `NOT EXISTS (SELECT 1 FROM conn_profile cp WHERE cp.ci_id = c.id AND cp.enabled = 1)`
	// hostStaleSQL takes the cutoff as its one argument. A host never
	// collected successfully has no last good collection and is not stale.
	hostStaleSQL = `(SELECT MAX(p.at) FROM probe p WHERE p.ci_id = c.id AND p.kind = 'collector_run'
	    AND p.outcome IN ('ok','partial')) < ?`
)

// The values HostFilter.Health takes.
const (
	HealthFailing = "failing"
	HealthStale   = "stale"
	HealthNever   = "never"
	HealthNoConn  = "noconn"
)

// healthWhere is the predicate for one problem bucket, and its arguments. An
// unrecognised bucket matches nothing rather than everything: a mistyped link
// must not claim the whole estate has a problem.
func healthWhere(health string, staleBefore time.Time) (string, []any) {
	switch health {
	case HealthFailing:
		return hostFailingSQL, nil
	case HealthNever:
		return hostNeverSQL, nil
	case HealthNoConn:
		return hostNoConnSQL, nil
	case HealthStale:
		if staleBefore.IsZero() {
			staleBefore = time.Now().Add(-14 * 24 * time.Hour)
		}
		return hostStaleSQL, []any{ts(staleBefore)}
	default:
		return "0", nil
	}
}

// NeedingAttention counts live hosts in at least one problem bucket: failing,
// stale, never collected, or with no connection. The buckets overlap (a host
// with no connection has usually never been collected), so this is not their
// sum.
func (s *Store) NeedingAttention(ctx context.Context, staleAfter time.Duration) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ci c WHERE c.retired_at IS NULL AND c.kind='host' AND (`+
			hostFailingSQL+` OR `+hostNeverSQL+` OR `+hostNoConnSQL+` OR `+hostStaleSQL+`)`,
		ts(time.Now().Add(-staleAfter))).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("hosts needing attention: %w", err)
	}
	return n, nil
}

// StackSizes counts each stack's live members, largest first, in one query.
func (s *Store) StackSizes(ctx context.Context) ([]Bucket, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.name, COUNT(c.id) AS n
		 FROM stack s
		 JOIN stack_member m ON m.stack_id = s.id
		 JOIN ci c ON c.id = m.ci_id AND c.retired_at IS NULL AND c.kind = 'host'
		 GROUP BY s.id, s.name
		 ORDER BY n DESC, s.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bucket
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.Key, &b.Count); err != nil {
			return nil, err
		}
		b.Label = b.Key
		out = append(out, b)
	}
	return out, rows.Err()
}
