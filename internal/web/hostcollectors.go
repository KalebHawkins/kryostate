package web

import (
	"context"
	"strings"
	"time"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/engine"
	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// hostValueLimit caps the values shown beside one collector on a host page;
// a built-in group records many.
const hostValueLimit = 2

// hostCollector is one row of a host page's "Collectors on this host": what
// runs, why it runs here, when, and what it last recorded.
type hostCollector struct {
	Title string
	Href  string
	Kind  string
	// Why is the reason in words; Query, when set, is the target that
	// admitted or excluded the host, shown as code after Why.
	Why   string
	Query string
	// Schedule says when it runs, such as "daily at 02:00".
	Schedule string
	Values   []hostValue
	More     int
}

// hostValue is one value a collector recorded on the host.
type hostValue struct {
	Attr    string
	Scope   string
	Value   string
	Changed *store.Change
}

// hostCollectors is the whole section.
type hostCollectors struct {
	Running []hostCollector
	// Skipped are the collectors that are on but do not run on this host,
	// each with the reason.
	Skipped []hostCollector
	// Family is the system the decision was made for; blank when the host
	// has not reported one, in which case every command is given the
	// benefit of the doubt until it is connected.
	Family string
	Err    string
}

// hostCollectorsFor decides which collectors run on a host, with the engine's
// own rule, once, and names the values each recorded there.
func (s *Server) hostCollectorsFor(ctx context.Context, ciID string, facts []model.Fact, changed map[int64]*store.Change) hostCollectors {
	var out hostCollectors
	if s.Engine == nil {
		return out
	}
	var plan []engine.Planned
	var err error
	if s.Scheduler != nil {
		plan, err = s.Scheduler.Plan(ctx)
	} else {
		var cs []engine.Collector
		cs, err = s.Engine.Collectors(ctx)
		plan = engine.DefaultPlan(cs)
	}
	if err != nil {
		out.Err = err.Error()
		return out
	}
	family, on, err := s.Engine.ForHost(ctx, ciID, plan)
	if err != nil {
		out.Err = err.Error()
		return out
	}
	out.Family = family

	byAttr := map[string][]model.Fact{}
	bySource := map[string][]model.Fact{}
	for _, f := range facts {
		byAttr[f.Attr] = append(byAttr[f.Attr], f)
		bySource[f.SourceName] = append(bySource[f.SourceName], f)
	}

	for _, h := range on {
		row := hostCollector{Title: h.Title, Href: hostCollectorHref(h.Collector), Kind: h.Kind,
			Schedule: scheduleText(h.Key, h.Schedule)}
		if !h.Runs {
			row.Why, row.Query = skipReason(h, family)
			out.Skipped = append(out.Skipped, row)
			continue
		}
		row.Why, row.Query = runReason(h)
		var vals []model.Fact
		if h.External != nil {
			vals = bySource["external:"+h.External.Name]
		} else {
			seen := map[string]bool{}
			for _, sp := range h.Specs {
				if !seen[sp.Attr] {
					seen[sp.Attr] = true
					vals = append(vals, byAttr[sp.Attr]...)
				}
			}
		}
		for _, f := range vals {
			if len(row.Values) == hostValueLimit {
				row.More++
				continue
			}
			v := hostValue{Attr: f.Attr, Scope: f.Scope, Value: f.Norm, Changed: changed[f.AssertionID]}
			if v.Value == "" {
				v.Value = f.Raw
			}
			row.Values = append(row.Values, v)
		}
		out.Running = append(out.Running, row)
	}
	return out
}

// hostCollectorHref is the collectors page entry for a collector. A built-in
// group spans several entries, so it opens the list filtered to them.
func hostCollectorHref(c engine.Collector) string {
	switch c.Kind {
	case engine.KindBuiltin:
		return "/collectors?source=" + srcBuiltin
	case engine.KindCatalog:
		return collectorURL(engine.CatalogTemplate(c.Key))
	}
	// spec.<id> and external.<name> are the collectors page's own ids.
	return collectorURL(c.Key)
}

// runReason says why a collector runs on a host.
func runReason(h engine.OnHost) (why, query string) {
	switch h.Kind {
	case engine.KindBuiltin:
		return "Built in · every host", ""
	case engine.KindExternal:
		return "External program · every host, run on the KryoState server", ""
	case engine.KindSpec:
		return specReason(h.Spec), ""
	}
	if h.Target == "" {
		return "every host", ""
	}
	return "matches", h.Target
}

// specReason is where a spec from collectors.json says it runs, in its own
// fields: "collectors.json: os linux, stacks web".
func specReason(sp collect.Spec) string {
	var parts []string
	if len(sp.OS) > 0 {
		parts = append(parts, "os "+strings.Join(sp.OS, ", "))
	}
	if len(sp.Stacks) > 0 {
		parts = append(parts, "stacks "+strings.Join(sp.Stacks, ", "))
	}
	if sp.Where != "" {
		parts = append(parts, "where "+sp.Where)
	}
	if len(parts) == 0 {
		return "collectors.json: every host"
	}
	return "collectors.json: " + strings.Join(parts, "; ")
}

// skipReason says why a collector that is on does not run on a host.
func skipReason(h engine.OnHost, family string) (why, query string) {
	switch {
	case h.NoSystem:
		return "no command for " + strings.Join(systemNames([]string{family}), "") +
			" (has " + strings.Join(systemNames(specSystems(h.Specs)), ", ") + ")", ""
	case h.BadTarget != "":
		return "its target does not parse, so it runs nowhere: " + h.BadTarget, h.Target
	case h.Kind == engine.KindSpec:
		return "excluded by " + specReason(h.Spec), ""
	}
	return "target excludes this host:", h.Target
}

// scheduleText says when a collector runs, with the time picked for it when
// nobody chose one: "daily at 03:17", "every 6h (from 01:23)".
func scheduleText(key string, sc engine.Schedule) string {
	out := sc.Describe()
	t := engine.SpreadText(key, sc)
	switch {
	case t == "":
	case sc.Every%(24*time.Hour) == 0:
		out += " at " + t
	default:
		out += " (" + t + ")"
	}
	return out
}
