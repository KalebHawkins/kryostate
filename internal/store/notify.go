package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// notifySchema holds the daily digest's configuration and history. It lives
// beside the code that uses it rather than in schema.sql, and is applied by
// migrate like the rest.
//
// notify_dest is where a digest goes. Nothing in it is a secret: a webhook
// URL carries its token in the URL, so the row holds a credential reference
// (env:NAME, file:/path, keyring:…) instead, and so does an SMTP password.
// Rows are written only by the command line; the web interface reads them.
//
// notify_config is one row of settings the web interface may change.
//
// notify_send records every digest, test and quiet day, so a restart does not
// send the same day twice and the page can say how the last one went. slot is
// the local date of the digest it belongs to; dest is empty for a day skipped
// as quiet.
const notifySchema = `
CREATE TABLE IF NOT EXISTS notify_dest (
    name         TEXT PRIMARY KEY,
    kind         TEXT NOT NULL,
    format       TEXT NOT NULL DEFAULT '',
    url_ref      TEXT NOT NULL DEFAULT '',
    mail_to      TEXT NOT NULL DEFAULT '',
    mail_from    TEXT NOT NULL DEFAULT '',
    smtp_addr    TEXT NOT NULL DEFAULT '',
    smtp_user    TEXT NOT NULL DEFAULT '',
    password_ref TEXT NOT NULL DEFAULT '',
    security     TEXT NOT NULL DEFAULT '',
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS notify_config (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    enabled    INTEGER NOT NULL,
    at_time    TEXT NOT NULL,
    sections   TEXT NOT NULL,
    send_quiet INTEGER NOT NULL,
    base_url   TEXT NOT NULL DEFAULT '',
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS notify_send (
    id           INTEGER PRIMARY KEY,
    at           TEXT NOT NULL,
    kind         TEXT NOT NULL,
    dest         TEXT NOT NULL DEFAULT '',
    slot         TEXT NOT NULL DEFAULT '',
    window_start TEXT NOT NULL DEFAULT '',
    window_end   TEXT NOT NULL DEFAULT '',
    ok           INTEGER NOT NULL,
    error        TEXT NOT NULL DEFAULT '',
    attempts     INTEGER NOT NULL DEFAULT 0,
    changes      INTEGER NOT NULL DEFAULT 0,
    failing      INTEGER NOT NULL DEFAULT 0,
    recovered    INTEGER NOT NULL DEFAULT 0,
    never        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS notify_send_slot ON notify_send (slot, kind);
`

func (s *Store) migrateNotify(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, notifySchema); err != nil {
		return fmt.Errorf("apply notification schema: %w", err)
	}
	return nil
}

// Destination kinds and the formats a webhook takes.
const (
	DestWebhook = "webhook"
	DestEmail   = "email"

	FormatSlack   = "slack"
	FormatGeneric = "generic"

	SecurityStartTLS = "starttls"
	SecurityTLS      = "tls"
	SecurityNone     = "none"
)

// NotifyDest is somewhere the digest is sent.
type NotifyDest struct {
	Name string
	Kind string
	// Format is how a webhook body is shaped: slack or generic.
	Format string
	// URLRef is a credential reference that resolves to a webhook URL.
	URLRef string
	// The rest describe an email destination.
	To          []string
	From        string
	SMTPAddr    string
	SMTPUser    string
	PasswordRef string
	Security    string
	CreatedBy   string
	CreatedAt   time.Time
}

