package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/KalebHawkins/hearsay/internal/collect"
	"github.com/KalebHawkins/hearsay/internal/store"
)

// TryResult is what one collector spec produced against one host, for a
// person checking a spec before trusting it.
type TryResult struct {
	Reading collect.Reading
	// OS is the family the host reported. A spec whose os list does not
	// include it would never run against this host in a real collection.
	OS        string
	OSMatches bool
}

// TrySpec runs one spec against one host and records nothing: no assertion,
// no probe, no evidence, no pinned host key. It exists so that someone
// writing a collector can see exactly what the command printed and what the
// parse rule took from it, before a real run writes it into the history.
func (e *Engine) TrySpec(ctx context.Context, p store.ConnProfile, sp collect.Spec) (TryResult, error) {
	t, family, err := e.dial(ctx, p)
	if err != nil {
		return TryResult{}, fmt.Errorf("connect to %s: %w", p.Address, err)
	}
	defer t.Close()
	rs := collect.Run(ctx, t, []collect.Spec{sp})
	return TryResult{
		Reading:   rs[0],
		OS:        family,
		OSMatches: slices.ContainsFunc(sp.OS, func(o string) bool { return strings.EqualFold(o, family) }),
	}, nil
}
