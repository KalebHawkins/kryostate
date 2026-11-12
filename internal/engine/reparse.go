package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// ReparseSummary counts what re-reading the stored output produced.
type ReparseSummary struct {
	// Stored counts the outputs the filter matched, before any were read.
	Stored int
	// Considered counts those belonging to a collector this build knows.
	Considered int
	// Changed counts values that came out different from the ones this same
	// output produced before.
	Changed int
	// Recovered counts outputs that previously yielded nothing and now do.
	Recovered int
	// Failed counts outputs the current rule still cannot read.
	Failed int
	// Skipped counts outputs held back from replay, such as truncated ones.
	Skipped int
}

// Reparse re-runs the current parse rules over stored command output.
//
// Renormalize fixes a rule that read a value wrongly. Reparse fixes the layer
// below it: a rule that took the wrong substring out of the command's output
// in the first place. Both exist so that a parsing mistake is repaired against
// the database rather than by connecting to several hundred hosts to ask them
// the same question again.
//
// Nothing is contacted. A host that has since been decommissioned still gets
// its value corrected, which is the case that makes this worth having.
//
// Each stored output replaces only the claim it produced itself, found through
// the link made when it was collected. Comparing against whichever claim
// currently wins would let a repair withdraw a document's value, or another
// collector's, or skip the repair entirely because some other source happens
// to already say the right thing.
func (e *Engine) Reparse(ctx context.Context, f store.EvidenceFilter) (ReparseSummary, error) {
	var sum ReparseSummary
	ev, err := e.Store.AllEvidence(ctx, f)
	if err != nil {
		return sum, err
	}
	sum.Stored = len(ev)
	if len(ev) == 0 {
		return sum, nil
	}

	all, err := e.AllSpecs(ctx)
	if err != nil {
		return sum, err
	}
	specs := map[string]collect.Spec{}
	for _, sp := range all.Specs {
		specs[sp.ID] = sp
	}
	srcID, err := e.Store.SourceID(ctx, model.SourceCollector, "builtin")
	if err != nil {
		return sum, err
	}
	runID := fmt.Sprintf("reparse-%d", time.Now().UnixNano())

	for _, item := range ev {
		sp, ok := specs[item.SpecID]
		if !ok {
			// A spec that no longer exists is not a failure to report: the
			// operator removed it, and its stored output is simply no longer
			// being read.
			continue
		}
		sum.Considered++

		// A value extracted from output that was cut short may be a prefix of
		// the real one, and replacing a correct claim with a truncated one
		// would be this command doing damage rather than repair.
		if item.Truncated {
			sum.Skipped++
			continue
		}

		value, perr := collect.Extract(sp.Parse, item.Output)
		if perr != nil || value == "" {
			sum.Failed++
			continue
		}

		prev, err := e.priorDerivation(ctx, item)
		if err != nil {
			return sum, err
		}
		switch {
		case prev == nil:
			sum.Recovered++
		case prev.Raw == value && prev.Retracted == nil:
			continue
		default:
			sum.Changed++
		}

		// The earlier reading is withdrawn rather than outranked. It was this
		// tool's misreading of output the host sent once, not a second thing
		// the host said, and leaving it in place would leave the collector
		// contradicting itself over a single observation.
		//
		// An assertion two specs both produced is left alone. Identical
		// values from the same source are stored once, so withdrawing it on
		// one spec's behalf would silently withdraw the other's claim as
		// well; the repair adds its corrected value and lets precedence
		// settle the rest.
		if prev != nil && prev.Retracted == nil {
			resting, err := e.Store.EvidenceRestingOn(ctx, prev.ID)
			if err != nil {
				return sum, err
			}
			if resting <= 1 {
				if err := e.Store.RetractAssertion(ctx, prev.ID, time.Now()); err != nil {
					return sum, err
				}
			}
		}

		r := collect.Reading{
			SpecID: item.SpecID, Attr: item.Attr, Scope: item.Scope,
			Value: value, Type: sp.Type, ObservedAt: item.At,
		}
		id, err := e.assertID(ctx, item.CIID, r, srcID, runID)
		if err != nil {
			return sum, err
		}
		if _, err := e.Store.LinkEvidenceIf(ctx, item.CIID, item.SpecID, item.Scope, id, item.Output); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

// priorDerivation returns the claim this stored output produced last time, or
// nil if it has never produced one. An assertion that has since been deleted
// is treated the same way.
func (e *Engine) priorDerivation(ctx context.Context, item store.Evidence) (*model.Assertion, error) {
	if item.AssertionID == 0 {
		return nil, nil
	}
	a, err := e.Store.Assertion(ctx, item.AssertionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}
