package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The tables here coordinate processes that share one database: a running
// kryostate serve and a kryostate refresh typed in another shell. They are
// created here rather than in schema.sql because nothing else joins to them.
const coordinationSQL = `
CREATE TABLE IF NOT EXISTS collection_lease (
	id           INTEGER PRIMARY KEY CHECK (id = 1),
	holder       TEXT NOT NULL,
	purpose      TEXT NOT NULL DEFAULT '',
	origin       TEXT NOT NULL DEFAULT '',
	actor        TEXT NOT NULL DEFAULT '',
	acquired_at  TEXT NOT NULL,
	heartbeat_at TEXT NOT NULL,
	expires_at   TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS host_backoff (
	ci_id      TEXT PRIMARY KEY,
	fails      INTEGER NOT NULL,
	until      TEXT NOT NULL,
	last_error TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL
);
`

func (s *Store) migrateCoordination(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, coordinationSQL); err != nil {
		return fmt.Errorf("apply coordination schema: %w", err)
	}
	return nil
}

// Lease is the one collection lease: whichever process holds it may open
// sessions into the estate, and every other process waits or skips.
type Lease struct {
	// Holder identifies the process, as host/pid/random.
	Holder string
	// Purpose is what the holder is doing, in words.
	Purpose string
	// Origin is "cli", "web" or "schedule".
	Origin string
	// Actor is who started it, when a person did.
	Actor       string
	AcquiredAt  time.Time
	HeartbeatAt time.Time
	ExpiresAt   time.Time
}

// Host is the machine the holder runs on, taken from the holder id.
func (l Lease) Host() string {
	h, _, _ := strings.Cut(l.Holder, "/")
	return h
}

// Describe says, for a person, what holds the lease.
func (l Lease) Describe() string {
	var what string
	switch l.Origin {
	case "cli":
		what = "a collection started from the command line"
		if l.Actor != "" {
			what += " by " + l.Actor
		}
	case "web":
		what = "a collection started from the web interface of another kryostate serve"
		if l.Actor != "" {
			what += " by " + l.Actor
		}
	case "schedule":
		what = "a scheduled run in another kryostate serve"
	default:
		what = "another collection"
		if l.Purpose != "" {
			what += " (" + l.Purpose + ")"
		}
	}
	if h := l.Host(); h != "" {
		what += " on " + h
	}
	return what
}

// LeaseRequest is what a process asks for when it takes the lease.
type LeaseRequest struct {
	Holder  string
	Purpose string
	Origin  string
	Actor   string
	TTL     time.Duration
	Now     time.Time
}

