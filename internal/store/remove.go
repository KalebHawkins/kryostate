package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DeleteHost deletes a configuration item and everything recorded about it:
// identities, assertions and their normalised values, evidence, probes, read
// failures, addresses, checks, connection profiles, stack memberships and
// back-off. It cannot be undone; DisableHost is the way to stop contacting a
// host while keeping it.
//
// The audit trail is kept. The rows naming the host stay where they are, and
// one more records the deletion with the host's name, so the trail still says
// what was deleted, by whom and why, after the host itself is gone.
//
// Every row is deleted explicitly rather than left to ON DELETE CASCADE,
// because read_failure and host_backoff do not cascade and because cascades
// only run on a connection with foreign keys enforced.
func (s *Store) DeleteHost(ctx context.Context, id, actor, reason string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var label string
	if err := tx.QueryRowContext(ctx, `SELECT label FROM ci WHERE id=?`, id).Scan(&label); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("delete %s: %w", id, ErrNoSuchHost)
		}
		return "", err
	}
	for _, q := range []string{
		`DELETE FROM evidence WHERE ci_id=?`,
		`DELETE FROM assertion_norm WHERE assertion_id IN (SELECT id FROM assertion WHERE ci_id=?)`,
		`DELETE FROM read_failure WHERE ci_id=?`,
		`DELETE FROM assertion WHERE ci_id=?`,
		`DELETE FROM probe WHERE ci_id=?`,
		`DELETE FROM check_def WHERE ci_id=?`,
		`DELETE FROM address WHERE ci_id=?`,
		`DELETE FROM conn_profile WHERE ci_id=?`,
		`DELETE FROM stack_member WHERE ci_id=?`,
		`DELETE FROM identity WHERE ci_id=?`,
		`DELETE FROM host_backoff WHERE ci_id=?`,
		`DELETE FROM ci WHERE id=?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return label, fmt.Errorf("delete %s: %w", label, err)
		}
	}
	detail := label
	if strings.TrimSpace(reason) != "" {
		detail += ": " + strings.TrimSpace(reason)
	}
	if err := auditTx(ctx, tx, actor, "delete host", id, detail); err != nil {
		return label, err
	}
	return label, tx.Commit()
}

// DisableHost stops KryoState contacting a host: it is left out of collection,
// scheduled runs, external collectors, liveness checks and DNS lookups. It is
// still listed, charted and queryable, and its connections are left exactly
// as they are, each with its own enabled flag, so EnableHost brings back
// everything as it was. Disabling a host that already is changes nothing and
// reports false.
func (s *Store) DisableHost(ctx context.Context, id, actor, reason string) (bool, error) {
	return s.setDisabled(ctx, id, actor, reason, true)
}

// EnableHost reverses DisableHost. It reports false when the host was not
// disabled.
func (s *Store) EnableHost(ctx context.Context, id, actor, reason string) (bool, error) {
	return s.setDisabled(ctx, id, actor, reason, false)
}

func (s *Store) setDisabled(ctx context.Context, id, actor, reason string, disable bool) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ci WHERE id=?`, id).Scan(&n); err != nil {
		return false, err
	}
	if n == 0 {
		return false, fmt.Errorf("%s: %w", id, ErrNoSuchHost)
	}
	q, action := `UPDATE ci SET disabled_at=NULL WHERE id=? AND disabled_at IS NOT NULL`, "enable host"
	args := []any{id}
	if disable {
		q, action = `UPDATE ci SET disabled_at=? WHERE id=? AND disabled_at IS NULL`, "disable host"
		args = []any{ts(time.Now()), id}
	}
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	if changed, _ := res.RowsAffected(); changed == 0 {
		return false, nil
	}
	if err := auditTx(ctx, tx, actor, action, id, strings.TrimSpace(reason)); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// migrateDisabled turns the retired hosts of an older database into disabled
// ones and drops the retired_at column. A retired host had its connections
// switched off or removed, so it stays quiet after the change as before; the
// difference is that it is listed again, marked disabled.
func (s *Store) migrateDisabled(ctx context.Context) error {
	if err := s.ensureColumn(ctx, "ci", "disabled_at", "TEXT"); err != nil {
		return err
	}
	has, err := s.hasColumn(ctx, "ci", "retired_at")
	if err != nil || !has {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := ts(time.Now())
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit (at, actor, action, subject, detail)
		 SELECT ?, 'kryostate', 'disable host', id, 'was retired on ' || retired_at || '; retired hosts are now disabled'
		 FROM ci WHERE retired_at IS NOT NULL AND disabled_at IS NULL`, now); err != nil {
		return fmt.Errorf("migrate retired hosts: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE ci SET disabled_at=retired_at WHERE retired_at IS NOT NULL AND disabled_at IS NULL`); err != nil {
		return fmt.Errorf("migrate retired hosts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE ci DROP COLUMN retired_at`); err != nil {
		return fmt.Errorf("drop ci.retired_at: %w", err)
	}
	return tx.Commit()
}

func auditTx(ctx context.Context, tx *sql.Tx, actor, action, subject, detail string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO audit (at, actor, action, subject, detail) VALUES (?,?,?,?,?)`,
		ts(time.Now()), actor, action, subject, detail)
	return err
}
