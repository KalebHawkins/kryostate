package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/KalebHawkins/kryostate/internal/collect"
)

// UserCollector is a collector somebody added with "kryostate collectors
// add": one spec, and who added it and when.
type UserCollector struct {
	Spec      collect.Spec
	AddedBy   string
	AddedAt   time.Time
	UpdatedBy string
	UpdatedAt time.Time
}

// Replaced reports whether the collector was added again after it was first
// added.
func (u UserCollector) Replaced() bool { return u.UpdatedAt.After(u.AddedAt) }

// PutUserCollector adds a collector, or replaces the one with the same id. It
// reports whether one was replaced. The spec is checked as a collectors file
// would check it; the caller checks it against the other collectors, which
// the store does not know about.
func (s *Store) PutUserCollector(ctx context.Context, sp collect.Spec, by string) (updated bool, err error) {
	if err := collect.CheckSpec(sp); err != nil {
		return false, err
	}
	b, err := json.Marshal(sp)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_collector WHERE id=?`, sp.ID).Scan(&n); err != nil {
		return false, err
	}
	now := ts(time.Now())
	if n > 0 {
		_, err = tx.ExecContext(ctx,
			`UPDATE user_collector SET spec_json=?, updated_by=?, updated_at=? WHERE id=?`, string(b), by, now, sp.ID)
	} else {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO user_collector (id, spec_json, added_by, added_at, updated_by, updated_at) VALUES (?,?,?,?,?,?)`,
			sp.ID, string(b), by, now, by, now)
	}
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	action := "collector added"
	if n > 0 {
		action = "collector updated"
	}
	return n > 0, s.Audit(ctx, by, action, sp.ID, sp.Attr+": "+sp.Command)
}

// UserCollectors lists the added collectors, by id.
func (s *Store) UserCollectors(ctx context.Context) ([]UserCollector, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, spec_json, added_by, added_at, updated_by, updated_at FROM user_collector ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserCollector
	for rows.Next() {
		var (
			id, js, added, updated string
			u                      UserCollector
		)
		if err := rows.Scan(&id, &js, &u.AddedBy, &added, &u.UpdatedBy, &updated); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(js), &u.Spec); err != nil {
			return nil, fmt.Errorf("added collector %s: %w", id, err)
		}
		u.Spec.ID = id
		u.AddedAt, u.UpdatedAt = parseTS(added), parseTS(updated)
		out = append(out, u)
	}
	return out, rows.Err()
}

// DeleteUserCollector removes an added collector. Values it recorded are kept.
// It returns sql.ErrNoRows when no collector by that id was added.
func (s *Store) DeleteUserCollector(ctx context.Context, id, by string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM user_collector WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return sql.ErrNoRows
	}
	return s.Audit(ctx, by, "collector removed", id, "")
}
