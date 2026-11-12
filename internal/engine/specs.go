package engine

import (
	"context"
	"slices"

	"github.com/KalebHawkins/kryostate/internal/collect"
)

// AllSpecs is every spec that may run: the built-in ones, any from a spec
// file, and the catalog collectors switched on in the store. The catalog part
// is read on each call, so switching one on in the web interface takes effect
// at the next collection without a restart.
func (e *Engine) AllSpecs(ctx context.Context) (*collect.SpecSet, error) {
	set := &collect.SpecSet{}
	if e.Specs != nil {
		set.Version = e.Specs.Version
		set.Specs = append(set.Specs, e.Specs.Specs...)
	}
	if e.Store == nil {
		return set, nil
	}
	choices, err := e.Store.Choices(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range choices {
		specs, err := c.Specs()
		if err != nil {
			// A row that no longer renders, say because the catalog entry
			// was removed in a later build, must not stop every other
			// collector from running.
			e.logf("catalog collector "+c.Label(), err)
			continue
		}
		set.Specs = append(set.Specs, specs...)
	}
	return set, nil
}

// specsForHost narrows AllSpecs to what applies to one host: its operating
// system, and any stacks a spec is limited to.
func (e *Engine) specsForHost(ctx context.Context, ciID, osFamily string) ([]collect.Spec, error) {
	all, err := e.AllSpecs(ctx)
	if err != nil {
		return nil, err
	}
	var member map[string]bool
	var out []collect.Spec
	for _, sp := range all.For(osFamily) {
		if len(sp.Stacks) > 0 {
			if member == nil {
				member = map[string]bool{}
				ms, err := e.Store.MembershipsOf(ctx, ciID)
				if err != nil {
					return nil, err
				}
				for _, m := range ms {
					member[m.Stack] = true
				}
			}
			if !slices.ContainsFunc(sp.Stacks, func(s string) bool { return member[s] }) {
				continue
			}
		}
		out = append(out, sp)
	}
	return out, nil
}
