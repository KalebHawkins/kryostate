package store

import (
	"context"
	"strings"
	"time"
)

// Change is a collected value that differs from the value the same source
// collected before it, for one host, attribute and instance.
type Change struct {
	AssertionID int64
	CIID        string
	Host        string
	Attr        string
	Scope       string
	Old         string
	New         string
	OldAt       time.Time
	At          time.Time
	// SpecID is the collector command that saw the new value, where known.
	// Values collected before spec ids were recorded leave it empty, and an
	// external program is named by SourceName instead.
	SpecID     string
	SourceName string
	RunID      string
	// Trigger is what started the run that saw the change, as on the runs
	// page.
	Trigger string
}

// ChangeFilter narrows Changes.
type ChangeFilter struct {
	Since time.Time
	CIID  string
	Attr  string
	Limit int
}

// Changes lists changed values, newest first.
//
// Only collected values count, and each source is compared with itself: a
// document disagreeing with a machine is not a change, and neither is one
// collector reporting a value another one never reported. The first value
// ever collected is not a change either; there is nothing it changed from.
// Retracted claims are skipped, so a corrected parse does not show up as the
// host having changed.
func (s *Store) Changes(ctx context.Context, f ChangeFilter) ([]Change, error) {
	if f.Limit <= 0 {
		f.Limit = 500
	}
	since := ts(f.Since)
	var extra []string
	args := []any{since}
	if f.CIID != "" {
		extra = append(extra, "a.ci_id = ?")
		args = append(args, f.CIID)
	}
	if f.Attr != "" {
		extra = append(extra, "a.attr = ?")
		args = append(args, f.Attr)
	}
	cond := ""
	if len(extra) > 0 {
		cond = " AND " + strings.Join(extra, " AND ")
	}
	// The window runs only over the series that have something new in the
	// period, so the cost follows what changed lately rather than the
	// length of the whole history.
	q := `
	WITH touched AS (
	  SELECT DISTINCT a.ci_id, a.attr, a.scope, a.source_id
	  FROM assertion a JOIN source s ON s.id=a.source_id
	  WHERE s.kind='collector' AND a.retracted_at IS NULL AND a.observed_at >= ?` + cond + `
	),
	seq AS (
	  SELECT a.id, a.ci_id, a.attr, a.scope, a.raw_value, a.observed_at, a.spec_id, a.run_id,
	         a.source_id,
	         LAG(a.raw_value)   OVER w AS prev_raw,
	         LAG(a.observed_at) OVER w AS prev_at
	  FROM assertion a
	  JOIN touched t ON t.ci_id=a.ci_id AND t.attr=a.attr AND t.scope=a.scope AND t.source_id=a.source_id
	  WHERE a.retracted_at IS NULL
	  WINDOW w AS (PARTITION BY a.ci_id, a.attr, a.scope, a.source_id ORDER BY a.observed_at, a.id)
	)
	SELECT q.id, q.ci_id, c.label, q.attr, q.scope, q.prev_raw, q.raw_value, q.prev_at, q.observed_at,
	       q.spec_id, s.name, q.run_id, COALESCE(r.trigger, '')
	FROM seq q
	JOIN ci c ON c.id=q.ci_id
	JOIN source s ON s.id=q.source_id
	LEFT JOIN run r ON r.id=q.run_id
	WHERE q.prev_raw IS NOT NULL AND q.prev_raw <> q.raw_value AND q.observed_at >= ?
	  AND c.retired_at IS NULL
	ORDER BY q.observed_at DESC, q.id DESC
	LIMIT ?`
	args = append(args, since, f.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		var oldAt, at string
		if err := rows.Scan(&c.AssertionID, &c.CIID, &c.Host, &c.Attr, &c.Scope, &c.Old, &c.New,
			&oldAt, &at, &c.SpecID, &c.SourceName, &c.RunID, &c.Trigger); err != nil {
			return nil, err
		}
		c.OldAt, c.At = parseTS(oldAt), parseTS(at)
		out = append(out, c)
	}
	return out, rows.Err()
}