// AcquireLease takes the collection lease when it is free, expired, or
// already held by the same holder (which renews it). It reports whether the
// caller now holds it and, either way, the lease as it stands.
//
// The insert-or-update is the transaction's first statement, so SQLite takes
// the write lock before reading anything: the same effect as BEGIN
// IMMEDIATE, with no read lock to upgrade and so no deadlock between two
// processes taking it at once. The other waits on busy_timeout.
func (s *Store) AcquireLease(ctx context.Context, r LeaseRequest) (bool, Lease, error) {
	if r.Holder == "" || r.TTL <= 0 {
		return false, Lease{}, errors.New("lease needs a holder and a duration")
	}
	if r.Now.IsZero() {
		r.Now = time.Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, Lease{}, err
	}
	defer tx.Rollback()
	now := ts(r.Now)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO collection_lease (id, holder, purpose, origin, actor, acquired_at, heartbeat_at, expires_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			holder = excluded.holder,
			purpose = excluded.purpose,
			origin = excluded.origin,
			actor = excluded.actor,
			acquired_at = CASE WHEN collection_lease.holder = excluded.holder
				THEN collection_lease.acquired_at ELSE excluded.acquired_at END,
			heartbeat_at = excluded.heartbeat_at,
			expires_at = excluded.expires_at
		WHERE collection_lease.holder = excluded.holder
		   OR collection_lease.expires_at <= excluded.acquired_at`,
		r.Holder, r.Purpose, r.Origin, r.Actor, now, now, ts(r.Now.Add(r.TTL))); err != nil {
		return false, Lease{}, err
	}
	l, _, err := scanLease(tx.QueryRowContext(ctx, leaseSelect))
	if err != nil {
		return false, Lease{}, err
	}
	if err := tx.Commit(); err != nil {
		return false, Lease{}, err
	}
	return l.Holder == r.Holder, l, nil
}

// RenewLease pushes the holder's lease out by ttl. It reports false when the
// holder no longer holds it, because it expired and somebody else took it.
func (s *Store) RenewLease(ctx context.Context, holder string, ttl time.Duration, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE collection_lease SET heartbeat_at = ?, expires_at = ? WHERE id = 1 AND holder = ?`,
		ts(now), ts(now.Add(ttl)), holder)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReleaseLease gives the lease up, if holder still holds it.
func (s *Store) ReleaseLease(ctx context.Context, holder string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM collection_lease WHERE id = 1 AND holder = ?`, holder)
	return err
}

// CurrentLease returns the lease when somebody holds it and it has not
// expired at now.
func (s *Store) CurrentLease(ctx context.Context, now time.Time) (Lease, bool, error) {
	l, ok, err := scanLease(s.db.QueryRowContext(ctx, leaseSelect))
	if err != nil || !ok {
		return Lease{}, false, err
	}
	if !l.ExpiresAt.After(now) {
		return Lease{}, false, nil
	}
	return l, true, nil
}

const leaseSelect = `SELECT holder, purpose, origin, actor, acquired_at, heartbeat_at, expires_at
	FROM collection_lease WHERE id = 1`

func scanLease(row *sql.Row) (Lease, bool, error) {
	var l Lease
	var acq, hb, exp string
	err := row.Scan(&l.Holder, &l.Purpose, &l.Origin, &l.Actor, &acq, &hb, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, false, nil
	}
	if err != nil {
		return Lease{}, false, err
	}
	l.AcquiredAt, l.HeartbeatAt, l.ExpiresAt = parseTS(acq), parseTS(hb), parseTS(exp)
	return l, true, nil
}

// HostBackoff is how the scheduler stands with a host it could not reach:
// how many times in a row, and when it will next try.
type HostBackoff struct {
	CIID      string
	Fails     int
	Until     time.Time
	LastError string
	UpdatedAt time.Time
}

// PutHostBackoff records a host's backoff, replacing what was there.
func (s *Store) PutHostBackoff(ctx context.Context, b HostBackoff) error {
	if b.UpdatedAt.IsZero() {
		b.UpdatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO host_backoff (ci_id, fails, until, last_error, updated_at) VALUES (?,?,?,?,?)
		ON CONFLICT(ci_id) DO UPDATE SET fails = excluded.fails, until = excluded.until,
			last_error = excluded.last_error, updated_at = excluded.updated_at`,
		b.CIID, b.Fails, ts(b.Until), b.LastError, ts(b.UpdatedAt))
	return err
}

// ClearHostBackoff forgets a host's failures, once it has answered.
func (s *Store) ClearHostBackoff(ctx context.Context, ciID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM host_backoff WHERE ci_id = ?`, ciID)
	return err
}

// HostBackoffs returns every recorded backoff, by host.
func (s *Store) HostBackoffs(ctx context.Context) (map[string]HostBackoff, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ci_id, fails, until, last_error, updated_at FROM host_backoff`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]HostBackoff{}
	for rows.Next() {
		var b HostBackoff
		var until, upd string
		if err := rows.Scan(&b.CIID, &b.Fails, &until, &b.LastError, &upd); err != nil {
			return nil, err
		}
		b.Until, b.UpdatedAt = parseTS(until), parseTS(upd)
		out[b.CIID] = b
	}
	return out, rows.Err()
}

// HostBackoffFor returns one host's backoff, if it has one.
func (s *Store) HostBackoffFor(ctx context.Context, ciID string) (HostBackoff, bool, error) {
	b := HostBackoff{CIID: ciID}
	var until, upd string
	err := s.db.QueryRowContext(ctx,
		`SELECT fails, until, last_error, updated_at FROM host_backoff WHERE ci_id = ?`, ciID).
		Scan(&b.Fails, &until, &b.LastError, &upd)
	if errors.Is(err, sql.ErrNoRows) {
		return HostBackoff{}, false, nil
	}
	if err != nil {
		return HostBackoff{}, false, err
	}
	b.Until, b.UpdatedAt = parseTS(until), parseTS(upd)
	return b, true, nil
}
