package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// Options controls planning and applying an import file.
type Options struct {
	// By is who is importing, for the audit trail.
	By string
	// Source names the file in the audit trail.
	Source string
	// CredentialMissing reports whether a keyring reference is not in the
	// OS credential store. Nil skips the check.
	CredentialMissing func(ref string) bool
}

// Action is what an import does to one thing.
type Action string

// Actions.
const (
	Create    Action = "create"
	Update    Action = "update"
	Unchanged Action = "unchanged"
)

// Plan is everything an import file would change, worked out before anything
// is written.
type Plan struct {
	Hosts    []HostPlan
	Stacks   []StackPlan
	Warnings []string
}

// HostPlan is what happens to one host.
type HostPlan struct {
	Host   Host
	ID     string
	Action Action
	// Changes describe changes to the host itself.
	Changes []string
	addFQDN bool
	setApp  string
	disable *bool
	// Target is nil when the host gets no connection from the file.
	Target      *TargetPlan
	TargetNote  string
	Memberships []MembershipPlan
	warnings    []string
}

// TargetPlan is the connection a host gets.
type TargetPlan struct {
	Action  Action
	Profile store.ConnProfile
	Changes []string
}

// MembershipPlan is one stack membership.
type MembershipPlan struct {
	Membership
	Action  Action
	OldRole string
}

// StackPlan is one stack the file creates or describes.
type StackPlan struct {
	Stack   model.Stack
	Action  Action
	Changes []string
}

// PlanImport compares a file with the store without writing anything. A host
// is matched by its fqdn or name; a reference that matches more than one
// host, or a name and an fqdn that match different hosts, is a problem.
func PlanImport(ctx context.Context, st *store.Store, f *File, opt Options) (*Plan, error) {
	p := &Plan{}
	var problems Problems

	views, err := st.Stacks(ctx)
	if err != nil {
		return nil, err
	}
	existing := map[string]model.Stack{}
	for _, v := range views {
		existing[v.Stack.Name] = v.Stack
	}
	planned := map[string]bool{}
	for _, s := range f.Stacks {
		planned[s.Name] = true
		want := model.Stack{Name: s.Name, Description: s.Description, Owner: s.Owner, Environment: s.Environment}
		cur, ok := existing[s.Name]
		if !ok {
			p.Stacks = append(p.Stacks, StackPlan{Stack: want, Action: Create})
			continue
		}
		sp := StackPlan{Stack: cur, Action: Unchanged}
		set := func(field string, dst *string, v string) {
			if v != "" && v != *dst {
				sp.Changes = append(sp.Changes, fmt.Sprintf("%s %q → %q", field, *dst, v))
				*dst = v
			}
		}
		set("description", &sp.Stack.Description, s.Description)
		set("owner", &sp.Stack.Owner, s.Owner)
		set("environment", &sp.Stack.Environment, s.Environment)
		if len(sp.Changes) > 0 {
			sp.Action = Update
		}
		p.Stacks = append(p.Stacks, sp)
	}

	claimed := map[string]string{}
	for _, h := range f.Hosts {
		hp, probs, err := planHost(ctx, st, f, h)
		if err != nil {
			return nil, err
		}
		if hp.ID != "" {
			if other, dup := claimed[hp.ID]; dup {
				probs = append(probs, Problem{Line: h.Line, Host: h.Name,
					Msg: fmt.Sprintf("matches the same recorded host as %s", other)})
			}
			claimed[hp.ID] = h.Name
		}
		problems = append(problems, probs...)
		for _, m := range h.Stacks {
			if _, ok := existing[m.Stack]; !ok && !planned[m.Stack] {
				planned[m.Stack] = true
				p.Stacks = append(p.Stacks, StackPlan{Stack: model.Stack{Name: m.Stack}, Action: Create})
			}
		}
		p.Hosts = append(p.Hosts, hp)
	}
	if len(problems) > 0 {
		return nil, problems
	}
	p.Warnings = append(p.Warnings, credentialWarnings(f, opt)...)
	for _, hp := range p.Hosts {
		for _, w := range hp.warnings {
			p.Warnings = append(p.Warnings, hp.Host.Name+": "+w)
		}
	}
	return p, nil
}

