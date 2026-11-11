package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Check is a liveness test an operator defined for one CI.
type Check struct {
	ID      int64
	CIID    string
	Name    string
	Kind    string // tcp or http
	Target  string
	Expect  string
	Timeout time.Duration
	Enabled bool
	Label   string // the CI's display name, filled by AllChecks
}

// Validate refuses a check that cannot mean anything, at the point somebody
// types it rather than at two in the morning when it fires.
func (c *Check) Validate() error {
	c.Name = strings.TrimSpace(c.Name)
	c.Target = strings.TrimSpace(c.Target)
	switch {
	case c.CIID == "":
		return errors.New("a check belongs to a configuration item")
	case c.Name == "":
		return errors.New("a check needs a name, because a host may have several")
	case c.Target == "":
		return errors.New("a check needs something to check")
	}
	switch c.Kind {
	case "tcp":
		if _, _, err := splitHostPort(c.Target); err != nil {
			return fmt.Errorf("tcp check %s: %w", c.Name, err)
		}
	case "http":
		u, err := url.Parse(c.Target)
		if err != nil {
			return fmt.Errorf("http check %s: %w", c.Name, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("http check %s: %q is not an http or https URL", c.Name, c.Target)
		}
		if c.Expect == "" {
			c.Expect = "2xx"
		}
		if !validExpect(c.Expect) {
			return fmt.Errorf("http check %s: expect %q is not a status code or a pattern like 2xx", c.Name, c.Expect)
		}
	default:
		return fmt.Errorf("check %s: kind must be tcp or http, got %q", c.Name, c.Kind)
	}
	if c.Timeout <= 0 {
		c.Timeout = 5 * time.Second
	}
	return nil
}

func validExpect(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case i > 0 && (r == 'x' || r == 'X'):
		default:
			return false
		}
	}
	return true
}

func splitHostPort(s string) (string, string, error) {
	i := strings.LastIndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return "", "", fmt.Errorf("%q is not host:port", s)
	}
	return s[:i], s[i+1:], nil
}

// PutCheck creates or replaces a check.
func (s *Store) PutCheck(ctx context.Context, c Check) error {
	if err := c.Validate(); err != nil {
		return err
	}
	en := 0
	if c.Enabled {
		en = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO check_def (ci_id, name, kind, target, expect, timeout_ms, enabled, created_at)
		 VALUES (?,?,?,?,?,?,?,?)
		 ON CONFLICT(ci_id, name) DO UPDATE SET
		   kind=excluded.kind, target=excluded.target, expect=excluded.expect,
		   timeout_ms=excluded.timeout_ms, enabled=excluded.enabled`,
		c.CIID, c.Name, c.Kind, c.Target, c.Expect, c.Timeout.Milliseconds(), en, ts(time.Now()))
	return err
}

// DeleteCheck removes a check.
func (s *Store) DeleteCheck(ctx context.Context, ciID, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM check_def WHERE ci_id=? AND name=?`, ciID, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no check named %q on %s", name, ciID)
	}
	return nil
}

// Checks lists the checks defined for one CI, enabled or not, because a
// disabled check is something somebody turned off and will want to find again.
func (s *Store) Checks(ctx context.Context, ciID string) ([]Check, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ci_id, name, kind, target, expect, timeout_ms, enabled
		 FROM check_def WHERE ci_id=? ORDER BY name`, ciID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanChecks(rows, nil)
}

// AllChecks lists every enabled check across live hosts, with the host label.
func (s *Store) AllChecks(ctx context.Context) ([]Check, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT k.id, k.ci_id, k.name, k.kind, k.target, k.expect, k.timeout_ms, k.enabled, c.label
		 FROM check_def k JOIN ci c ON c.id=k.ci_id
		 WHERE k.enabled=1 AND c.retired_at IS NULL
		 ORDER BY c.label, k.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var label string
	return scanChecks(rows, &label)
}

type rowScanner interface {
	Next() bool
	Scan(...any) error
	Err() error
}

func scanChecks(rows rowScanner, withLabel *string) ([]Check, error) {
	var out []Check
	for rows.Next() {
		var c Check
		var ms int64
		var en int
		var err error
		if withLabel != nil {
			err = rows.Scan(&c.ID, &c.CIID, &c.Name, &c.Kind, &c.Target, &c.Expect, &ms, &en, &c.Label)
		} else {
			err = rows.Scan(&c.ID, &c.CIID, &c.Name, &c.Kind, &c.Target, &c.Expect, &ms, &en)
		}
		if err != nil {
			return nil, err
		}
		c.Timeout = time.Duration(ms) * time.Millisecond
		c.Enabled = en == 1
		out = append(out, c)
	}
	return out, rows.Err()
}
