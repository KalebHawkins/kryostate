package engine

import (
	"context"
	"slices"
	"strings"

	"github.com/KalebHawkins/kryostate/internal/collect"
)

// Fit is whether a collector runs on one host and, either way, why: the
// decision a run makes before connecting, written down so a page can say it.
type Fit struct {
	Runs bool
	// Spec is the command that decided: the one that runs, or the last one
	// the host's system has but its target excludes. Zero for an external
	// program, and when no command is for the host's system.
	Spec collect.Spec
	// Target is the query that decided, the spec's where and stacks joined.
	// Blank means every host whose system has a command.
	Target string
	// NoSystem is set when no command is for the host's system.
	NoSystem bool
	// BadTarget is why the target does not parse, which keeps the
	// collector off every host.
	BadTarget string
}

// fit decides whether a collector has anything to do on a host, given the
// operating system the host last reported (empty when unknown) and the hosts
// each of its specs targets. A host whose system is unknown is given the
// benefit of the doubt; the engine checks again once it is connected.
func (c Collector) fit(family, ciID string, tg *Targets) Fit {
	if c.External != nil {
		return Fit{Runs: true}
	}
	f := Fit{NoSystem: true}
	for _, sp := range c.Specs {
		if family != "" && !slices.ContainsFunc(sp.OS, func(o string) bool { return strings.EqualFold(o, family) }) {
			continue
		}
		f.NoSystem = false
		f.Spec, f.Target = sp, SpecTarget(sp)
		if !tg.specApplies(sp, ciID) {
			if tg != nil {
				if err := tg.Err(f.Target); err != nil {
					f.BadTarget = err.Error()
				}
			}
			continue
		}
		f.Runs, f.BadTarget = true, ""
		return f
	}
	return f
}

// OnHost is one collector as it stands for one host.
type OnHost struct {
	Planned
	Fit
}

// ForHost decides, for every planned collector, whether it runs on one host
// and why, by the same rule a scheduled run uses. The host's system is the one
// it last reported, or Windows for a host reached over WinRM that has not
// reported one yet; hosts are loaded once, and only when some collector is
// limited to some hosts.
func (e *Engine) ForHost(ctx context.Context, ciID string, plan []Planned) (family string, out []OnHost, err error) {
	families, err := e.Store.HostFamilies(ctx)
	if err != nil {
		return "", nil, err
	}
	family = families[ciID]
	if family == "" {
		if profiles, err := e.Store.ConnProfiles(ctx, ciID); err == nil {
			for _, p := range profiles {
				if p.Transport == "winrm" {
					family = "windows"
				}
			}
		}
	}
	var tg *Targets
	for _, p := range plan {
		if anyTarget(p.Specs) {
			if tg, err = e.LoadTargets(ctx); err != nil {
				return family, nil, err
			}
			break
		}
	}
	out = make([]OnHost, 0, len(plan))
	for _, p := range plan {
		out = append(out, OnHost{Planned: p, Fit: p.fit(family, ciID, tg)})
	}
	return family, out, nil
}

// DefaultPlan is every collector on its default schedule, for when there is
// no scheduler to say otherwise.
func DefaultPlan(cs []Collector) []Planned {
	out := make([]Planned, 0, len(cs))
	for _, c := range cs {
		out = append(out, Planned{Collector: c, Schedule: c.Default})
	}
	return out
}

// CatalogTemplate is the catalog entry a catalog collector's key names.
func CatalogTemplate(key string) string { return catalogTemplate(key) }
