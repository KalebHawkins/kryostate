package engine

import (
	"context"
	"errors"
	"slices"
	"strconv"

	"github.com/KalebHawkins/kryostate/internal/collect"
)

// warnSkipped logs, once per switched-on collector and reason, that it is
// skipped and how to clear it.
func (e *Engine) warnSkipped(c collect.Choice, err error) {
	if e.Log == nil {
		return
	}
	key := strconv.FormatInt(c.ID, 10) + "\x00" + err.Error()
	if _, seen := e.warned.LoadOrStore(key, true); seen {
		return
	}
	if errors.Is(err, collect.ErrNotInCatalog) {
		e.Log.Warn("switched-on collector skipped: its catalog entry no longer exists in this build",
			"template", c.Template, "choice", c.ID,
			"remove", "Turn off on the Collectors page, or: kryostate collectors -disable "+strconv.FormatInt(c.ID, 10))
		return
	}
	e.Log.Warn("switched-on collector skipped", "collector", c.Label(), "choice", c.ID, "err", err)
}

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
			// collector from running. It is skipped, and said once.
			e.warnSkipped(c, err)
			continue
		}
		set.Specs = append(set.Specs, specs...)
	}
	return set, nil
}

// specsForHost narrows AllSpecs to what applies to one host: its operating
// system, and the hosts a spec targets. tg is the run's view of every host;
// when it is nil and some spec has a target, the hosts are loaded here.
func (e *Engine) specsForHost(ctx context.Context, ciID, osFamily string, tg *Targets) ([]collect.Spec, error) {
	all, err := e.AllSpecs(ctx)
	if err != nil {
		return nil, err
	}
	specs := all.For(osFamily)
	if tg == nil && anyTarget(specs) {
		if tg, err = e.LoadTargets(ctx); err != nil {
			return nil, err
		}
	}
	return slices.DeleteFunc(specs, func(sp collect.Spec) bool { return !tg.specApplies(sp, ciID) }), nil
}
