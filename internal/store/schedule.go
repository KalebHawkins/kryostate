package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Schedule is how often one collector runs on its own. Every of zero means
// the collector only runs when somebody asks. At, when set, is a local time
// of day in HH:MM and only applies to whole-day intervals.
//
// Which values are sensible is the engine's business; the store keeps what it
// is given and refuses only what it could not read back.
type Schedule struct {
	Collector string
	Every     time.Duration
	At        string
	UpdatedBy string
	UpdatedAt time.Time
}

// Manual reports whether the collector only runs when asked.
func (s Schedule) Manual() bool { return s.Every <= 0 }

// Schedules returns every schedule somebody chose, by collector key.
// Collectors missing from the map run on their default.
func (s *Store) Schedules(ctx context.Context) (map[string]Schedule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT collector, every_sec, at_time, updated_by, updated_at FROM collector_schedule`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Schedule{}
	for rows.Next() {
		var sc Schedule
		var sec int64
		var at string
		if err := rows.Scan(&sc.Collector, &sec, &sc.At, &sc.UpdatedBy, &at); err != nil {
			return nil, err
		}
		sc.Every = time.Duration(sec) * time.Second
		sc.UpdatedAt = parseTS(at)
		out[sc.Collector] = sc
	}
	return out, rows.Err()
}

// PutSchedule records a chosen schedule and audits who chose it.
func (s *Store) PutSchedule(ctx context.Context, sc Schedule, by string) error {
	if strings.TrimSpace(sc.Collector) == "" {
		return errors.New("a schedule needs a collector")
	}
	if sc.Every < 0 {
		return errors.New("an interval cannot be negative")
	}
	if sc.Every%time.Second != 0 {
		return errors.New("an interval is kept to the second")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO collector_schedule (collector, every_sec, at_time, updated_by, updated_at)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(collector) DO UPDATE SET every_sec=excluded.every_sec,
		   at_time=excluded.at_time, updated_by=excluded.updated_by, updated_at=excluded.updated_at`,
		sc.Collector, int64(sc.Every/time.Second), sc.At, by, ts(time.Now()))
	if err != nil {
		return err
	}
	what := "manual only"
	if !sc.Manual() {
		what = "every " + sc.Every.String()
		if sc.At != "" {
			what += " at " + sc.At
		}
	}
	return s.Audit(ctx, by, "schedule", sc.Collector, what)
}

// ResetSchedule forgets a chosen schedule, so the collector returns to the
// default for its kind.
func (s *Store) ResetSchedule(ctx context.Context, collector, by string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM collector_schedule WHERE collector=?`, collector); err != nil {
		return err
	}
	return s.Audit(ctx, by, "schedule", collector, "default")
}

// CollectorState is what happened the last time a collector ran.
type CollectorState struct {
	Collector    string
	LastStarted  time.Time
	LastFinished time.Time
	Outcome      string
	Detail       string
	RunID        string
}

// CollectorStates returns the last run of every collector that has run.
func (s *Store) CollectorStates(ctx context.Context) (map[string]CollectorState, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT collector, last_started, last_finished, last_outcome, last_detail, last_run_id FROM collector_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]CollectorState{}
	for rows.Next() {
		var c CollectorState
		var st, fin string
		if err := rows.Scan(&c.Collector, &st, &fin, &c.Outcome, &c.Detail, &c.RunID); err != nil {
			return nil, err
		}
		c.LastStarted, c.LastFinished = parseTS(st), parseTS(fin)
		out[c.Collector] = c
	}
	return out, rows.Err()
}

// MarkCollectorsStarted records that these collectors began a run. The start
// is what the next due time is counted from, so it is written before any host
// is contacted: a run that dies halfway is still a run, and is not repeated
// at the next tick.
func (s *Store) MarkCollectorsStarted(ctx context.Context, keys []string, at time.Time, runID string) error {
	for _, k := range keys {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO collector_state (collector, last_started, last_outcome, last_run_id)
			 VALUES (?,?,'running',?)
			 ON CONFLICT(collector) DO UPDATE SET last_started=excluded.last_started,
			   last_outcome='running', last_detail='', last_run_id=excluded.last_run_id`,
			k, ts(at), runID); err != nil {
			return err
		}
	}
	return nil
}

