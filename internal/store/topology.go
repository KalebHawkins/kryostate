package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/KalebHawkins/hearsay/internal/model"
)

// Address records one answer a resolver gave for a CI. Every answer is kept.
// When a name returns several addresses, the tool refuses to guess which one a
// collector should use and raises it for a person instead.
type Address struct {
	Addr       string
	Family     string
	Resolver   string
	ObservedAt time.Time
	Selected   bool
	SelectedBy string
}

// RecordAddress stores a resolved address without choosing it.
func (s *Store) RecordAddress(ctx context.Context, ciID, addr, family, resolver string, at time.Time) error {
	if at.IsZero() {
		at = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO address (ci_id, addr, family, resolver, observed_at) VALUES (?,?,?,?,?)
		 ON CONFLICT(ci_id, addr) DO UPDATE SET observed_at=excluded.observed_at, resolver=excluded.resolver`,
		ciID, addr, family, resolver, ts(at))
	return err
}

// Addresses lists every address observed for a CI.
func (s *Store) Addresses(ctx context.Context, ciID string) ([]Address, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT addr, family, resolver, observed_at, selected, selected_by
		 FROM address WHERE ci_id=? ORDER BY addr`, ciID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Address
	for rows.Next() {
		var a Address
		var at string
		var sel int
		if err := rows.Scan(&a.Addr, &a.Family, &a.Resolver, &at, &sel, &a.SelectedBy); err != nil {
			return nil, err
		}
		a.ObservedAt = parseTS(at)
		a.Selected = sel == 1
		out = append(out, a)
	}
	return out, rows.Err()
}

// SelectAddress records a person's decision about which address a collector
// should use. The decision is attributed, because the next person will ask why.
func (s *Store) SelectAddress(ctx context.Context, ciID, addr, by string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE address SET selected=0, selected_by='' WHERE ci_id=?`, ciID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE address SET selected=1, selected_by=? WHERE ci_id=? AND addr=?`, by, ciID, addr)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("address not recorded for this item")
	}
	return tx.Commit()
}

// ConnProfile says how to reach a CI and which credential to ask for. It
// stores a reference, never a secret.
type ConnProfile struct {
	ID            int64
	CIID          string
	Transport     string
	Address       string
	Port          int
	Username      string
	CredentialRef string
	BastionRef    string
	HostKey       string
	// BastionHostKey is pinned separately. The same jump host may front many
	// targets, but the key is recorded per profile so that removing a host
	// removes its trust with it.
	BastionHostKey string
	HostKeyPolicy  string
	Enabled        bool
}

