package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"time"

	"github.com/KalebHawkins/hearsay/internal/model"
)

// Fingerprint identifies a disagreement across detection runs, so that finding
// the same conflict again updates the existing record instead of burying a
// decision someone already made under a pile of duplicates. Scope is part of
// it: two instances on one host can disagree in the same words and are still
// two findings, each needing its own decision.
func Fingerprint(ciID string, kind model.DiscrepancyKind, attr, scope, claimed, observed string) string {
	h := sha256.Sum256([]byte(ciID + "\x00" + string(kind) + "\x00" + attr + "\x00" +
		scope + "\x00" + claimed + "\x00" + observed))
	return hex.EncodeToString(h[:12])
}

// RaiseDiscrepancy records a disagreement. If the same disagreement is already
// recorded, its disposition is left alone unless the detector had previously
// closed it: a decision a person made stands, but a finding that lapsed and
// has come back is open again.
func (s *Store) RaiseDiscrepancy(ctx context.Context, d model.Discrepancy) error {
	fp := Fingerprint(d.CIID, d.Kind, d.Attr, d.Scope, d.Claimed, d.Observed)
	if d.DetectedAt.IsZero() {
		d.DetectedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO discrepancy (fingerprint, ci_id, attr, scope, kind, summary, claimed, claimed_by, observed, observed_by, detected_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(fingerprint) DO UPDATE SET
		   detected_at=excluded.detected_at, summary=excluded.summary,
		   disposition=CASE WHEN discrepancy.disposition='lapsed' THEN 'open' ELSE discrepancy.disposition END,
		   disposed_by=CASE WHEN discrepancy.disposition='lapsed' THEN '' ELSE discrepancy.disposed_by END,
		   disposed_at=CASE WHEN discrepancy.disposition='lapsed' THEN NULL ELSE discrepancy.disposed_at END`,
		fp, d.CIID, d.Attr, d.Scope, string(d.Kind), d.Summary, d.Claimed, d.ClaimedBy,
		d.Observed, d.ObservedBy, ts(d.DetectedAt))
	return err
}

// ResolveStale closes discrepancies of a kind that were not re-detected in the
// current pass. A conflict that no longer exists should stop demanding
// attention, but the row stays for the record, marked as lapsed rather than
// accepted: nobody looked at it, it simply stopped happening.
func (s *Store) ResolveStale(ctx context.Context, kind model.DiscrepancyKind, before time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE discrepancy SET disposition='lapsed', note='no longer detected', disposed_by='system', disposed_at=?
		 WHERE kind=? AND disposition='open' AND detected_at < ?`,
		ts(time.Now()), string(kind), ts(before))
	return err
}

// ResolveStaleFor lapses open findings of a kind for named hosts only.
//
// A pass that could not ask the question about a host must not answer it. When
// a resolver times out, the host was not shown to have recovered; lapsing its
// finding would turn an outage into a clean bill of health for everything the
// outage prevented checking.
func (s *Store) ResolveStaleFor(ctx context.Context, kind model.DiscrepancyKind, before time.Time, ciIDs []string) error {
	// SQLite caps bound variables per statement. An estate larger than the cap
	// would make the whole statement fail, and a reconciliation that silently
	// lapses nothing is the failure mode this function exists to prevent.
	const batch = 500
	now := ts(time.Now())
	cut := ts(before)
	for start := 0; start < len(ciIDs); start += batch {
		end := start + batch
		if end > len(ciIDs) {
			end = len(ciIDs)
		}
		chunk := ciIDs[start:end]
		ph := make([]string, len(chunk))
		args := []any{now, string(kind), cut}
		for i, id := range chunk {
			ph[i] = "?"
			args = append(args, id)
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE discrepancy SET disposition='lapsed', note='no longer detected', disposed_by='system', disposed_at=?
			 WHERE kind=? AND disposition='open' AND detected_at < ? AND ci_id IN (`+strings.Join(ph, ",")+`)`,
			args...); err != nil {
			return err
		}
	}
	return nil
}

// DiscrepancyFilter narrows the list shown on the landing page.
type DiscrepancyFilter struct {
	Kind        model.DiscrepancyKind
	Disposition model.Disposition
	CIID        string
}

// Discrepancies returns disagreements, newest first.
func (s *Store) Discrepancies(ctx context.Context, f DiscrepancyFilter, limit int) ([]model.Discrepancy, error) {
	q := `SELECT d.id, d.ci_id, c.label, d.attr, d.scope, d.kind, d.summary, d.claimed, d.claimed_by,
	             d.observed, d.observed_by, d.detected_at, d.disposition, d.disposed_by, d.disposed_at, d.note
	      FROM discrepancy d JOIN ci c ON c.id=d.ci_id WHERE 1=1`
	var args []any
	if f.Kind != "" {
		q += ` AND d.kind=?`
		args = append(args, string(f.Kind))
	}
	if f.Disposition != "" {
		q += ` AND d.disposition=?`
		args = append(args, string(f.Disposition))
	}
	if f.CIID != "" {
		q += ` AND d.ci_id=?`
		args = append(args, f.CIID)
	}
	q += ` ORDER BY d.detected_at DESC, d.id DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Discrepancy
	for rows.Next() {
		var d model.Discrepancy
		var kind, disp, detected string
		var disposedAt sql.NullString
		if err := rows.Scan(&d.ID, &d.CIID, &d.CILabel, &d.Attr, &d.Scope, &kind, &d.Summary,
			&d.Claimed, &d.ClaimedBy, &d.Observed, &d.ObservedBy, &detected,
			&disp, &d.DisposedBy, &disposedAt, &d.Note); err != nil {
			return nil, err
		}
		d.Kind = model.DiscrepancyKind(kind)
		d.Disposition = model.Disposition(disp)
		d.DetectedAt = parseTS(detected)
		if disposedAt.Valid {
			t := parseTS(disposedAt.String)
			d.DisposedAt = &t
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DiscrepancyCounts summarises open disagreements by kind for the landing page.
func (s *Store) DiscrepancyCounts(ctx context.Context) (map[model.DiscrepancyKind]int, int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, COUNT(*) FROM discrepancy WHERE disposition='open' GROUP BY kind`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := map[model.DiscrepancyKind]int{}
	total := 0
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, 0, err
		}
		out[model.DiscrepancyKind(k)] = n
		total += n
	}
	return out, total, rows.Err()
}

// Dispose records what a person decided about a disagreement and why. The
// chosen value is written as a human assertion marked decided, so it wins in
// the resolved view against the source that was rejected. Without that mark a
// "keep the document" decision would close the finding while the value it
// rejected carried on being displayed.
func (s *Store) Dispose(ctx context.Context, id int64, disp model.Disposition, by, note string) error {
	var d model.Discrepancy
	var kind string
	err := s.db.QueryRowContext(ctx,
		`SELECT ci_id, attr, scope, kind, claimed, observed FROM discrepancy WHERE id=?`, id).
		Scan(&d.CIID, &d.Attr, &d.Scope, &kind, &d.Claimed, &d.Observed)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE discrepancy SET disposition=?, disposed_by=?, disposed_at=?, note=? WHERE id=?`,
		string(disp), by, ts(time.Now()), note, id); err != nil {
		return err
	}

	if d.Attr != "" && (disp == model.DispConfirmMachine || disp == model.DispKeepDocument) {
		value := d.Observed
		if disp == model.DispKeepDocument {
			value = d.Claimed
		}
		srcID, err := s.SourceID(ctx, model.SourceHuman, by)
		if err != nil {
			return err
		}
		if _, err := s.Assert(ctx, model.Assertion{
			CIID: d.CIID, Attr: d.Attr, Scope: d.Scope, Raw: value,
			SourceID: srcID, ObservedAt: time.Now(), Decided: true,
		}, s.normalizedFor(ctx, d.Attr, value)); err != nil {
			return err
		}
	}
	return s.Audit(ctx, by, "dispose", d.CIID, string(disp)+" "+d.Attr+": "+note)
}

// Normalizer is the part of the normaliser this package needs. It is an
// interface so that store does not depend on the rule engine.
type Normalizer interface {
	NormalizeFor(attr, value string) (Normalized, bool)
}

// SetNormalizer supplies the rules used when a person's decision is recorded,
// so a chosen value is charted beside the collected ones instead of showing up
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
}

// Runs returns recent collection runs.
func (s *Store) Runs(ctx context.Context, limit int) ([]RunSummary, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, collector, started_at, COALESCE(finished_at,''), status, targets, succeeded, failed
		 FROM run ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunSummary
	for rows.Next() {
		var r RunSummary
		var st, fin string
		if err := rows.Scan(&r.ID, &r.Collector, &st, &fin, &r.Status, &r.Targets, &r.Succeeded, &r.Failed); err != nil {
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
