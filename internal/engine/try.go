package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// TryResult is what one collector produced against one host, for a person
// checking it before trusting it.
type TryResult struct {
	// Spec is the one that ran: of several variants, the one for the
	// host's operating system.
	Spec    collect.Spec
	Reading collect.Reading
	// OS is the family the host reported. A spec whose os list does not
	// include it would never run against this host in a real collection.
	OS        string
	OSMatches bool
	// Skipped is set when nothing was run because no spec names the host's
	// system. Only TryAsCollected skips.
	Skipped bool
}

// ErrNotTargeted is returned by TryAsCollected for a host outside the
// collector's target.
var ErrNotTargeted = errors.New("this collector does not run on that host: the host is outside its target")

// TrySpec runs one spec against one host. See TrySpecs.
func (e *Engine) TrySpec(ctx context.Context, p store.ConnProfile, sp collect.Spec) (TryResult, error) {
	return e.TrySpecs(ctx, p, []collect.Spec{sp})
}

// TrySpecs connects to a host, picks the spec written for its operating
// system, runs it, and records nothing: no assertion, no probe, no evidence,
// no pinned host key. It exists so that someone choosing or writing a
// collector can see exactly what the command printed and what was taken from
// it, before a real run writes it into the history.
//
// When no spec names the host's system the first is run anyway, because the
// person asked, and the result says it would be skipped for real.
func (e *Engine) TrySpecs(ctx context.Context, p store.ConnProfile, specs []collect.Spec) (TryResult, error) {
	return e.try(ctx, p, specs, false)
}

// TryAsCollected is TrySpecs bound by the rules of a real collection: a host
// outside every spec's target is refused before anything connects, and when
// no spec names the host's system nothing is run and the result is Skipped.
// It is for callers less trusted than the operator's own command line.
func (e *Engine) TryAsCollected(ctx context.Context, p store.ConnProfile, specs []collect.Spec) (TryResult, error) {
	return e.try(ctx, p, specs, true)
}

func (e *Engine) try(ctx context.Context, p store.ConnProfile, specs []collect.Spec, asCollected bool) (TryResult, error) {
	if len(specs) == 0 {
		return TryResult{}, errors.New("nothing to try")
	}
	var tg *Targets
	if asCollected {
		if anyTarget(specs) {
			var err error
			if tg, err = e.LoadTargets(ctx); err != nil {
				return TryResult{}, err
			}
		}
		if !slices.ContainsFunc(specs, func(sp collect.Spec) bool { return tg.specApplies(sp, p.CIID) }) {
			return TryResult{}, ErrNotTargeted
		}
	}
	t, family, err := e.connectGuarded(ctx, "", p)
	if err != nil {
		return TryResult{}, fmt.Errorf("connect to %s: %w", p.Address, err)
	}
	defer t.Close()
	res := TryResult{Spec: specs[0], OS: family}
	for _, sp := range specs {
		if slices.ContainsFunc(sp.OS, func(o string) bool { return strings.EqualFold(o, family) }) {
			res.Spec, res.OSMatches = sp, true
			break
		}
	}
	if asCollected {
		if !res.OSMatches {
			res.Skipped = true
			return res, nil
		}
		if !tg.specApplies(res.Spec, p.CIID) {
			return TryResult{}, ErrNotTargeted
		}
	}
	res.Reading = collect.Run(ctx, t, []collect.Spec{res.Spec})[0]
	return res, nil
}
