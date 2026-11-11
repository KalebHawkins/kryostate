package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/KalebHawkins/hearsay/internal/model"
	"github.com/KalebHawkins/hearsay/internal/store"
)

// DetectSummary counts what a detection pass raised.
type DetectSummary struct {
	Conflicts    int
	Unparsed     int
	Stale        int
	Undocumented int
}

// Detect compares what the sources say and raises the disagreements.
//
// This is the output the tool exists for. An inventory that merely stores
// values tells you what it was told; comparing a document against a machine
// tells you which of the two is wrong, which is the question anyone actually
// has.
func (e *Engine) Detect(ctx context.Context, staleAfter time.Duration) (DetectSummary, error) {
	var sum DetectSummary
	pass := time.Now()

	hosts, err := e.Store.Hosts(ctx, store.HostFilter{})
	if err != nil {
		return sum, err
	}
	tracked, documented, err := e.Store.DocumentedAttrs(ctx)
	if err != nil {
		return sum, err
	}

	for _, h := range hosts {
		facts, err := e.Store.Facts(ctx, h.CI.ID)
		if err != nil {
			return sum, err
		}
		for _, f := range facts {
			if f.Contested {
				n, err := e.raiseConflict(ctx, h, f)
				if err != nil {
					return sum, err
				}
				sum.Conflicts += n
			}

			// A value that arrived and that no rule understood is a gap in the
			// rules, not a gap in the estate. It belongs in a queue rather
			// than silently becoming its own category on every chart.
			if f.Norm == "" && f.ValueType == model.TypeVersion && f.SourceKind == model.SourceCollector {
				if err := e.Store.RaiseDiscrepancy(ctx, model.Discrepancy{
					CIID: h.CI.ID, Attr: f.Attr, Scope: f.Scope, Kind: model.DiscUnparsed,
					Summary:    fmt.Sprintf("%s reported %s as %q and no rule recognised it", h.CI.Label, f.Attr, f.Raw),
					Observed:   f.Raw,
					ObservedBy: string(f.SourceKind),
				}); err != nil {
					return sum, err
				}
				sum.Unparsed++
			}

			// Something is running that nobody wrote down. Raised only for
			// attributes documents do cover elsewhere, because an attribute
			// no document has ever mentioned is not an omission, it is simply
			// not something this estate tracks on paper.
			if f.SourceKind == model.SourceCollector && tracked[f.Attr] &&
				!documented[h.CI.ID][f.Attr+"\x00"+f.Scope] {
				if err := e.Store.RaiseDiscrepancy(ctx, model.Discrepancy{
					CIID: h.CI.ID, Attr: f.Attr, Scope: f.Scope, Kind: model.DiscUndocumented,
					Summary: fmt.Sprintf("%s reports %s as %q and no document records it",
						h.CI.Label, f.Attr, f.Raw),
					Observed:   f.Raw,
					ObservedBy: f.SourceName,
				}); err != nil {
					return sum, err
				}
				sum.Undocumented++
			}

			if staleAfter > 0 && f.SourceKind == model.SourceCollector &&
				!f.ConfirmedAt.IsZero() && pass.Sub(f.ConfirmedAt) > staleAfter {
				if err := e.Store.RaiseDiscrepancy(ctx, model.Discrepancy{
					CIID: h.CI.ID, Attr: f.Attr, Scope: f.Scope, Kind: model.DiscStale,
					Summary: fmt.Sprintf("%s was last confirmed %s ago on %s",
						f.Attr, pass.Sub(f.ConfirmedAt).Round(time.Hour), h.CI.Label),
					Observed:   f.Raw,
					ObservedBy: f.SourceName,
				}); err != nil {
					return sum, err
				}
				sum.Stale++
			}
		}
	}

	_ = e.Store.ResolveStale(ctx, model.DiscValueConflict, pass)
	_ = e.Store.ResolveStale(ctx, model.DiscUnparsed, pass)
	_ = e.Store.ResolveStale(ctx, model.DiscUndocumented, pass)
	if staleAfter > 0 {
		// Only reconciled when staleness was actually evaluated. A pass with
		// the check turned off has not found these absent, it has not looked.
		_ = e.Store.ResolveStale(ctx, model.DiscStale, pass)
	}
	return sum, nil
}

func (e *Engine) raiseConflict(ctx context.Context, h store.HostRow, f model.Fact) (int, error) {
	all, err := e.Store.Conflicts(ctx, h.CI.ID, f.Attr, f.Scope)
	if err != nil {
		return 0, err
	}
	raised := 0
	for _, other := range all {
		if other.AssertionID == f.AssertionID || other.Raw == f.Raw {
			continue
		}
		if err := e.Store.RaiseDiscrepancy(ctx, model.Discrepancy{
			CIID: h.CI.ID, Attr: f.Attr, Scope: f.Scope, Kind: model.DiscValueConflict,
			Summary: fmt.Sprintf("%s: %s says %q, %s says %q",
				h.CI.Label, other.SourceKind, other.Raw, f.SourceKind, f.Raw),
			Claimed:    other.Raw,
			ClaimedBy:  string(other.SourceKind),
			Observed:   f.Raw,
			ObservedBy: string(f.SourceKind),
		}); err != nil {
			return raised, err
		}
		raised++
	}
	return raised, nil
}

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
