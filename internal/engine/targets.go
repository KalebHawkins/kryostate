package engine

import (
	"context"
	"sync"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/query"
)

// SpecTarget is the query choosing the hosts a spec runs on: its where, and
// its stacks written as a query, joined with &&. Blank means every host whose
// system the spec names.
func SpecTarget(sp collect.Spec) string {
	return query.And(query.In("stack", sp.Stacks), sp.Where)
}

// Targets decides which hosts each target matches. It is loaded once per run,
// from every host's current values in three statements, and each distinct
// target is compiled once, so the cost does not grow with the number of
// commands.
//
// A target that does not parse matches no host. Collecting from every host
// because a limit was mistyped would be the worse surprise; the collector's
// page shows the error.
type Targets struct {
	hosts   []query.Host
	byID    map[string]int
	resolve query.Resolver

	mu       sync.Mutex
	compiled map[string]compiled
}

type compiled struct {
	e   *query.Expr
	err error
}

// NewTargets evaluates targets against the given hosts. A nil resolver names
// parameterised attributes the default way.
func NewTargets(hosts []query.Host, resolve query.Resolver) *Targets {
	t := &Targets{hosts: hosts, byID: make(map[string]int, len(hosts)), resolve: resolve,
		compiled: map[string]compiled{}}
	for i, h := range hosts {
		t.byID[h.ID] = i
	}
	return t
}

// LoadTargets reads every live host for target evaluation.
func (e *Engine) LoadTargets(ctx context.Context) (*Targets, error) {
	hosts, err := e.Store.QueryHosts(ctx)
	if err != nil {
		return nil, err
	}
	return NewTargets(hosts, e.Store.QueryResolver(ctx)), nil
}

func (t *Targets) compile(where string) compiled {
	t.mu.Lock()
	defer t.mu.Unlock()
	c, ok := t.compiled[where]
	if !ok {
		c.e, c.err = query.Compile(where, t.resolve)
		t.compiled[where] = c
	}
	return c
}

// Err is why a target does not parse, or nil.
func (t *Targets) Err(where string) error {
	if where == "" {
		return nil
	}
	return t.compile(where).err
}

// Match reports whether a target selects a host. A blank target selects every
// host; one that does not parse selects none, and so does any target for a
// host that was not loaded.
func (t *Targets) Match(where, ciID string) bool {
	if where == "" {
		return true
	}
	c := t.compile(where)
	if c.err != nil {
		return false
	}
	i, ok := t.byID[ciID]
	return ok && c.e.Match(t.hosts[i])
}

// Hosts lists the hosts a target selects, in name order.
func (t *Targets) Hosts(where string) []query.Host {
	if where == "" {
		return t.hosts
	}
	c := t.compile(where)
	if c.err != nil {
		return nil
	}
	var out []query.Host
	for _, h := range t.hosts {
		if c.e.Match(h) {
			out = append(out, h)
		}
	}
	return out
}

// All is every host loaded, in name order.
func (t *Targets) All() []query.Host { return t.hosts }

// specApplies reports whether a spec targets a host. A nil Targets means
// nothing was loaded because no spec has a target.
func (t *Targets) specApplies(sp collect.Spec, ciID string) bool {
	where := SpecTarget(sp)
	if where == "" {
		return true
	}
	return t != nil && t.Match(where, ciID)
}

// targetsFor loads the hosts once for a run, or returns nil when no
// collector is limited to some hosts and so nothing needs loading.
func (e *Engine) targetsFor(ctx context.Context) (*Targets, error) {
	all, err := e.AllSpecs(ctx)
	if err != nil {
		return nil, err
	}
	if !anyTarget(all.Specs) {
		return nil, nil
	}
	return e.LoadTargets(ctx)
}

// anyTarget reports whether any spec is limited to some hosts, so that hosts
// need loading at all.
func anyTarget(specs []collect.Spec) bool {
	for _, sp := range specs {
		if SpecTarget(sp) != "" {
			return true
		}
	}
	return false
}
