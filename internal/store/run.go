package store

import (
	"context"
	"time"
)

// Normalizer is the part of the normaliser this package needs. It is an
// interface so that store does not depend on the rule engine.
type Normalizer interface {
	NormalizeFor(attr, value string) (Normalized, bool)
}

// SetNormalizer supplies the rules used when a person states an application,
// so a stated value is charted beside the collected ones instead of showing up
// as a value no rule understood.
func (s *Store) SetNormalizer(n Normalizer) { s.norm = n }

func (s *Store) normalizedFor(ctx context.Context, attr, value string) *Normalized {
	if s.norm == nil {
		return nil
	}
	if n, ok := s.norm.NormalizeFor(attr, value); ok {
		return &n
	}
	return nil
}

// StartRun opens a collection run. Runs are recorded whether or not they
// finish, so a partial pass can never be mistaken for a complete enumeration
// of what a host has installed.
func (s *Store) StartRun(ctx context.Context, collector string, targets int) (string, error) {
	id := newID()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO run (id, collector, started_at, status, targets) VALUES (?,?,?,'running',?)`,
		id, collector, ts(time.Now()), targets)
	return id, err
}

// FinishRun closes a run with its outcome counts.
func (s *Store) FinishRun(ctx context.Context, id, status string, ok, failed int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE run SET finished_at=?, status=?, succeeded=?, failed=? WHERE id=?`,
		ts(time.Now()), status, ok, failed, id)
	return err
}

// RunSummary describes a completed or running collection.
type RunSummary struct {
	ID        string
	Collector string
	Started   time.Time
	Finished  time.Time
	Status    string
	Targets   int
	Succeeded int
	Failed    int
	// Trigger says what started the run: empty for a person at the command
	// line, "web" for the button, "schedule" for the scheduler.
	Trigger string
	Note    string
}

// Runs returns recent collection runs.
func (s *Store) Runs(ctx context.Context, limit int) ([]RunSummary, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, collector, started_at, COALESCE(finished_at,''), status, targets, succeeded, failed, trigger, note
		 FROM run ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunSummary
	for rows.Next() {
		var r RunSummary
		var st, fin string
		if err := rows.Scan(&r.ID, &r.Collector, &st, &fin, &r.Status, &r.Targets, &r.Succeeded, &r.Failed, &r.Trigger, &r.Note); err != nil {
			return nil, err
		}
		r.Started, r.Finished = parseTS(st), parseTS(fin)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Coverage reports how much of the estate has been collected and how fresh it
// is. This is the number a configuration database can honestly own: not
// whether hosts are up, but whether what it says about them is current.
type Coverage struct {
	Hosts        int
	WithProfile  int
	EverSeen     int
	FreshLast24h int
	Stale        int
	NeverSeen    int
	FailingNow   int
}

// Coverage computes the freshness summary for the landing page.
func (s *Store) Coverage(ctx context.Context, staleAfter time.Duration) (Coverage, error) {
	var c Coverage
	cutoff := ts(time.Now().Add(-staleAfter))
	day := ts(time.Now().Add(-24 * time.Hour))
	q := `
	SELECT
	  (SELECT COUNT(*) FROM ci WHERE retired_at IS NULL AND kind='host'),
	  (SELECT COUNT(DISTINCT ci_id) FROM conn_profile WHERE enabled=1),
	  (SELECT COUNT(DISTINCT ci_id) FROM probe WHERE kind='collector_run' AND outcome='ok'),
	  (SELECT COUNT(DISTINCT ci_id) FROM probe WHERE kind='collector_run' AND outcome='ok' AND at > ?),
	  (SELECT COUNT(*) FROM ci c WHERE c.retired_at IS NULL AND c.kind='host'
	     AND EXISTS (SELECT 1 FROM probe p WHERE p.ci_id=c.id AND p.kind='collector_run' AND p.outcome='ok')
	     AND NOT EXISTS (SELECT 1 FROM probe p WHERE p.ci_id=c.id AND p.kind='collector_run' AND p.outcome='ok' AND p.at > ?))`
	if err := s.db.QueryRowContext(ctx, q, day, cutoff).
		Scan(&c.Hosts, &c.WithProfile, &c.EverSeen, &c.FreshLast24h, &c.Stale); err != nil {
		return c, err
	}
	c.NeverSeen = c.Hosts - c.EverSeen
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM ci c WHERE c.retired_at IS NULL AND c.kind='host'
		AND EXISTS (
		  SELECT 1 FROM probe p WHERE p.ci_id=c.id AND p.kind='collector_run' AND p.outcome<>'ok'
		  AND p.at=(SELECT MAX(q.at) FROM probe q WHERE q.ci_id=c.id AND q.kind='collector_run'))`).
		Scan(&c.FailingNow)
	return c, err
}
