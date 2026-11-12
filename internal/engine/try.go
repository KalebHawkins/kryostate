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
}

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
	if len(specs) == 0 {
		return TryResult{}, errors.New("nothing to try")
	}
	t, family, err := e.dial(ctx, p)
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
	res.Reading = collect.Run(ctx, t, []collect.Spec{res.Spec})[0]
	return res, nil
}