// PutConnProfile creates or replaces the profile for a CI and transport.
//
// An update that carries no host key leaves any established pin alone. Editing
// a port or a username is not a reason to start trusting a new key, and the
// CLI has no way to ask for a key it has not seen yet. Clearing a pin
// deliberately is what PinHostKey with an empty key is for.
func (s *Store) PutConnProfile(ctx context.Context, p ConnProfile) error {
	en := 0
	if p.Enabled {
		en = 1
	}
	if p.HostKeyPolicy == "" {
		p.HostKeyPolicy = "pinned"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO conn_profile (ci_id, transport, address, port, username, credential_ref, bastion_ref, host_key, bastion_host_key, host_key_policy, enabled, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(ci_id, transport) DO UPDATE SET
		   address=excluded.address, port=excluded.port, username=excluded.username,
		   credential_ref=excluded.credential_ref, bastion_ref=excluded.bastion_ref,
		   host_key=CASE WHEN excluded.host_key='' THEN conn_profile.host_key ELSE excluded.host_key END,
		   bastion_host_key=CASE WHEN excluded.bastion_host_key='' THEN conn_profile.bastion_host_key ELSE excluded.bastion_host_key END,
		   host_key_policy=excluded.host_key_policy,
		   enabled=excluded.enabled`,
		p.CIID, p.Transport, p.Address, p.Port, p.Username, p.CredentialRef,
		p.BastionRef, p.HostKey, p.BastionHostKey, p.HostKeyPolicy, en, ts(time.Now()))
	return err
}

// ConnProfiles returns the enabled profiles for a CI.
func (s *Store) ConnProfiles(ctx context.Context, ciID string) ([]ConnProfile, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ci_id, transport, address, port, username, credential_ref, bastion_ref, host_key, bastion_host_key, host_key_policy, enabled
		 FROM conn_profile WHERE ci_id=? ORDER BY transport`, ciID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanProfiles(rows)
}

// CollectTargets returns every enabled profile across the estate, which is the
// set a collection run may touch. A CI with no profile is listed in the
// inventory and is not scheduled: an unreachable entry must not become a hot
// retry loop, and must not quietly shrink the denominator either.
//
// Where somebody has chosen an address for a host, that choice is what gets
// dialled. A selection that did not change where the tool connects would be a
// button that does nothing.
func (s *Store) CollectTargets(ctx context.Context, transport string) ([]ConnProfile, error) {
	q := `SELECT p.id, p.ci_id, p.transport,
	             COALESCE((SELECT a.addr FROM address a WHERE a.ci_id=p.ci_id AND a.selected=1 LIMIT 1), p.address),
	             p.port, p.username, p.credential_ref,
	             p.bastion_ref, p.host_key, p.bastion_host_key, p.host_key_policy, p.enabled
	      FROM conn_profile p JOIN ci c ON c.id=p.ci_id
	      WHERE p.enabled=1 AND c.retired_at IS NULL`
	args := []any{}
	if transport != "" {
		q += ` AND p.transport=?`
		args = append(args, transport)
	}
	q += ` ORDER BY c.label`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanProfiles(rows)
}

func scanProfiles(rows *sql.Rows) ([]ConnProfile, error) {
	var out []ConnProfile
	for rows.Next() {
		var p ConnProfile
		var en int
		if err := rows.Scan(&p.ID, &p.CIID, &p.Transport, &p.Address, &p.Port, &p.Username,
			&p.CredentialRef, &p.BastionRef, &p.HostKey, &p.BastionHostKey,
			&p.HostKeyPolicy, &en); err != nil {
			return nil, err
		}
		p.Enabled = en == 1
		out = append(out, p)
	}
	return out, rows.Err()
}

// PinHostKey stores the key a host presented, so a later change is visible
// rather than silently accepted.
func (s *Store) PinHostKey(ctx context.Context, ciID, transport, key string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE conn_profile SET host_key=? WHERE ci_id=? AND transport=?`, key, ciID, transport)
	return err
}

// PinBastionHostKey stores the key the jump host presented.
func (s *Store) PinBastionHostKey(ctx context.Context, ciID, transport, key string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE conn_profile SET bastion_host_key=? WHERE ci_id=? AND transport=?`, key, ciID, transport)
	return err
}

// PutStack creates or updates a stack and returns its id.
func (s *Store) PutStack(ctx context.Context, st model.Stack) (int64, error) {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO stack (name, description, owner, environment) VALUES (?,?,?,?)
		 ON CONFLICT(name) DO UPDATE SET description=excluded.description,
		   owner=excluded.owner, environment=excluded.environment`,
		st.Name, st.Description, st.Owner, st.Environment)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.db.QueryRowContext(ctx, `SELECT id FROM stack WHERE name=?`, st.Name).Scan(&id)
	return id, err
}

// EnsureStack returns the id of a named stack, creating it bare if it does not
// exist. Unlike PutStack it never changes an existing stack, so a document
// that only names a stack cannot erase the owner and description a person
// recorded for it.
func (s *Store) EnsureStack(ctx context.Context, name string) (int64, error) {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO stack (name, description, owner, environment) VALUES (?,'','','')
		 ON CONFLICT(name) DO NOTHING`, name); err != nil {
		return 0, err
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM stack WHERE name=?`, name).Scan(&id)
	return id, err
}

// AddToStack records membership. A host may belong to several stacks: a shared
// database serves more than one deployed system, and pretending otherwise
// files it under whichever one was imported last.
func (s *Store) AddToStack(ctx context.Context, stackID int64, ciID, role string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO stack_member (stack_id, ci_id, role) VALUES (?,?,?)
		 ON CONFLICT(stack_id, ci_id) DO UPDATE SET role=excluded.role`,
		stackID, ciID, role)
	return err
}

