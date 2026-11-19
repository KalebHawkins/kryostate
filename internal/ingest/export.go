package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"go.yaml.in/yaml/v3"

	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// Example is a commented import file, for kryostate import --example.
const Example = `# A kryostate import file. Import it with: kryostate import hosts.yaml
# JSON with the same structure works too. Importing again changes only what
# differs; hosts, connections and stack memberships not in the file are left
# alone, never removed.

# Optional. Applies to every host's target unless the host says otherwise.
defaults:
  transport: ssh            # ssh or winrm
  user: ops
  # A reference, never a password: agent:, keyring:service/account,
  # env:NAME, file:PATH or exec:COMMAND. Quote agent: as "agent:", because
  # YAML does not accept a value that ends in a colon. Store a keyring
  # password with
  #   kryostate secret set kryostate/ops
  credential: keyring:kryostate/ops
  port: 22

# Optional. Details for stacks the hosts name. A stack named only by a host
# is created bare; details recorded by hand are kept unless given here.
stacks:
  - name: payments-prod
    description: Payment services
    owner: team-payments
    environment: prod

hosts:
  - name: web01
    fqdn: web01.example.com   # optional; also the address when none is given
    application: ledger       # optional
    stacks:                   # optional: "stack", "stack:role" or {stack, role}
      - payments-prod:app
      - {stack: edge, role: proxy}
    target:                   # optional; merged over defaults
      user: svc-ro
      address: 192.0.2.10
      bastion: "ops@jump.example.com:22/agent:"   # quoted: it ends in a colon
  - name: db01
    stacks: [payments-prod:database]
  - name: lab01
    target: false             # no connection: listed, never collected
    disabled: true            # optional; true disables, false enables
`

// Encode writes a file in a format.
func Encode(f *File, format string) ([]byte, error) {
	switch format {
	case FormatJSON:
		b, err := json.MarshalIndent(f, "", "  ")
		return append(b, '\n'), err
	case FormatYAML:
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(f); err != nil {
			return nil, err
		}
		return buf.Bytes(), enc.Close()
	}
	return nil, fmt.Errorf("unknown format %q: use yaml or json", format)
}

// FromStore writes the recorded hosts, their connections and stacks as an
// import file, so the inventory can be kept in a file from here on. A host
// with more than one connection keeps its ssh one in the file; the others are
// named in the returned notes.
func FromStore(ctx context.Context, st *store.Store) (*File, []string, error) {
	f := &File{}
	var notes []string
	views, err := st.Stacks(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, v := range views {
		s := v.Stack
		if s.Description != "" || s.Owner != "" || s.Environment != "" {
			f.Stacks = append(f.Stacks, Stack{Name: s.Name, Description: s.Description, Owner: s.Owner, Environment: s.Environment})
		}
	}
	hosts, err := st.Hosts(ctx, store.HostFilter{})
	if err != nil {
		return nil, nil, err
	}
	members, err := st.AllMemberships(ctx)
	if err != nil {
		return nil, nil, err
	}
	var targets []*Target
	for _, row := range hosts {
		h := Host{Name: row.CI.Label}
		ids, err := st.Identities(ctx, row.CI.ID)
		if err != nil {
			return nil, nil, err
		}
		for _, i := range ids {
			if i.Scheme == model.IdentFQDN && h.FQDN == "" {
				h.FQDN = i.Value
			}
		}
		facts, err := st.Facts(ctx, row.CI.ID)
		if err != nil {
			return nil, nil, err
		}
		for _, fc := range facts {
			// Only an application a person stated belongs in the file; what
			// the host reports is collected again.
			if fc.Attr == model.AttrApplication && fc.Scope == "" &&
				(fc.SourceKind == model.SourceHuman || fc.SourceKind == model.SourceDocument) {
				h.Application = fc.Raw
			}
		}
		for _, m := range members[row.CI.ID] {
			h.Stacks = append(h.Stacks, Membership{Stack: m.Stack, Role: m.Role})
		}
		if row.CI.DisabledAt != nil {
			t := true
			h.Disabled = &t
		}
		profiles, err := st.ConnProfiles(ctx, row.CI.ID)
		if err != nil {
			return nil, nil, err
		}
		if len(profiles) > 0 {
			pr := profiles[0]
			for _, p := range profiles {
				if p.Transport == "ssh" {
					pr = p
				}
			}
			for _, p := range profiles {
				if p.Transport != pr.Transport {
					notes = append(notes, fmt.Sprintf("%s: its %s connection is not in the file, which holds one per host", h.Name, p.Transport))
				}
			}
			t := Target{Transport: pr.Transport, Port: pr.Port, User: pr.Username,
				Credential: pr.CredentialRef, Bastion: pr.BastionRef, Disabled: !pr.Enabled}
			want := h.FQDN
			if want == "" {
				want = defaultAddress(ctx, st, row.CI.ID, row.CI.Label)
			}
			if pr.Address != want {
				t.Address = pr.Address
			}
			h.Target = &HostTarget{Target: t}
			targets = append(targets, &h.Target.Target)
		}
		f.Hosts = append(f.Hosts, h)
	}
	f.Defaults = factorDefaults(targets)
	if f.Defaults != nil {
		for i := range f.Hosts {
			if f.Hosts[i].Target == nil {
				f.Hosts[i].Target = &HostTarget{None: true}
			}
		}
	}
	for i := range f.Hosts {
		if t := f.Hosts[i].Target; t != nil && !t.None && t.Target == (Target{}) {
			f.Hosts[i].Target = nil
		}
	}
	return f, notes, nil
}

// factorDefaults moves what every target shares into defaults, so the file
// says it once.
func factorDefaults(ts []*Target) *Target {
	if len(ts) < 2 {
		return nil
	}
	d := Target{}
	shared := func(get func(*Target) string, set func(*Target, string)) {
		v := get(ts[0])
		for _, t := range ts[1:] {
			if get(t) != v {
				return
			}
		}
		set(&d, v)
		for _, t := range ts {
			set(t, "")
		}
	}
	shared(func(t *Target) string { return t.Transport }, func(t *Target, v string) { t.Transport = v })
	shared(func(t *Target) string { return t.User }, func(t *Target, v string) { t.User = v })
	shared(func(t *Target) string { return t.Credential }, func(t *Target, v string) { t.Credential = v })
	shared(func(t *Target) string { return t.Bastion }, func(t *Target, v string) { t.Bastion = v })
	shared(func(t *Target) string { return fmt.Sprint(t.Port) }, func(t *Target, v string) {
		var p int
		fmt.Sscan(v, &p)
		t.Port = p
	})
	if d == (Target{}) {
		return nil
	}
	return &d
}
