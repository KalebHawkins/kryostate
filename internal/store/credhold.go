package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// CredentialHold is how a password credential stands after failing to log
// in. Fails counts authentication failures in a row, across hosts and runs;
// once it reaches the limit the credential is held, and nothing logs in with
// it again until a person replaces or releases it. A directory account that
// locks after a few bad passwords would otherwise be locked by the first run
// after the password changed, one host at a time.
type CredentialHold struct {
	Ref       string
	Username  string
	Fails     int
	Held      bool
	LastHost  string
	LastError string
	UpdatedAt time.Time
}

// CredentialHoldFor returns the standing of one credential, if it has failed.
func (s *Store) CredentialHoldFor(ctx context.Context, ref, username string) (CredentialHold, bool, error) {
	h := CredentialHold{Ref: ref, Username: username}
	var held int
	var upd string
	err := s.db.QueryRowContext(ctx, `
		SELECT fails, held, last_host, last_error, updated_at FROM credential_hold
		WHERE ref = ? AND username = ?`, ref, username).
		Scan(&h.Fails, &held, &h.LastHost, &h.LastError, &upd)
	if errors.Is(err, sql.ErrNoRows) {
		return CredentialHold{}, false, nil
	}
	if err != nil {
		return CredentialHold{}, false, err
	}
	h.Held, h.UpdatedAt = held != 0, parseTS(upd)
	return h, true, nil
}

// NoteCredentialFailure counts one authentication failure and holds the
// credential once limit failures have happened in a row. It returns the
// standing afterwards.
func (s *Store) NoteCredentialFailure(ctx context.Context, ref, username, host, errText string, limit int) (CredentialHold, error) {
	if limit < 1 {
		limit = 1
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO credential_hold (ref, username, fails, held, last_host, last_error, updated_at)
		VALUES (?, ?, 1, ?, ?, ?, ?)
		ON CONFLICT(ref, username) DO UPDATE SET
			fails = credential_hold.fails + 1,
			held = CASE WHEN credential_hold.fails + 1 >= ? THEN 1 ELSE credential_hold.held END,
			last_host = excluded.last_host,
			last_error = excluded.last_error,
			updated_at = excluded.updated_at`,
		ref, username, boolInt(limit <= 1), host, errText, ts(time.Now()), limit); err != nil {
		return CredentialHold{}, err
	}
	h, _, err := s.CredentialHoldFor(ctx, ref, username)
	return h, err
}

// ClearCredentialHold forgets a credential's failures: after it logged in,
// or once a person has replaced the password. An empty username clears the
// reference for every user.
func (s *Store) ClearCredentialHold(ctx context.Context, ref, username string) (int64, error) {
	var res sql.Result
	var err error
	if username == "" {
		res, err = s.db.ExecContext(ctx, `DELETE FROM credential_hold WHERE ref = ?`, ref)
	} else {
		res, err = s.db.ExecContext(ctx, `DELETE FROM credential_hold WHERE ref = ? AND username = ?`, ref, username)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CredentialHolds lists every credential that has failed, held or not.
func (s *Store) CredentialHolds(ctx context.Context) ([]CredentialHold, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ref, username, fails, held, last_host, last_error, updated_at
		FROM credential_hold ORDER BY ref, username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CredentialHold
	for rows.Next() {
		var h CredentialHold
		var held int
		var upd string
		if err := rows.Scan(&h.Ref, &h.Username, &h.Fails, &held, &h.LastHost, &h.LastError, &upd); err != nil {
			return nil, err
		}
		h.Held, h.UpdatedAt = held != 0, parseTS(upd)
		out = append(out, h)
	}
	return out, rows.Err()
}
