package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// HostRemoval says what RemoveHost took away.
type HostRemoval struct {
	// AlreadyRetired is set when the host had been retired before; the
	// operational clean-up is done again anyway.
	AlreadyRetired bool
	Profiles       int64
	Memberships    int64
	Checks         int64
	Backoff        int64
}

// String sums up a removal in a few words, for a command's output and the
// audit trail.
func (r HostRemoval) String() string {
	return fmt.Sprintf("%d connections, %d stack memberships, %d checks, %d back-off entries removed",
		r.Profiles, r.Memberships, r.Checks, r.Backoff)
}

// RemoveHost retires a host and takes it out of everything that would act on
// it: its connection profiles (and so their pinned host keys), its stack
// memberships, its liveness checks and its collection back-off. What was said
// about it is kept: assertions, probes, evidence, read failures, identities
// and the audit trail, so the history still explains itself. Unretire brings
// the host back; its connections and memberships have to be added again.
//
// It is one transaction, so a failure part-way leaves the host as it was.
func (s *Store) RemoveHost(ctx context.Context, id, actor, reason string) (HostRemoval, error) {
	var r HostRemoval
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return r, err
	}
	defer tx.Rollback()

	var retired sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT retired_at FROM ci WHERE id=?`, id).Scan(&retired); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return r, fmt.Errorf("remove %s: %w", id, ErrNoSuchHost)
		}
		return r, err
	}
	r.AlreadyRetired = retired.Valid
	if !r.AlreadyRetired {
		if _, err := tx.ExecContext(ctx, `UPDATE ci SET retired_at=? WHERE id=?`, ts(time.Now()), id); err != nil {
			return r, err
		}
	}
	for _, d := range []struct {
		n *int64
		q string
	}{
		{&r.Profiles, `DELETE FROM conn_profile WHERE ci_id=?`},
		{&r.Memberships, `DELETE FROM stack_member WHERE ci_id=?`},
		{&r.Checks, `DELETE FROM check_def WHERE ci_id=?`},
		{&r.Backoff, `DELETE FROM host_backoff WHERE ci_id=?`},
	} {
		res, err := tx.ExecContext(ctx, d.q, id)
		if err != nil {
			return r, err
		}
		*d.n, _ = res.RowsAffected()
	}
	detail := r.String()
	if reason != "" {
		detail = reason + "; " + detail
	}
	if err := auditTx(ctx, tx, actor, "remove host", id, detail); err != nil {
		return r, err
	}
	return r, tx.Commit()
}

// PurgeCI deletes a configuration item and everything recorded about it:
// identities, assertions and their normalised values, evidence, probes, read
// failures, addresses, checks, connection profiles, stack memberships,
// back-off and its audit entries. One audit entry saying it was purged, and by
// whom, is written in their place. It cannot be undone; RemoveHost is the
// reversible way to take a host out of service.
//
// Every row is deleted explicitly rather than left to ON DELETE CASCADE,
// because read_failure and host_backoff do not cascade and because cascades
// only run on a connection with foreign keys enforced.
func (s *Store) PurgeCI(ctx context.Context, id, actor, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var label string
	if err := tx.QueryRowContext(ctx, `SELECT label FROM ci WHERE id=?`, id).Scan(&label); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("purge %s: %w", id, ErrNoSuchHost)
		}
		return err
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
		`DELETE FROM audit WHERE subject=?`,
		`DELETE FROM ci WHERE id=?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return fmt.Errorf("purge %s: %w", label, err)
		}
	}
	detail := label
	if strings.TrimSpace(reason) != "" {
		detail += ": " + reason
	}
	if err := auditTx(ctx, tx, actor, "purge host", id, detail); err != nil {
		return err
	}
	return tx.Commit()
}

func auditTx(ctx context.Context, tx *sql.Tx, actor, action, subject, detail string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO audit (at, actor, action, subject, detail) VALUES (?,?,?,?,?)`,
		ts(time.Now()), actor, action, subject, detail)
	return err
}