// MarkCollectorFinished records how a collector's run ended.
func (s *Store) MarkCollectorFinished(ctx context.Context, key string, at time.Time, outcome, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO collector_state (collector, last_finished, last_outcome, last_detail)
		 VALUES (?,?,?,?)
		 ON CONFLICT(collector) DO UPDATE SET last_finished=excluded.last_finished,
		   last_outcome=excluded.last_outcome, last_detail=excluded.last_detail`,
		key, ts(at), outcome, detail)
	return err
}

// MarkCollectorsRan records a complete run of these collectors in one go,
// for a full collection that ran every collector at once.
func (s *Store) MarkCollectorsRan(ctx context.Context, keys []string, started, finished time.Time, runID, outcome, detail string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, k := range keys {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO collector_state (collector, last_started, last_finished, last_outcome, last_detail, last_run_id)
			 VALUES (?,?,?,?,?,?)
			 ON CONFLICT(collector) DO UPDATE SET last_started=excluded.last_started,
			   last_finished=excluded.last_finished, last_outcome=excluded.last_outcome,
			   last_detail=excluded.last_detail, last_run_id=excluded.last_run_id`,
			k, ts(started), ts(finished), outcome, detail, runID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LastFullRun is when the most recent complete collection of every
// collector started, or zero. A collector that has no state of its own was
// last run by that.
func (s *Store) LastFullRun(ctx context.Context) (time.Time, error) {
	var at string
	err := s.db.QueryRowContext(ctx,
		`SELECT started_at FROM run WHERE collector='builtin' AND status='complete'
		 ORDER BY started_at DESC LIMIT 1`).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	return parseTS(at), err
}

// StartRunAs opens a run and records what started it.
func (s *Store) StartRunAs(ctx context.Context, collector, trigger string, targets int) (string, error) {
	id := newID()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO run (id, collector, started_at, status, targets, trigger) VALUES (?,?,?,'running',?,?)`,
		id, collector, ts(time.Now()), targets, trigger)
	return id, err
}

// FinishRunNote closes a run with its counts and a short note, such as how
// many hosts were skipped while backing off.
func (s *Store) FinishRunNote(ctx context.Context, id, status string, ok, failed int, note string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE run SET finished_at=?, status=?, succeeded=?, failed=?, note=? WHERE id=?`,
		ts(time.Now()), status, ok, failed, note, id)
	if err == nil {
		s.afterRun(ctx)
	}
	return err
}

// HostFamilies returns the operating system family each host last reported,
// lower case, for choosing which collectors apply before connecting. A host
// that never reported one is missing from the map.
func (s *Store) HostFamilies(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ci_id, raw_value FROM current_fact WHERE attr='os.family' AND scope=''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, v string
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		v = strings.ToLower(strings.TrimSpace(v))
		switch {
		case strings.Contains(v, "linux"):
			v = "linux"
		case strings.Contains(v, "aix"):
			v = "aix"
		case strings.Contains(v, "windows"):
			v = "windows"
		}
		if v != "" {
			out[id] = v
		}
	}
	return out, rows.Err()
}

// FailingHost is a host whose most recent collection did not succeed.
type FailingHost struct {
	CIID    string
	Label   string
	Outcome string
	Detail  string
	At      time.Time
}

// FailingHosts lists live hosts whose latest collector run failed, most
// recent failure first.
func (s *Store) FailingHosts(ctx context.Context, limit int) ([]FailingHost, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, c.label, p.outcome, p.detail, p.at
		 FROM ci c JOIN probe p ON p.ci_id=c.id
		 WHERE c.retired_at IS NULL AND c.kind='host' AND p.kind='collector_run'
		   AND p.id = (SELECT q.id FROM probe q WHERE q.ci_id=c.id AND q.kind='collector_run'
		               ORDER BY q.at DESC, q.id DESC LIMIT 1)
		   AND p.outcome NOT IN ('ok','partial')
		 ORDER BY p.at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FailingHost
	for rows.Next() {
		var f FailingHost
		var at string
		if err := rows.Scan(&f.CIID, &f.Label, &f.Outcome, &f.Detail, &at); err != nil {
			return nil, err
		}
		f.At = parseTS(at)
		out = append(out, f)
	}
	return out, rows.Err()
}

// Health is the estate in a handful of numbers, each a count of live hosts.
type Health struct {
	Hosts int
	// OK hosts' latest collection succeeded, wholly or in part.
	OK int
	// Failing hosts' latest collection did not.
	Failing int
	// Never were never collected from, successfully or otherwise.
	Never int
	// NoConnection have no enabled way in, so nothing will ever be collected.
	NoConnection int
	// Stale last collected successfully longer ago than the window given.
	Stale int
}

// EstateHealth counts hosts by the outcome of their latest collection.
func (s *Store) EstateHealth(ctx context.Context, staleAfter time.Duration) (Health, error) {
	var h Health
	cutoff := ts(time.Now().Add(-staleAfter))
	err := s.db.QueryRowContext(ctx, `
	WITH live AS (SELECT id FROM ci WHERE retired_at IS NULL AND kind='host'),
	latest AS (
	  SELECT p.ci_id, p.outcome FROM probe p JOIN live l ON l.id=p.ci_id
	  WHERE p.kind='collector_run'
	    AND p.id=(SELECT q.id FROM probe q WHERE q.ci_id=p.ci_id AND q.kind='collector_run'
	              ORDER BY q.at DESC, q.id DESC LIMIT 1)
	),
	lastok AS (
	  SELECT p.ci_id, MAX(p.at) AS at FROM probe p JOIN live l ON l.id=p.ci_id
	  WHERE p.kind='collector_run' AND p.outcome IN ('ok','partial') GROUP BY p.ci_id
	)
	SELECT
	  (SELECT COUNT(*) FROM live),
	  (SELECT COUNT(*) FROM latest WHERE outcome IN ('ok','partial')),
	  (SELECT COUNT(*) FROM latest WHERE outcome NOT IN ('ok','partial')),
	  (SELECT COUNT(*) FROM live WHERE id NOT IN (SELECT ci_id FROM latest)),
	  (SELECT COUNT(*) FROM live WHERE id NOT IN (SELECT ci_id FROM conn_profile WHERE enabled=1)),
	  (SELECT COUNT(*) FROM lastok WHERE at < ?)`, cutoff).
		Scan(&h.Hosts, &h.OK, &h.Failing, &h.Never, &h.NoConnection, &h.Stale)
	if err != nil {
		return h, fmt.Errorf("estate health: %w", err)
	}
	return h, nil
}