// AddNotifyDest records a destination. Names are unique.
func (s *Store) AddNotifyDest(ctx context.Context, d NotifyDest, by string) error {
	if strings.TrimSpace(d.Name) == "" {
		return errors.New("a destination needs a name")
	}
	if d.Kind != DestWebhook && d.Kind != DestEmail {
		return fmt.Errorf("unknown destination kind %q", d.Kind)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notify_dest WHERE name=?`, d.Name).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("a destination named %q already exists; remove it first", d.Name)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO notify_dest (name, kind, format, url_ref, mail_to, mail_from, smtp_addr, smtp_user,
		   password_ref, security, created_by, created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.Name, d.Kind, d.Format, d.URLRef, strings.Join(d.To, ","), d.From, d.SMTPAddr, d.SMTPUser,
		d.PasswordRef, d.Security, by, ts(time.Now()))
	if err != nil {
		return err
	}
	return s.Audit(ctx, by, "notify add", d.Name, d.Kind)
}

// RemoveNotifyDest forgets a destination. Its send history stays.
func (s *Store) RemoveNotifyDest(ctx context.Context, name, by string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM notify_dest WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no destination named %q", name)
	}
	return s.Audit(ctx, by, "notify remove", name, "")
}

// NotifyDests lists every destination by name.
func (s *Store) NotifyDests(ctx context.Context) ([]NotifyDest, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, kind, format, url_ref, mail_to, mail_from, smtp_addr, smtp_user, password_ref,
		   security, created_by, created_at FROM notify_dest ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotifyDest
	for rows.Next() {
		var d NotifyDest
		var to, at string
		if err := rows.Scan(&d.Name, &d.Kind, &d.Format, &d.URLRef, &to, &d.From, &d.SMTPAddr, &d.SMTPUser,
			&d.PasswordRef, &d.Security, &d.CreatedBy, &at); err != nil {
			return nil, err
		}
		for _, a := range strings.Split(to, ",") {
			if a = strings.TrimSpace(a); a != "" {
				d.To = append(d.To, a)
			}
		}
		d.CreatedAt = parseTS(at)
		out = append(out, d)
	}
	return out, rows.Err()
}

// FindNotifyDest returns one destination by name.
func (s *Store) FindNotifyDest(ctx context.Context, name string) (NotifyDest, error) {
	all, err := s.NotifyDests(ctx)
	if err != nil {
		return NotifyDest{}, err
	}
	for _, d := range all {
		if d.Name == name {
			return d, nil
		}
	}
	return NotifyDest{}, fmt.Errorf("no destination named %q", name)
}

// The sections a digest may carry.
const (
	SectionChanges = "changes"
	SectionHealth  = "health"
	SectionNever   = "never"
	SectionRules   = "rules"
)

// DigestSections are the sections in the order a digest shows them.
var DigestSections = []string{SectionChanges, SectionHealth, SectionNever, SectionRules}

// NotifySettings are the digest settings safe to change from the browser.
// BaseURL is the exception: it is only set from the command line, because it
// becomes a link in every message.
type NotifySettings struct {
	Enabled bool
	// At is the local time of day, HH:MM.
	At        string
	Sections  []string
	SendQuiet bool
	BaseURL   string
	UpdatedBy string
	UpdatedAt time.Time
}

// Has reports whether a section is switched on.
func (n NotifySettings) Has(section string) bool {
	for _, s := range n.Sections {
		if s == section {
			return true
		}
	}
	return false
}

// DefaultNotifySettings is what an estate that never chose gets: a digest at
// eight in the morning with every section, sent only when something happened.
// Nothing is sent until a destination is added.
func DefaultNotifySettings() NotifySettings {
	return NotifySettings{Enabled: true, At: "08:00", Sections: append([]string(nil), DigestSections...)}
}

// GetNotifySettings returns the settings, or the defaults when none were
// saved.
func (s *Store) GetNotifySettings(ctx context.Context) (NotifySettings, error) {
	var n NotifySettings
	var en, quiet int
	var secs, at string
	err := s.db.QueryRowContext(ctx,
		`SELECT enabled, at_time, sections, send_quiet, base_url, updated_by, updated_at FROM notify_config WHERE id=1`).
		Scan(&en, &n.At, &secs, &quiet, &n.BaseURL, &n.UpdatedBy, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultNotifySettings(), nil
	}
	if err != nil {
		return n, err
	}
	n.Enabled, n.SendQuiet = en == 1, quiet == 1
	n.UpdatedAt = parseTS(at)
	for _, sec := range strings.Split(secs, ",") {
		if sec = strings.TrimSpace(sec); sec != "" {
			n.Sections = append(n.Sections, sec)
		}
	}
	return n, nil
}

// PutNotifySettings saves the settings and audits who changed them.
func (s *Store) PutNotifySettings(ctx context.Context, n NotifySettings, by string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO notify_config (id, enabled, at_time, sections, send_quiet, base_url, updated_by, updated_at)
		 VALUES (1,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET enabled=excluded.enabled, at_time=excluded.at_time,
		   sections=excluded.sections, send_quiet=excluded.send_quiet, base_url=excluded.base_url,
		   updated_by=excluded.updated_by, updated_at=excluded.updated_at`,
		b2i(n.Enabled), n.At, strings.Join(n.Sections, ","), b2i(n.SendQuiet), n.BaseURL, by, ts(time.Now()))
	if err != nil {
		return err
	}
	what := "off"
	if n.Enabled {
		what = "daily at " + n.At + "; " + strings.Join(n.Sections, ",")
	}
	return s.Audit(ctx, by, "notify settings", "digest", what)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// The kinds of send recorded.
const (
	SendDigest  = "digest"
	SendTest    = "test"
	SendSkipped = "skipped"
)

// NotifySend is one attempt to deliver, after its retries.
type NotifySend struct {
	ID          int64
	At          time.Time
	Kind        string
	Dest        string
	Slot        string
	WindowStart time.Time
	WindowEnd   time.Time
	OK          bool
	Error       string
	Attempts    int
	Changes     int
	Failing     int
	Recovered   int
	Never       int
}

// RecordNotifySend appends a send to the history.
func (s *Store) RecordNotifySend(ctx context.Context, n NotifySend) error {
	if n.At.IsZero() {
		n.At = time.Now()
	}
	var ws, we string
	if !n.WindowStart.IsZero() {
		ws = ts(n.WindowStart)
	}
	if !n.WindowEnd.IsZero() {
		we = ts(n.WindowEnd)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO notify_send (at, kind, dest, slot, window_start, window_end, ok, error, attempts,
		   changes, failing, recovered, never) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		ts(n.At), n.Kind, n.Dest, n.Slot, ws, we, b2i(n.OK), n.Error, n.Attempts,
		n.Changes, n.Failing, n.Recovered, n.Never)
	return err
}

// NotifySends returns the most recent sends, newest first.
func (s *Store) NotifySends(ctx context.Context, limit int) ([]NotifySend, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, at, kind, dest, slot, window_start, window_end, ok, error, attempts,
		   changes, failing, recovered, never FROM notify_send ORDER BY at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotifySend
	for rows.Next() {
		var n NotifySend
		var at, ws, we string
		var ok int
		if err := rows.Scan(&n.ID, &at, &n.Kind, &n.Dest, &n.Slot, &ws, &we, &ok, &n.Error, &n.Attempts,
			&n.Changes, &n.Failing, &n.Recovered, &n.Never); err != nil {
			return nil, err
		}
		n.At, n.WindowStart, n.WindowEnd = parseTS(at), parseTS(ws), parseTS(we)
		n.OK = ok == 1
		out = append(out, n)
	}
	return out, rows.Err()
}

// DigestSlotDone says which destinations already had the digest for a slot,
// successfully or not. The empty name is present when the day was skipped as
// quiet.
func (s *Store) DigestSlotDone(ctx context.Context, slot string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT dest FROM notify_send WHERE slot=? AND kind IN (?,?)`, slot, SendDigest, SendSkipped)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out[d] = true
	}
	return out, rows.Err()
}

// LastDigestEnd is the end of the window the most recent digest covered, or
// zero when none was ever built. The next digest starts there.
func (s *Store) LastDigestEnd(ctx context.Context) (time.Time, error) {
	var we sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(window_end) FROM notify_send WHERE kind IN (?,?) AND window_end <> ''`,
		SendDigest, SendSkipped).Scan(&we)
	if err != nil || !we.Valid {
		return time.Time{}, err
	}
	return parseTS(we.String), nil
}