func planHost(ctx context.Context, st *store.Store, f *File, h Host) (HostPlan, Problems, error) {
	hp := HostPlan{Host: h, Action: Create}
	prob := func(format string, args ...any) Problems {
		return Problems{{Line: h.Line, Host: h.Name, Msg: fmt.Sprintf(format, args...)}}
	}
	lookup := func(ref string) (string, error) {
		id, err := st.LookupCI(ctx, ref)
		if errors.Is(err, store.ErrNoSuchHost) {
			return "", nil
		}
		return id, err
	}
	byName, err := lookup(h.Name)
	if err != nil {
		return hp, prob("%v", err), nil
	}
	byFQDN := ""
	if h.FQDN != "" {
		if byFQDN, err = lookup(h.FQDN); err != nil {
			return hp, prob("%v", err), nil
		}
	}
	if byName != "" && byFQDN != "" && byName != byFQDN {
		return hp, prob("the name matches host %s and the fqdn matches host %s; fix the file or the store", byName, byFQDN), nil
	}
	hp.ID = byFQDN
	if hp.ID == "" {
		hp.ID = byName
	}

	var (
		profiles []store.ConnProfile
		roles    = map[string]string{}
		ci       model.CI
	)
	if hp.ID != "" {
		hp.Action = Unchanged
		if ci, err = st.CI(ctx, hp.ID); err != nil {
			return hp, nil, err
		}
		if ci.Kind != model.KindHost {
			return hp, prob("%s is a %s, not a host", hp.ID, ci.Kind), nil
		}
		ids, err := st.Identities(ctx, hp.ID)
		if err != nil {
			return hp, nil, err
		}
		if h.FQDN != "" && !hasIdentity(ids, model.IdentFQDN, h.FQDN) {
			hp.addFQDN = true
			hp.Changes = append(hp.Changes, "add fqdn "+h.FQDN)
		}
		if h.Application != "" {
			facts, err := st.Facts(ctx, hp.ID)
			if err != nil {
				return hp, nil, err
			}
			cur := model.Fact{}
			for _, fc := range facts {
				if fc.Attr == model.AttrApplication && fc.Scope == "" {
					cur = fc
				}
			}
			switch {
			case cur.Raw == h.Application:
			case cur.SourceKind == model.SourceCollector:
				hp.warnings = append(hp.warnings, fmt.Sprintf("the host reports application %q, which wins over %q from the file", cur.Raw, h.Application))
			default:
				hp.setApp = h.Application
				if cur.Raw == "" {
					hp.Changes = append(hp.Changes, "application "+h.Application)
				} else {
					hp.Changes = append(hp.Changes, fmt.Sprintf("application %s → %s", cur.Raw, h.Application))
				}
			}
		}
		if h.Disabled != nil && *h.Disabled != (ci.DisabledAt != nil) {
			hp.disable = h.Disabled
			hp.Changes = append(hp.Changes, map[bool]string{true: "disable", false: "enable"}[*h.Disabled])
		}
		if profiles, err = st.ConnProfiles(ctx, hp.ID); err != nil {
			return hp, nil, err
		}
		ms, err := st.MembershipsOf(ctx, hp.ID)
		if err != nil {
			return hp, nil, err
		}
		for _, m := range ms {
			roles[m.Stack] = m.Role
		}
	} else {
		hp.setApp = h.Application
		if h.Disabled != nil && *h.Disabled {
			hp.disable = h.Disabled
		}
	}

	if t, ok := f.Effective(h); ok {
		tp := &TargetPlan{Action: Create, Profile: store.ConnProfile{Transport: t.Transport, Port: t.Port,
			Username: t.User, CredentialRef: t.Credential, BastionRef: t.Bastion, Enabled: !t.Disabled,
			Address: t.Address}}
		for _, cur := range profiles {
			if cur.Transport != t.Transport {
				continue
			}
			tp.Action = Unchanged
			tp.Profile.ID, tp.Profile.HostKeyPolicy = cur.ID, cur.HostKeyPolicy
			if tp.Profile.Address == "" {
				tp.Profile.Address = cur.Address
			}
			diff := func(field, was, now string) {
				if was != now {
					tp.Changes = append(tp.Changes, fmt.Sprintf("%s %s → %s", field, orNone(was), orNone(now)))
				}
			}
			diff("address", cur.Address, tp.Profile.Address)
			if effectivePort(cur.Transport, cur.Port) != effectivePort(t.Transport, t.Port) {
				diff("port", fmt.Sprint(cur.Port), fmt.Sprint(t.Port))
			} else {
				tp.Profile.Port = cur.Port
			}
			diff("user", cur.Username, tp.Profile.Username)
			diff("credential", cur.CredentialRef, tp.Profile.CredentialRef)
			diff("bastion", cur.BastionRef, tp.Profile.BastionRef)
			if cur.Enabled != tp.Profile.Enabled {
				tp.Changes = append(tp.Changes, map[bool]string{true: "enable", false: "disable"}[tp.Profile.Enabled])
			}
			if len(tp.Changes) > 0 {
				tp.Action = Update
			}
		}
		if tp.Profile.Address == "" {
			tp.Profile.Address = h.FQDN
		}
		if tp.Profile.Address == "" && hp.ID != "" {
			tp.Profile.Address = defaultAddress(ctx, st, hp.ID, ci.Label)
		}
		if tp.Profile.Address == "" {
			tp.Profile.Address = h.Name
		}
		hp.Target = tp
	} else if h.Target != nil && h.Target.None && len(profiles) > 0 {
		hp.TargetNote = "target: false; its existing connections are left as they are"
	}

	for _, m := range h.Stacks {
		mp := MembershipPlan{Membership: m, Action: Create}
		if role, ok := roles[m.Stack]; ok {
			mp.OldRole, mp.Action = role, Unchanged
			// A stack named without a role keeps the role already recorded:
			// an import never takes anything away.
			if m.Role == "" {
				mp.Role = role
			} else if role != m.Role {
				mp.Action = Update
			}
		}
		hp.Memberships = append(hp.Memberships, mp)
	}

	if hp.Action == Unchanged && hp.changes() {
		hp.Action = Update
	}
	return hp, nil, nil
}

