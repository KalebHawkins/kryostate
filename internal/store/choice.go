package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/query"
)

// PutChoice switches a catalog collector on, or changes where an existing one
// runs. The value is checked here as well as when it is rendered, so a bad
// value is refused when someone types it rather than at collection time. So
// is a target that does not parse, and a target that only names stacks must
// name stacks that exist, as a mistyped stack name would otherwise quietly
// collect nothing.
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
	c.Target = strings.TrimSpace(c.Target)
	if err := s.checkTarget(ctx, c.Target); err != nil {
		return c, err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO collector_choice (template, param, attr, target, created_by, created_at)
		 VALUES (?,?,?,?,?,?)
		 ON CONFLICT(template, param) DO UPDATE SET attr=excluded.attr, target=excluded.target, stacks=''`,
		c.Template, c.Param, c.Attr, c.Target, by, ts(time.Now()))
	if err != nil {
		return c, err
	}
	err = s.db.QueryRowContext(ctx,
		`SELECT id FROM collector_choice WHERE template=? AND param=?`, c.Template, c.Param).Scan(&c.ID)
	if err != nil {
		return c, err
	}
	where := "all hosts"
	if c.Target != "" {
		where = c.Target
	}
	return c, s.Audit(ctx, by, "collector on", c.Label(), c.Attr+" where "+where)
}

// checkTarget refuses a target that does not parse, and one made only of
// stacks naming a stack that does not exist.
func (s *Store) checkTarget(ctx context.Context, target string) error {
	if target == "" {
		return nil
	}
	if _, err := query.Parse(target); err != nil {
		return errors.New("where it runs: " + err.Error())
	}
	if stacks, ok := query.ListOf(target, "stack"); ok {
		for _, name := range stacks {
			if _, err := s.StackID(ctx, name); err != nil {
				return err
			}
		}
	}
	return nil
}

// Choices lists the catalog collectors switched on.
func (s *Store) Choices(ctx context.Context) ([]collect.Choice, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, template, param, attr, target FROM collector_choice ORDER BY template, param`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []collect.Choice
	for rows.Next() {
		var c collect.Choice
		if err := rows.Scan(&c.ID, &c.Template, &c.Param, &c.Attr, &c.Target); err != nil {
			return nil, err
		}
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
// every host, one limited to stacks gains the new stacks, and any other
// target is widened with ||. A blank target means every host. It reports
// whether anything changed.
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
		widened, changed := widen(have.Target, strings.TrimSpace(c.Target))
		if !changed {
			return have, false, nil
		}
		have.Target = widened
		if c.Attr != "" {
			have.Attr = c.Attr
		}
		out, err := s.PutChoice(ctx, have, by)
		return out, err == nil, err
	}
	out, err := s.PutChoice(ctx, c, by)
	return out, err == nil, err
}

// widen returns a target that matches every host either one does.
func widen(have, add string) (string, bool) {
	switch {
	case have == "":
		return "", false
	case add == "":
		return "", true
	case slices.Contains(query.Alternatives(have), add):
		return have, false
	}
	a, okA := query.ListOf(have, "stack")
	b, okB := query.ListOf(add, "stack")
	if okA && okB {
		merged := cleanList(append(slices.Clone(a), b...))
		if len(merged) == len(a) {
			return have, false
		}
		return query.In("stack", merged), true
	}
	return query.Or(have, add), true
}

// StacksTarget is the target that limits a collector to members of some
// stacks, as older versions stored it.
func StacksTarget(stacks []string) string { return query.In("stack", cleanList(stacks)) }

// migrateChoiceStacks turns the stack lists older versions stored into
// targets, once, so that the target is the only thing deciding where a
// collector runs.
func (s *Store) migrateChoiceStacks(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, stacks, target FROM collector_choice WHERE stacks<>''`)
	if err != nil {
		return err
	}
	type upd struct {
		id     int64
		target string
	}
	var todo []upd
	for rows.Next() {
		var u upd
		var stacks, target string
		if err := rows.Scan(&u.id, &stacks, &target); err != nil {
			rows.Close()
			return err
		}
		u.target = query.And(target, StacksTarget(strings.Split(stacks, ",")))
		todo = append(todo, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, u := range todo {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE collector_choice SET target=?, stacks='' WHERE id=?`, u.target, u.id); err != nil {
			return err
		}
	}
	return nil
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
