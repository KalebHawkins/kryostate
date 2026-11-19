package store

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/query"
)

// QueryHosts loads every host as a query sees it: its name, id, stacks,
// roles, whether it is disabled and its current host-wide values. It takes
// three statements however many hosts there are, so it is what deciding where
// collectors run, and counting the hosts a target matches, are built on.
// Disabled hosts are included; what contacts hosts leaves them out itself.
func (s *Store) QueryHosts(ctx context.Context) ([]query.Host, error) {
	return s.queryHosts(ctx, nil, "")
}

// QueryHost is one host as QueryHosts would list it, with every value, or
// nothing when there is no such host. It is for deciding what runs on one
// host without loading the estate.
func (s *Store) QueryHost(ctx context.Context, id string) ([]query.Host, error) {
	if id == "" {
		return nil, nil
	}
	return s.queryHosts(ctx, nil, id)
}

// QueryHostsFor is QueryHosts with only the values of the given attributes
// loaded, for evaluating queries that read nothing else: see
// query.Expr.ValueKeys. Every host is still listed, with its name, stacks,
// roles and whether it is disabled.
func (s *Store) QueryHostsFor(ctx context.Context, attrs []string) ([]query.Host, error) {
	if attrs == nil {
		attrs = []string{}
	}
	return s.queryHosts(ctx, attrs, "")
}

// queryHosts loads the hosts with the values of attrs, or of every
// attribute when attrs is nil: every host, or only the one with id.
func (s *Store) queryHosts(ctx context.Context, attrs []string, id string) ([]query.Host, error) {
	hostQ, args := `SELECT id, label, disabled_at IS NOT NULL FROM ci WHERE kind='host'`, []any{}
	if id != "" {
		hostQ += ` AND id=?`
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, hostQ+` ORDER BY label`, args...)
	if err != nil {
		return nil, err
	}
	var out []query.Host
	at := map[string]int{}
	for rows.Next() {
		var h query.Host
		if err := rows.Scan(&h.ID, &h.Name, &h.Disabled); err != nil {
			rows.Close()
			return nil, err
		}
		h.Values = map[string]string{}
		at[h.ID] = len(out)
		out = append(out, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	q := `SELECT f.ci_id, f.attr, CASE WHEN f.norm_value<>'' THEN f.norm_value ELSE f.raw_value END
		 FROM fact_now f WHERE f.scope=''`
	args = nil
	if id != "" {
		// One host's values come off its own index; the table-order read
		// below is for all of them.
		q += ` AND f.ci_id=?`
		args = append(args, id)
	}
	if attrs != nil {
		list, err := json.Marshal(attrs)
		if err != nil {
			return nil, err
		}
		q += ` AND f.attr IN (SELECT value FROM json_each(?))`
		args = append(args, string(list))
	}
	if id == "" {
		q += " " + inTableOrder
	}
	rows, err = s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, attr, v string
		if err := rows.Scan(&id, &attr, &v); err != nil {
			rows.Close()
			return nil, err
		}
		if i, ok := at[id]; ok {
			out[i].Values[attr] = v
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	members, err := s.AllMemberships(ctx)
	if err != nil {
		return nil, err
	}
	for id, ms := range members {
		i, ok := at[id]
		if !ok {
			continue
		}
		for _, m := range ms {
			out[i].Stacks = append(out[i].Stacks, m.Stack)
			if m.Role != "" {
				out[i].Roles = append(out[i].Roles, m.Role)
			}
		}
	}
	return out, nil
}

// HostName is a host's id and name, for pickers.
type HostName struct{ ID, Name string }

// FindHosts lists hosts whose name contains text, ignoring case, in name
// order, at most limit of them, with how many match in all. It is what a
// type-ahead asks, so it never loads more than it shows.
func (s *Store) FindHosts(ctx context.Context, text string, limit int) ([]HostName, int, error) {
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.TrimSpace(text)) + "%"
	const where = `FROM ci WHERE kind='host' AND label LIKE ? ESCAPE '\'`
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) `+where, like).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, label `+where+` ORDER BY label COLLATE NOCASE LIMIT ?`, like, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []HostName
	for rows.Next() {
		var h HostName
		if err := rows.Scan(&h.ID, &h.Name); err != nil {
			return nil, 0, err
		}
		out = append(out, h)
	}
	return out, total, rows.Err()
}

// QueryResolver names the attribute a parameterised collector records,
// honouring a name a person chose when switching it on.
func (s *Store) QueryResolver(ctx context.Context) query.Resolver {
	choices, _ := s.Choices(ctx)
	return func(tmpl, param string) string {
		for _, c := range choices {
			if c.Template == tmpl && c.Param == param && c.Attr != "" {
				return c.Attr
			}
		}
		t, _ := collect.Lookup(tmpl)
		return collect.DefaultAttr(t, param)
	}
}
