package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/KalebHawkins/hearsay/internal/model"
)

// ErrNoSuchHost is returned when a reference matches nothing.
var ErrNoSuchHost = errors.New("no configuration item matches")

// minIDPrefix is the shortest id prefix accepted on its own. Shorter than this
// and a prefix starts colliding with the hex-looking parts of real hostnames.
const minIDPrefix = 6

// LookupCI turns what a person typed into one configuration item id.
//
// People think in hostnames, and the opaque id exists precisely so that
// hostnames are not the key. This bridges the two without giving that up: a
// name is used to find the item, but a name that matches two items is an
// error that lists both, never a guess. The reference may be the full id, an
// unambiguous id prefix of at least six characters, any current identity
// value (fqdn, hostname, serial, machine id), or the display label. Retired
// items are included so that unretiring one can be done by name.
func (s *Store) LookupCI(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", errors.New("empty host reference")
	}

	// An exact id is unambiguous by construction, so it is tried alone first.
	var id string
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM ci WHERE id=?`, ref).Scan(&id); err == nil {
		return id, nil
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM ci WHERE label = ? COLLATE NOCASE
		UNION
		SELECT ci_id FROM identity WHERE value = ? COLLATE NOCASE AND valid_to IS NULL
		UNION
		SELECT id FROM ci WHERE length(?) >= ? AND id LIKE ? || '%'`,
		ref, ref, ref, minIDPrefix, strings.ToLower(ref))
	if err != nil {
		return "", err
	}
	var ids []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return "", err
		}
		ids = append(ids, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}

	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		hint := s.suggest(ctx, ref)
		if hint != "" {
			return "", fmt.Errorf("%w %q; did you mean %s?", ErrNoSuchHost, ref, hint)
		}
		return "", fmt.Errorf("%w %q; list hosts with: hearsay hosts", ErrNoSuchHost, ref)
	default:
		sort.Strings(ids)
		var b strings.Builder
		for _, v := range ids {
			c, err := s.CI(ctx, v)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "\n  %s  %s", c.ID, c.Label)
			if c.RetiredAt != nil {
				b.WriteString(" (retired)")
			}
		}
		return "", fmt.Errorf("%w: %q matches %d items; use the id instead:%s",
			ErrAmbiguousIdentity, ref, len(ids), b.String())
	}
}

// LookupCIs resolves several references, stopping at the first that fails.
func (s *Store) LookupCIs(ctx context.Context, refs []string) ([]string, error) {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		id, err := s.LookupCI(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

// suggest names up to three live hosts whose label contains the reference,
// for the error message when nothing matched exactly.
func (s *Store) suggest(ctx context.Context, ref string) string {
	rows, err := s.db.QueryContext(ctx,
		`SELECT label FROM ci WHERE retired_at IS NULL AND label LIKE ? ORDER BY label LIMIT 3`,
		"%"+ref+"%")
	if err != nil {
		return ""
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			names = append(names, n)
		}
	}
	if rows.Err() != nil {
		return ""
	}
	return strings.Join(names, ", ")
}

// Claim records a value a named person states directly, outside of settling a
// disagreement. It is an ordinary human assertion: it competes under the
// precedence table like any other source, which is what lets env, site and
// owner, where a person outranks every machine, be set at all. The reason is
// written to the audit trail.
func (s *Store) Claim(ctx context.Context, ciID, attr, scope, value, by, reason string) error {
	if strings.TrimSpace(attr) == "" {
		return errors.New("a claim needs an attribute")
	}
	srcID, err := s.SourceID(ctx, model.SourceHuman, by)
	if err != nil {
		return err
	}
	if _, err := s.Assert(ctx, model.Assertion{
		CIID: ciID, Attr: attr, Scope: scope, Raw: value,
		SourceID: srcID, ObservedAt: time.Now(),
	}, s.normalizedFor(ctx, attr, value)); err != nil {
		return err
	}
	detail := attr + "=" + value
	if scope != "" {
		detail = attr + "[" + scope + "]=" + value
	}
	if reason != "" {
		detail += ": " + reason
	}
	return s.Audit(ctx, by, "claim", ciID, detail)
}
