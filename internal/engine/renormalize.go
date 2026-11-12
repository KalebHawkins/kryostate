package engine

import (
	"context"

	"github.com/KalebHawkins/kryostate/internal/store"
)

// Renormalize applies the current rule set to every stored raw value.
//
// This is why collectors are forbidden from normalising. A rule that turned
// out to be wrong is corrected here, over values already in hand, without
// asking anything of hosts that may be unreachable or gone.
func (e *Engine) Renormalize(ctx context.Context, attrLike string) (int, int, error) {
	if attrLike == "" {
		attrLike = "%"
	}
	raws, err := e.Store.RawAssertions(ctx, attrLike)
	if err != nil {
		return 0, 0, err
	}
	matched, unmatched := 0, 0
	for id, r := range raws {
		res, ok := e.Norm.Normalize(r.Attr, r.Value)
		if !ok {
			// The old projection goes, even though the raw value stays. A
			// rule that was withdrawn because it was wrong must not leave its
			// reading of a value behind to be charted as current.
			if err := e.Store.ClearNormalized(ctx, id); err != nil {
				return matched, unmatched, err
			}
			unmatched++
			continue
		}
		if err := e.Store.SetNormalized(ctx, id, store.Normalized{
			Value: res.Value, Family: res.Family, Major: res.Major,
			Minor: res.Minor, Patch: res.Patch, RuleID: res.RuleID,
			Version: e.Norm.Version,
		}); err != nil {
			return matched, unmatched, err
		}
		matched++
	}
	return matched, unmatched, nil
}