// RemoveFromStack ends a membership. It reports whether there was one, so a
// mistyped host is not silently treated as removed.
func (s *Store) RemoveFromStack(ctx context.Context, stackID int64, ciID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM stack_member WHERE stack_id=? AND ci_id=?`, stackID, ciID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// StackID returns the id of an existing stack, or an error naming it.
func (s *Store) StackID(ctx context.Context, name string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM stack WHERE name=?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errors.New("no stack named " + name)
	}
	return id, err
}

// Membership is one stack a host belongs to, and the role it plays there.
type Membership struct {
	Stack string
	Role  string
}

// MembershipsOf lists the stacks a CI belongs to.
func (s *Store) MembershipsOf(ctx context.Context, ciID string) ([]Membership, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.name, m.role FROM stack_member m JOIN stack s ON s.id=m.stack_id
		 WHERE m.ci_id=? ORDER BY s.name`, ciID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Membership
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.Stack, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteStack removes a stack and its memberships. The hosts themselves, and
// everything asserted about them, are untouched: a stack is a grouping, not a
// container.
func (s *Store) DeleteStack(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM stack WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("no stack named " + name)
	}
	// The schema cascades, but only when foreign keys are enforced on this
	// connection; deleting explicitly does not depend on that.
	_, err = s.db.ExecContext(ctx,
		`DELETE FROM stack_member WHERE stack_id NOT IN (SELECT id FROM stack)`)
	return err
}

// DeleteConnProfile removes how a host is reached over one transport. Probes
// already recorded through it stay in the history.
func (s *Store) DeleteConnProfile(ctx context.Context, ciID, transport string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM conn_profile WHERE ci_id=? AND transport=?`, ciID, transport)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("no " + transport + " connection for this host")
	}
	return nil
}

// StackView is a stack with its membership counts.
type StackView struct {
	Stack  model.Stack
	Health model.StackHealth
}

// Stacks returns every stack with a count breakdown of its members. There is
// no single health colour: a stack with eleven of fourteen members collected
// is neither up nor down, and rolling it into one light is how a dashboard
// starts lying.
func (s *Store) Stacks(ctx context.Context) ([]StackView, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, description, owner, environment FROM stack ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StackView
	for rows.Next() {
		var v StackView
		if err := rows.Scan(&v.Stack.ID, &v.Stack.Name, &v.Stack.Description,
			&v.Stack.Owner, &v.Stack.Environment); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		h, err := s.stackHealth(ctx, out[i].Stack.ID)
		if err != nil {
			return nil, err
		}
		out[i].Health = h
	}
	return out, nil
}

func (s *Store) stackHealth(ctx context.Context, stackID int64) (model.StackHealth, error) {
	var h model.StackHealth
	err := s.db.QueryRowContext(ctx, `
		SELECT
		  COUNT(*),
		  COALESCE(SUM(CASE WHEN last.outcome='ok' THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN last.outcome IS NOT NULL AND last.outcome<>'ok' THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN last.outcome IS NULL THEN 1 ELSE 0 END), 0)
		FROM stack_member m
		JOIN ci c ON c.id=m.ci_id AND c.retired_at IS NULL
		LEFT JOIN (
		  SELECT p.ci_id, p.outcome FROM probe p
		  WHERE p.kind='collector_run'
		    AND p.id=(SELECT q.id FROM probe q WHERE q.ci_id=p.ci_id AND q.kind='collector_run'
		              ORDER BY q.at DESC, q.id DESC LIMIT 1)
		) last ON last.ci_id=m.ci_id
		WHERE m.stack_id=?`, stackID).
		Scan(&h.Members, &h.Collected, &h.Failed, &h.NeverSeen)
	if err != nil {
		return h, err
	}
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM discrepancy d JOIN stack_member m ON m.ci_id=d.ci_id
		 WHERE m.stack_id=? AND d.disposition='open'`, stackID).Scan(&h.Discrepancy)
	return h, err
}

// StackMembers lists the CIs in a stack with their role in it.
func (s *Store) StackMembers(ctx context.Context, name string) ([]HostRow, error) {
	return s.Hosts(ctx, HostFilter{Stack: name})
}
