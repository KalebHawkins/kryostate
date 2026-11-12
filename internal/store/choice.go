package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/KalebHawkins/kryostate/internal/collect"
)

// PutChoice switches a catalog collector on, or changes where an existing one
// runs. The value is checked here as well as when it is rendered, so a bad
// value is refused when someone types it rather than at collection time.
func (s *Store) PutChoice(ctx context.Context, c collect.Choice, by string) (collect.Choice, error) {
	t, ok := collect.Lookup(c.Template)
	if !ok {
		return c, errors.New("no catalog collector named " + c.Template)
	}
	c.Param = strings.TrimSpace(c.Param)
	if t.Fixed() {
		c.Param = ""
	} else if err := collect.CheckParam(t.Param.Kind, c.Param); err != nil {
		return c, err
	}
	c.Attr = strings.TrimSpace(c.Attr)
	if c.Attr == "" {
		c.Attr = collect.DefaultAttr(t, c.Param)
	}
	if err := collect.CheckAttr(c.Attr); err != nil {
		return c, err
	}
	stacks := cleanList(c.Stacks)
	for _, name := range stacks {
		if _, err := s.StackID(ctx, name); err != nil {
			return c, err
		}
	}
	c.Stacks = stacks
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO collector_choice (template, param, attr, stacks, created_by, created_at)
		 VALUES (?,?,?,?,?,?)
		 ON CONFLICT(template, param) DO UPDATE SET attr=excluded.attr, stacks=excluded.stacks`,
		c.Template, c.Param, c.Attr, strings.Join(stacks, ","), by, ts(time.Now()))
	if err != nil {
		return c, err
	}
	err = s.db.QueryRowContext(ctx,
		`SELECT id FROM collector_choice WHERE template=? AND param=?`, c.Template, c.Param).Scan(&c.ID)
	if err != nil {
		return c, err
	}
	return c, s.Audit(ctx, by, "collector on", c.Label(), c.Attr+" stacks="+strings.Join(stacks, ","))
}

// Choices lists the catalog collectors switched on.
func (s *Store) Choices(ctx context.Context) ([]collect.Choice, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, template, param, attr, stacks FROM collector_choice ORDER BY template, param`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []collect.Choice
	for rows.Next() {
		var c collect.Choice
		var stacks string
		if err := rows.Scan(&c.ID, &c.Template, &c.Param, &c.Attr, &stacks); err != nil {
			return nil, err
		}
		c.Stacks = cleanList(strings.Split(stacks, ","))
		out = append(out, c)
	}
	return out, rows.Err()
}

// Choice returns one switched-on collector.
func (s *Store) Choice(ctx context.Context, id int64) (collect.Choice, error) {
	cs, err := s.Choices(ctx)
	if err != nil {
		return collect.Choice{}, err
	}
	for _, c := range cs {
		if c.ID == id {
			return c, nil
		}
	}
	return collect.Choice{}, sql.ErrNoRows
}

// EnableChoice switches a catalog collector on for some more hosts without
// taking it away from any. It differs from PutChoice, which sets where a
// collector runs: here a collector already on for every host stays on for
// every host, and one limited to stacks gains the new stacks. No stacks
// means every host. It reports whether anything changed.
func (s *Store) EnableChoice(ctx context.Context, c collect.Choice, by string) (collect.Choice, bool, error) {
	cs, err := s.Choices(ctx)
	if err != nil {
		return c, false, err
	}
	param := strings.TrimSpace(c.Param)
	if t, ok := collect.Lookup(c.Template); ok && t.Fixed() {
		param = ""
	}
	for _, have := range cs {
		if have.Template != c.Template || have.Param != param {
			continue
		}
		if len(have.Stacks) == 0 {
			return have, false, nil
		}
		if len(c.Stacks) == 0 {
			have.Stacks = nil
		} else {
			merged := cleanList(append(append([]string{}, have.Stacks...), c.Stacks...))
			if len(merged) == len(have.Stacks) {
				return have, false, nil
			}
			have.Stacks = merged
		}
		if c.Attr != "" {
			have.Attr = c.Attr
		}
		out, err := s.PutChoice(ctx, have, by)
		return out, err == nil, err
	}
	out, err := s.PutChoice(ctx, c, by)
	return out, err == nil, err
}

// DeleteChoice switches a catalog collector off. Values it already collected
// stay in the history.
func (s *Store) DeleteChoice(ctx context.Context, id int64, by string) error {
	c, err := s.Choice(ctx, id)
	if err != nil {
		return errors.New("that collector is not switched on")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM collector_choice WHERE id=?`, id); err != nil {
		return err
	}
	return s.Audit(ctx, by, "collector off", c.Label(), c.Attr)
}

func cleanList(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
