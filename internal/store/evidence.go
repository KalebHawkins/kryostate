package store

import (
	"context"
	"time"
)

// MaxEvidence bounds the stored output of one command. The transport already
// caps what it will read from a host; this is a second, much lower cap on what
// is worth keeping forever. A version string that needs more than this to
// parse is not going to be fixed by a regular expression either.
const MaxEvidence = 8 << 10

// Evidence is what a collector command printed, kept against the day a parse
// rule turns out to be wrong.
//
// Only the latest output per host, spec and scope is retained. Scope is part
// of the key because one spec can report on several instances on a machine,
// and each instance's output has to be repairable separately. The history of
// what a host claimed lives in the assertion table; this is a repair kit, and
// keeping every identical copy of `cat /etc/os-release` would be a slow way to
// fill a disk.
type Evidence struct {
	CIID      string
	SpecID    string
	Attr      string
	Scope     string
	Command   string
	Output    string
	Truncated bool
	// AssertionID is the claim this output produced. A repair replaces that
	// claim specifically. Without it a reparse would have to guess from the
	// resolved winner, and would withdraw a document's value, or another
	// collector's, or nothing at all.
	AssertionID int64
	RunID       string
	At          time.Time
}

// RecordEvidence stores the output of one command, replacing whatever was
// held for that host, spec and scope.
func (s *Store) RecordEvidence(ctx context.Context, e Evidence) error {
	if e.CIID == "" || e.SpecID == "" {
		return nil
	}
	out := e.Output
	truncated := e.Truncated
	if len(out) > MaxEvidence {
		out = out[:MaxEvidence]
		truncated = true
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	var aid any
	if e.AssertionID != 0 {
		aid = e.AssertionID
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO evidence (ci_id, spec_id, attr, scope, command, output, truncated, assertion_id, run_id, at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(ci_id, spec_id, scope) DO UPDATE SET
		   attr=excluded.attr, command=excluded.command, output=excluded.output,
		   truncated=excluded.truncated, assertion_id=excluded.assertion_id,
		   run_id=excluded.run_id, at=excluded.at`,
		e.CIID, e.SpecID, e.Attr, e.Scope, e.Command, out, boolInt(truncated), aid, e.RunID, ts(e.At))
	return err
}

// LinkEvidence records which assertion a stored output produced. Collection
// writes the output before it knows the assertion's id, so the link is made
// immediately afterwards.
func (s *Store) LinkEvidence(ctx context.Context, ciID, specID, scope string, assertionID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE evidence SET assertion_id=? WHERE ci_id=? AND spec_id=? AND scope=?`,
		assertionID, ciID, specID, scope)
	return err
}

// LinkEvidenceIf records the link only while the stored output is still the
// one that was read. A reparse works from a snapshot, and a collection that
// replaces the output in between would otherwise end up with the new output
// credited for a value derived from the old one.
//
// It reports whether the link was made.
func (s *Store) LinkEvidenceIf(ctx context.Context, ciID, specID, scope string, assertionID int64, output string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE evidence SET assertion_id=? WHERE ci_id=? AND spec_id=? AND scope=? AND output=?`,
		assertionID, ciID, specID, scope, output)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// EvidenceRestingOn counts the stored outputs credited with producing one
// assertion. More than one means the assertion is shared, and withdrawing it
// on behalf of a single spec would take another spec's claim with it.
func (s *Store) EvidenceRestingOn(ctx context.Context, assertionID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM evidence WHERE assertion_id=?`, assertionID).Scan(&n)
	return n, err
}

// EvidenceFilter narrows a reparse to the specs worth re-running.
type EvidenceFilter struct {
	CIID   string
	SpecID string
	Attr   string
}

// AllEvidence returns stored command output, optionally narrowed. The result
// is the input to a reparse: everything needed to re-derive a value without
// contacting a host.
func (s *Store) AllEvidence(ctx context.Context, f EvidenceFilter) ([]Evidence, error) {
	q := `SELECT ci_id, spec_id, attr, scope, command, output, truncated,
	             COALESCE(assertion_id, 0), run_id, at
	      FROM evidence WHERE 1=1`
	var args []any
	if f.CIID != "" {
		q += " AND ci_id=?"
		args = append(args, f.CIID)
	}
	if f.SpecID != "" {
		q += " AND spec_id=?"
		args = append(args, f.SpecID)
	}
	if f.Attr != "" {
		q += " AND attr LIKE ?"
		args = append(args, f.Attr)
	}
	q += " ORDER BY ci_id, spec_id"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Evidence
	for rows.Next() {
		var e Evidence
		var trunc int
		var at string
		if err := rows.Scan(&e.CIID, &e.SpecID, &e.Attr, &e.Scope, &e.Command,
			&e.Output, &trunc, &e.AssertionID, &e.RunID, &at); err != nil {
			return nil, err
		}
		e.Truncated = trunc != 0
		e.At = parseTS(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ForgetEvidence deletes stored output. Somebody who decides a command's
// output is too sensitive to keep needs a way to remove it that is not
// deleting the host.
func (s *Store) ForgetEvidence(ctx context.Context, f EvidenceFilter) (int64, error) {
	q := `DELETE FROM evidence WHERE 1=1`
	var args []any
	if f.CIID != "" {
		q += " AND ci_id=?"
		args = append(args, f.CIID)
	}
	if f.SpecID != "" {
		q += " AND spec_id=?"
		args = append(args, f.SpecID)
	}
	if f.Attr != "" {
		q += " AND attr LIKE ?"
		args = append(args, f.Attr)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// EvidenceFor returns the stored output behind a host's current values, for
// the page that has to explain where a number came from.
func (s *Store) EvidenceFor(ctx context.Context, ciID string) (map[string]Evidence, error) {
	ev, err := s.AllEvidence(ctx, EvidenceFilter{CIID: ciID})
	if err != nil {
		return nil, err
	}
	out := make(map[string]Evidence, len(ev))
	for _, e := range ev {
		key := e.Attr
		if e.Scope != "" {
			key += "@" + e.Scope
		}
		out[key] = e
	}
	return out, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
