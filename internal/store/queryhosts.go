package store

import (
	"context"
	"strings"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/query"
)

// QueryHosts loads every live host as a query sees it: its name, id, stacks,
// roles and current host-wide values. It takes three statements however many
// hosts there are, so it is what deciding where collectors run, and counting
// the hosts a target matches, are built on.
func (s *Store) QueryHosts(ctx context.Context) ([]query.Host, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, label FROM ci WHERE kind='host' AND retired_at IS NULL ORDER BY label`)
	if err != nil {
		return nil, err
	}
	var out []query.Host
	at := map[string]int{}
	for rows.Next() {
		var h query.Host
		if err := rows.Scan(&h.ID, &h.Name); err != nil {
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

	rows, err = s.db.QueryContext(ctx,
		`SELECT ci_id, attr, CASE WHEN norm_value<>'' THEN norm_value ELSE raw_value END
		 FROM current_fact WHERE scope=''`)
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

// HostName is a live host's id and name, for pickers.
type HostName struct{ ID, Name string }

// FindHosts lists live hosts whose name contains text, ignoring case, in name
// order, at most limit of them, with how many match in all. It is what a
// type-ahead asks, so it never loads more than it shows.
func (s *Store) FindHosts(ctx context.Context, text string, limit int) ([]HostName, int, error) {
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.TrimSpace(text)) + "%"
	const where = `FROM ci WHERE kind='host' AND retired_at IS NULL AND label LIKE ? ESCAPE '\'`
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