// HostRef names a live host.
type HostRef struct {
	CIID  string
	Label string
}

// HealthTransitions lists the live hosts whose latest collection went from
// working to failing between since and until, and those that went back. A
// host first collected in the window and failing counts as started failing;
// a host with no collection in the window is in neither list.
func (s *Store) HealthTransitions(ctx context.Context, since, until time.Time) (started, recovered []FailingHost, err error) {
	latest := func(cmp string) string {
		return `(SELECT p.%s FROM probe p WHERE p.ci_id=c.id AND p.kind='collector_run' AND p.at ` + cmp +
			` ? ORDER BY p.at DESC, p.id DESC LIMIT 1)`
	}
	now := latest("<=")
	before := latest("<")
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, c.label, `+fmt.Sprintf(now, "outcome")+`, `+fmt.Sprintf(now, "detail")+`, `+
			fmt.Sprintf(now, "at")+`, COALESCE(`+fmt.Sprintf(before, "outcome")+`, '')
		 FROM ci c
		 WHERE c.retired_at IS NULL AND c.kind='host'
		   AND EXISTS (SELECT 1 FROM probe p WHERE p.ci_id=c.id AND p.kind='collector_run' AND p.at >= ? AND p.at <= ?)
		 ORDER BY c.label`,
		ts(until), ts(until), ts(until), ts(since), ts(since), ts(until))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	bad := func(o string) bool { return o != "" && o != "ok" && o != "partial" }
	for rows.Next() {
		var f FailingHost
		var at, prev string
		if err := rows.Scan(&f.CIID, &f.Label, &f.Outcome, &f.Detail, &at, &prev); err != nil {
			return nil, nil, err
		}
		f.At = parseTS(at)
		switch {
		case bad(f.Outcome) && !bad(prev):
			started = append(started, f)
		case !bad(f.Outcome) && bad(prev):
			recovered = append(recovered, f)
		}
	}
	return started, recovered, rows.Err()
}

// NeverCollected lists up to limit live hosts never collected from, by
// label, and how many there are in all.
func (s *Store) NeverCollected(ctx context.Context, limit int) ([]HostRef, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ci c WHERE c.retired_at IS NULL AND c.kind='host' AND `+hostNeverSQL).Scan(&total); err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, c.label FROM ci c WHERE c.retired_at IS NULL AND c.kind='host' AND `+hostNeverSQL+
			` ORDER BY c.label LIMIT ?`, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []HostRef
	for rows.Next() {
		var h HostRef
		if err := rows.Scan(&h.CIID, &h.Label); err != nil {
			return nil, 0, err
		}
		out = append(out, h)
	}
	return out, total, rows.Err()
}
