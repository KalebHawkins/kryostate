package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ReadFailure is an open episode of a source failing to read an attribute on
// a host: the host answered, but the command failed or printed nothing usable.
// While it is open the source's earlier value is not current.
type ReadFailure struct {
	CIID     string
	Attr     string
	Scope    string
	SourceID int64
	SpecID   string
	Error    string
	// Since is the first failed reading of the episode; LastAt the latest.
	Since  time.Time
	LastAt time.Time
}

// RecordReadFailure notes that a reading failed. A failure already open for
// the same attribute is extended rather than repeated, so a value that has
// been unreadable for a month is one row saying since when.
func (s *Store) RecordReadFailure(ctx context.Context, f ReadFailure) error {
	if f.CIID == "" || f.Attr == "" {
		return errors.New("read failure needs a ci and an attribute")
	}
	if f.LastAt.IsZero() {
		f.LastAt = time.Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM read_failure
		 WHERE ci_id=? AND attr=? AND scope=? AND source_id=? AND cleared_at IS NULL
		 ORDER BY id DESC LIMIT 1`,
		f.CIID, f.Attr, f.Scope, f.SourceID).Scan(&id)
	switch {
	case err == nil:
		_, err = tx.ExecContext(ctx,
			`UPDATE read_failure SET last_at=MAX(last_at, ?), error=?, spec_id=? WHERE id=?`,
			ts(f.LastAt), f.Error, f.SpecID, id)
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx,
			`INSERT INTO read_failure (ci_id, attr, scope, source_id, spec_id, error, since, last_at)
			 VALUES (?,?,?,?,?,?,?,?)`,
			f.CIID, f.Attr, f.Scope, f.SourceID, f.SpecID, f.Error, ts(f.LastAt), ts(f.LastAt))
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// clearReadFailure closes the episode a good reading ends. Only an episode
// that began before the reading is closed: output replayed from storage
// arrives with its original time and says nothing about a failure since.
func (s *Store) clearReadFailure(ctx context.Context, ciID, attr, scope string, sourceID int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE read_failure SET cleared_at=?
		 WHERE ci_id=? AND attr=? AND scope=? AND source_id=? AND cleared_at IS NULL AND since <= ?`,
		ts(at), ciID, attr, scope, sourceID, ts(at))
	return err
}

// ReadFailures lists a host's open episodes, by attribute.
func (s *Store) ReadFailures(ctx context.Context, ciID string) ([]ReadFailure, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ci_id, attr, scope, source_id, spec_id, error, since, last_at
		 FROM read_failure WHERE ci_id=? AND cleared_at IS NULL ORDER BY attr, scope`, ciID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReadFailure
	for rows.Next() {
		var f ReadFailure
		var since, last string
		if err := rows.Scan(&f.CIID, &f.Attr, &f.Scope, &f.SourceID, &f.SpecID, &f.Error, &since, &last); err != nil {
			return nil, err
		}
		f.Since, f.LastAt = parseTS(since), parseTS(last)
		out = append(out, f)
	}
	return out, rows.Err()
}
