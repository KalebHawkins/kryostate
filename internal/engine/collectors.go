package engine

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/KalebHawkins/kryostate/internal/collect"
)

// Collector kinds, as the schedules page and the command line name them.
const (
	KindBuiltin  = "builtin"
	KindCatalog  = "catalog"
	KindSpec     = "spec"
	KindExternal = "external"
)

// Collector is one thing that can be scheduled: a group of built-in commands,
// a catalog collector somebody switched on, one command from a collector
// file, or one external program.
//
// Its Key is what a schedule is stored against and what the command line
// takes:
//
//	builtin.os, builtin.hw, builtin.host   built-in commands, by fact group
//	catalog.cpu.cores                      a catalog entry that takes no value
//	catalog.file.exists.7                  a catalog template, by its number
//	spec.<id>                              a command from collectors.json
//	external.<name>                        a program from externals.json
type Collector struct {
	Key   string
	Kind  string
	Title string
	// Specs are the commands it runs. Empty for an external program.
	Specs []collect.Spec
	// External is set for an external program.
	External *collect.External
	// Default is the schedule it runs on when nobody chose one.
	Default Schedule
	// Reveals marks a catalog collector that reads content, such as a file.
	Reveals bool
}

// Schedule is an effective schedule: a stored one, or a default.
type Schedule struct {
	// Every is the interval. Zero means manual only.
	Every time.Duration
	// At is HH:MM local time, for whole-day intervals only.
	At string
}

// Manual reports whether the collector only runs when asked.
func (s Schedule) Manual() bool { return s.Every <= 0 }

// builtinGroups names the built-in fact groups. Anything else built in is
// grouped by its first attribute segment and titled with it.
var builtinGroups = map[string]string{
	"os":   "Operating system (built in)",
	"hw":   "Hardware (built in)",
	"host": "Host identity (built in)",
}

var builtinIDs = sync.OnceValue(func() map[string]string {
	out := map[string]string{}
	set, err := collect.BuiltinSpecs()
	if err != nil {
		return out
	}
	for _, sp := range set.Specs {
		out[sp.ID] = "builtin." + attrGroup(sp.Attr)
	}
	return out
})

func attrGroup(attr string) string {
	g, _, _ := strings.Cut(attr, ".")
	return g
}

// DefaultSchedule is the schedule a collector runs on when nobody chose one.
//
// The defaults follow how fast the thing being read changes. Hardware,
// operating system versions and application versions move when someone
// upgrades, which is rarely and on purpose, so once a day is often enough to
// notice. Settings and file checks drift more casually and are cheap, so they
// are read four times a day. Nothing defaults to more often than that: the
// cost of a schedule lands on hosts that belong to other people.
func DefaultSchedule(c Collector) Schedule {
	day := Schedule{Every: 24 * time.Hour}
	switch c.Kind {
	case KindCatalog:
		t, ok := collect.Lookup(catalogTemplate(c.Key))
		if !ok {
			return day
		}
		if t.Category == "Files & config" {
			return Schedule{Every: 6 * time.Hour}
		}
		return day
	default:
		return day
	}
}

// catalogTemplate takes the catalog entry id back out of a collector key.
func catalogTemplate(key string) string {
	rest := strings.TrimPrefix(key, "catalog.")
	if i := strings.LastIndex(rest, "."); i >= 0 {
		if _, err := strconv.ParseUint(rest[i+1:], 10, 64); err == nil {
			return rest[:i]
		}
	}
	return rest
}

// choiceKey is the collector key of a switched-on catalog entry: its spec id
// without the variant.
func choiceKey(c collect.Choice) string {
	id := c.SpecID("x")
	return strings.TrimSuffix(id, ".x")
}

// CollectorKey names the collector a spec belongs to. Every spec belongs to
// exactly one, so running a collector means running exactly its specs.
func CollectorKey(specID string) string {
	if k, ok := builtinIDs()[specID]; ok {
		return k
	}
	if strings.HasPrefix(specID, "catalog.") {
		if i := strings.LastIndex(specID, "."); i > len("catalog.") {
			return specID[:i]
		}
	}
	return "spec." + specID
}

// CollectorKeyFor names the collector behind a recorded value. External
// programs record under a source of their own rather than a spec.
func CollectorKeyFor(specID, sourceName string) string {
	if name, ok := strings.CutPrefix(sourceName, "external:"); ok {
		return "external." + name
	}
	if specID == "" {
		return ""
	}
	return CollectorKey(specID)
}

// Collectors lists every collector, built in first, then catalog, then the
// operator's own commands and programs. Catalog collectors are read from the
// store on each call, so one switched on a moment ago is included.
func (e *Engine) Collectors(ctx context.Context) ([]Collector, error) {
	all, err := e.AllSpecs(ctx)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*Collector{}
	var order []string
	add := func(key, kind, title string) *Collector {
		c, ok := byKey[key]
		if !ok {
			c = &Collector{Key: key, Kind: kind, Title: title}
			byKey[key] = c
			order = append(order, key)
		}
		return c
	}
	builtin := builtinIDs()
	for _, sp := range all.Specs {
		key := CollectorKey(sp.ID)
		switch {
		case builtin[sp.ID] != "":
			g := attrGroup(sp.Attr)
			title := builtinGroups[g]
			if title == "" {
				title = g + " (built in)"
			}
			c := add(key, KindBuiltin, title)
			c.Specs = append(c.Specs, sp)
		case strings.HasPrefix(sp.ID, "catalog."):
			c := add(key, KindCatalog, key)
			c.Specs = append(c.Specs, sp)
		default:
			c := add(key, KindSpec, sp.ID+" ("+sp.Attr+")")
			c.Specs = append(c.Specs, sp)
		}
	}
	// Catalog titles come from the choice, which says what a person chose
	// in their own words, rather than from a spec id.
	if e.Store != nil {
		if choices, err := e.Store.Choices(ctx); err == nil {
			for _, ch := range choices {
				if c, ok := byKey[choiceKey(ch)]; ok {
					c.Title = ch.Label()
					if t, ok := collect.Lookup(ch.Template); ok {
						c.Reveals = t.Reveals
					}
				}
			}
		}
	}
	for i := range e.Externals {
		ex := e.Externals[i]
		c := add("external."+ex.Name, KindExternal, ex.Name+" (external program)")
		c.External = &ex
	}
	rank := map[string]int{KindBuiltin: 0, KindCatalog: 1, KindSpec: 2, KindExternal: 3}
	out := make([]Collector, 0, len(order))
	for _, k := range order {
		c := byKey[k]
		c.Default = DefaultSchedule(*c)
		out = append(out, *c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Kind] != rank[out[j].Kind] {
			return rank[out[i].Kind] < rank[out[j].Kind]
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

// appliesTo reports whether a collector has anything to do on a host. See
// fit, which also says why.
func (c Collector) appliesTo(family, ciID string, tg *Targets) bool {
	return c.fit(family, ciID, tg).Runs
}

// ChoiceKey is the collector key a switched-on catalog collector runs under,
// as Plan and the schedules list it.
func ChoiceKey(c collect.Choice) string { return choiceKey(c) }
