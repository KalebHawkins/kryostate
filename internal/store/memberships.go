package store

import "context"

// AllMemberships maps every CI in a stack to its memberships, for filtering
// the whole inventory on stack and role without a query per host.
func (s *Store) AllMemberships(ctx context.Context) (map[string][]Membership, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.ci_id, s.name, m.role FROM stack_member m JOIN stack s ON s.id=m.stack_id
		 ORDER BY s.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Membership{}
	for rows.Next() {
		var id string
		var m Membership
		if err := rows.Scan(&id, &m.Stack, &m.Role); err != nil {
			return nil, err
		}
		out[id] = append(out[id], m)
	}
	return out, rows.Err()
}