// changes reports whether anything about the host would change.
func (hp HostPlan) changes() bool {
	if len(hp.Changes) > 0 || (hp.Target != nil && hp.Target.Action != Unchanged) {
		return true
	}
	for _, m := range hp.Memberships {
		if m.Action != Unchanged {
			return true
		}
	}
	return false
}

func hasIdentity(ids []model.Identity, scheme model.IdentityScheme, value string) bool {
	for _, i := range ids {
		if i.Scheme == scheme && strings.EqualFold(i.Value, value) {
			return true
		}
	}
	return false
}

// defaultAddress is the strongest name a recorded host is known by: its
// fqdn, then its hostname, then its label.
func defaultAddress(ctx context.Context, st *store.Store, id, label string) string {
	ids, _ := st.Identities(ctx, id)
	for _, scheme := range []model.IdentityScheme{model.IdentFQDN, model.IdentHostname} {
		for _, i := range ids {
			if i.Scheme == scheme && i.Value != "" {
				return i.Value
			}
		}
	}
	return label
}

func effectivePort(transport string, port int) int {
	if port == 0 && transport == "ssh" {
		return 22
	}
	// The engine always dials WinRM over HTTPS, where 0 means 5986.
	if port == 0 && transport == "winrm" {
		return 5986
	}
	return port
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// credentialWarnings names keyring credentials the file uses that are not in
// the OS credential store yet, with the command that stores each.
func credentialWarnings(f *File, opt Options) []string {
	if opt.CredentialMissing == nil {
		return nil
	}
	refs := map[string]bool{}
	for _, h := range f.Hosts {
		t, ok := f.Effective(h)
		if !ok {
			continue
		}
		refs[t.Credential] = true
		if t.Bastion != "" {
			if _, _, _, ref, err := collect.ParseBastion(t.Bastion); err == nil {
				refs[ref] = true
			}
		}
	}
	var out []string
	for ref := range refs {
		name, ok := strings.CutPrefix(ref, "keyring:")
		if ok && opt.CredentialMissing(ref) {
			out = append(out, fmt.Sprintf("credential %s is not in the OS credential store; store it with: kryostate secret set %s", ref, name))
		}
	}
	sort.Strings(out)
	return out
}

// Counts is what a plan does, in numbers.
type Counts struct {
	HostsCreated, HostsUpdated, HostsUnchanged int
	TargetsCreated, TargetsUpdated             int
	MembershipsAdded, RolesChanged             int
	StacksCreated, StacksUpdated               int
}

// Counts totals the plan.
func (p *Plan) Counts() Counts {
	var c Counts
	for _, h := range p.Hosts {
		switch h.Action {
		case Create:
			c.HostsCreated++
		case Update:
			c.HostsUpdated++
		default:
			c.HostsUnchanged++
		}
		if h.Target != nil {
			switch h.Target.Action {
			case Create:
				c.TargetsCreated++
			case Update:
				c.TargetsUpdated++
			}
		}
		for _, m := range h.Memberships {
			switch m.Action {
			case Create:
				c.MembershipsAdded++
			case Update:
				c.RolesChanged++
			}
		}
	}
	for _, s := range p.Stacks {
		switch s.Action {
		case Create:
			c.StacksCreated++
		case Update:
			c.StacksUpdated++
		}
	}
	return c
}

// Empty reports whether the plan changes nothing.
func (p *Plan) Empty() bool {
	c := p.Counts()
	return c.HostsCreated+c.HostsUpdated+c.StacksCreated+c.StacksUpdated == 0
}

// Summary is one line totalling the plan.
func (p *Plan) Summary() string {
	if p.Empty() {
		return fmt.Sprintf("nothing to change: the store already matches the file (%d hosts)", len(p.Hosts))
	}
	c := p.Counts()
	s := fmt.Sprintf("%d hosts created, %d updated, %d unchanged; %d targets created, %d updated; %d stack memberships added, %d roles changed; %d stacks created, %d updated",
		c.HostsCreated, c.HostsUpdated, c.HostsUnchanged, c.TargetsCreated, c.TargetsUpdated,
		c.MembershipsAdded, c.RolesChanged, c.StacksCreated, c.StacksUpdated)
	switch n := len(p.Warnings); n {
	case 0:
	case 1:
		s += "; 1 warning"
	default:
		s += fmt.Sprintf("; %d warnings", n)
	}
	return s
}

// Write prints the plan: each host that changes with what changes, then the
// stacks, the unchanged hosts on one line, and any warnings.
func (p *Plan) Write(w io.Writer) {
	var unchanged []string
	for _, h := range p.Hosts {
		if h.Action == Unchanged {
			unchanged = append(unchanged, h.Host.Name)
			continue
		}
		fmt.Fprintf(w, "%s host %s\n", h.Action, h.Host.Name)
		for _, c := range h.Changes {
			fmt.Fprintf(w, "  %s\n", c)
		}
		if h.Action == Create {
			if h.Host.FQDN != "" {
				fmt.Fprintf(w, "  fqdn %s\n", h.Host.FQDN)
			}
			if h.setApp != "" {
				fmt.Fprintf(w, "  application %s\n", h.setApp)
			}
			if h.disable != nil {
				fmt.Fprintf(w, "  disabled\n")
			}
		}
		if t := h.Target; t != nil {
			pr := t.Profile
			fmt.Fprintf(w, "  target %s: %s %s@%s%s using %s", pr.Transport, t.Action, pr.Username, pr.Address, portSuffix(pr.Port), pr.CredentialRef)
			if pr.BastionRef != "" {
				fmt.Fprintf(w, " via %s", pr.BastionRef)
			}
			if !pr.Enabled {
				fmt.Fprint(w, " (disabled)")
			}
			if len(t.Changes) > 0 {
				fmt.Fprintf(w, " (%s)", strings.Join(t.Changes, ", "))
			}
			fmt.Fprintln(w)
		}
		if h.TargetNote != "" {
			fmt.Fprintf(w, "  %s\n", h.TargetNote)
		}
		for _, m := range h.Memberships {
			switch m.Action {
			case Create:
				fmt.Fprintf(w, "  stack %s: add%s\n", m.Stack, asRole(m.Role))
			case Update:
				fmt.Fprintf(w, "  stack %s: role %s → %s\n", m.Stack, orNone(m.OldRole), orNone(m.Role))
			}
		}
	}
	for _, s := range p.Stacks {
		if s.Action == Unchanged {
			continue
		}
		fmt.Fprintf(w, "%s stack %s", s.Action, s.Stack.Name)
		if len(s.Changes) > 0 {
			fmt.Fprintf(w, " (%s)", strings.Join(s.Changes, ", "))
		}
		fmt.Fprintln(w)
	}
	if len(unchanged) > 0 {
		fmt.Fprintf(w, "unchanged: %s\n", strings.Join(unchanged, ", "))
	}
	for _, warn := range p.Warnings {
		fmt.Fprintf(w, "warning: %s\n", warn)
	}
}

func asRole(role string) string {
	if role == "" {
		return ""
	}
	return " as " + role
}

func portSuffix(port int) string {
	if port == 0 {
		return ""
	}
	return fmt.Sprintf(":%d", port)
}

// Apply writes a plan, auditing each change. It changes only what the plan
// says: hosts, connections and memberships not in the file are left alone.
func Apply(ctx context.Context, st *store.Store, p *Plan, opt Options) error {
	if p.Empty() {
		return nil
	}
	by := opt.By
	reason := "import " + opt.Source
	stackIDs := map[string]int64{}
	for _, s := range p.Stacks {
		var id int64
		var err error
		switch s.Action {
		case Create:
			if id, err = st.PutStack(ctx, s.Stack); err == nil {
				err = st.Audit(ctx, by, "create stack", s.Stack.Name, s.Stack.Description)
			}
		case Update:
			if id, err = st.PutStack(ctx, s.Stack); err == nil {
				err = st.Audit(ctx, by, "edit stack", s.Stack.Name, strings.Join(s.Changes, ", "))
			}
		default:
			id, err = st.StackID(ctx, s.Stack.Name)
		}
		if err != nil {
			return fmt.Errorf("stack %s: %w", s.Stack.Name, err)
		}
		stackIDs[s.Stack.Name] = id
	}
	for _, hp := range p.Hosts {
		if hp.Action == Unchanged {
			continue
		}
		if err := applyHost(ctx, st, hp, stackIDs, by, reason); err != nil {
			return fmt.Errorf("host %s: %w", hp.Host.Name, err)
		}
	}
	return st.Audit(ctx, by, "import", opt.Source, p.Summary())
}

func applyHost(ctx context.Context, st *store.Store, hp HostPlan, stackIDs map[string]int64, by, reason string) error {
	h := hp.Host
	id := hp.ID
	if hp.Action == Create {
		ev := []model.Identity{{Scheme: model.IdentHostname, Value: strings.ToLower(h.Name)}}
		if h.FQDN != "" {
			ev = append([]model.Identity{{Scheme: model.IdentFQDN, Value: h.FQDN}}, ev...)
		}
		var created bool
		var err error
		if id, created, err = st.ResolveCI(ctx, model.KindHost, h.Name, ev); err != nil {
			return err
		}
		if created {
			if err := st.Audit(ctx, by, "create host", id, h.Name); err != nil {
				return err
			}
		}
	}
	if hp.addFQDN {
		if err := st.AddIdentity(ctx, id, model.IdentFQDN, h.FQDN); err != nil {
			return err
		}
		if err := st.Audit(ctx, by, "add identity", id, "fqdn="+h.FQDN); err != nil {
			return err
		}
	}
	if hp.setApp != "" {
		if err := st.SetApplication(ctx, id, hp.setApp, by, reason); err != nil {
			return err
		}
	}
	if hp.disable != nil {
		var err error
		if *hp.disable {
			_, err = st.DisableHost(ctx, id, by, reason)
		} else {
			_, err = st.EnableHost(ctx, id, by, reason)
		}
		if err != nil {
			return err
		}
	}
	if t := hp.Target; t != nil && t.Action != Unchanged {
		pr := t.Profile
		pr.CIID = id
		// No host key is given, so PutConnProfile keeps any pinned key.
		pr.HostKey, pr.BastionHostKey = "", ""
		if err := st.PutConnProfile(ctx, pr); err != nil {
			return err
		}
		if err := st.Audit(ctx, by, "connection", id, fmt.Sprintf("%s %s@%s%s using %s", pr.Transport,
			pr.Username, pr.Address, portSuffix(pr.Port), pr.CredentialRef)); err != nil {
			return err
		}
	}
	for _, m := range hp.Memberships {
		if m.Action == Unchanged {
			continue
		}
		sid, ok := stackIDs[m.Stack]
		if !ok {
			var err error
			if sid, err = st.EnsureStack(ctx, m.Stack); err != nil {
				return err
			}
			stackIDs[m.Stack] = sid
		}
		if err := st.AddToStack(ctx, sid, id, m.Role); err != nil {
			return err
		}
		if err := st.Audit(ctx, by, "add to stack", id, m.Stack+asRole(m.Role)); err != nil {
			return err
		}
	}
	return nil
}
