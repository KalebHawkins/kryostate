package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/query"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// Systems an added collector may name in its os field: the ones KryoState
// connects to.
var addSystems = []string{"linux", "aix", "windows"}

// reservedPrefixes start the ids of other kinds of collector, so an added
// collector may not start with one.
var reservedPrefixes = []string{"catalog.", collect.RefBuiltin, collect.RefSpec, collect.RefExternal}

// AddedCollectors are the collectors added with "kryostate collectors add"
// that run. They are read from the store on each call, so one added while the
// server runs is collected, scheduled and shown without a restart.
//
// An added collector whose id a built-in collector or the collectors file
// also has is skipped, and said once: the file wins, because it is what the
// server was started with. "collectors add" refuses such an id, so this only
// happens when the file is edited afterwards.
func (e *Engine) AddedCollectors(ctx context.Context) ([]store.UserCollector, error) {
	if e == nil || e.Store == nil {
		return nil, nil
	}
	all, err := e.Store.UserCollectors(ctx)
	if err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	if e.Specs != nil {
		for _, sp := range e.Specs.Specs {
			taken[sp.ID] = true
		}
	}
	out := all[:0]
	for _, u := range all {
		if taken[u.Spec.ID] {
			e.warnShadowed(u.Spec.ID)
			continue
		}
		out = append(out, u)
	}
	return out, nil
}

func (e *Engine) warnShadowed(id string) {
	if e.Log == nil {
		return
	}
	if _, seen := e.warned.LoadOrStore("added\x00"+id, true); seen {
		return
	}
	e.Log.Warn("added collector skipped: the collectors file defines the same id, and the file wins",
		"collector", id, "remove", "kryostate collectors rm "+id)
}

// LoadedSpecs is every spec that is not a catalog choice: the built-in ones,
// those from the collectors file, and the added ones.
func (e *Engine) LoadedSpecs(ctx context.Context) ([]collect.Spec, error) {
	var out []collect.Spec
	if e == nil {
		return nil, nil
	}
	if e.Specs != nil {
		out = append(out, e.Specs.Specs...)
	}
	added, err := e.AddedCollectors(ctx)
	if err != nil {
		return nil, err
	}
	for _, u := range added {
		out = append(out, u.Spec)
	}
	return out, nil
}

// Added finds a running added collector by its spec id.
func (e *Engine) Added(ctx context.Context, id string) (store.UserCollector, bool) {
	added, err := e.AddedCollectors(ctx)
	if err != nil {
		return store.UserCollector{}, false
	}
	for _, u := range added {
		if u.Spec.ID == id {
			return u, true
		}
	}
	return store.UserCollector{}, false
}

// CheckAdd checks specs about to be added, as a whole, before any is written:
// each must load as a collectors file entry would, and its id must not be one
// a built-in collector, the catalog or the collectors file already uses. An
// id already added is fine; adding it again replaces it.
func (e *Engine) CheckAdd(ctx context.Context, specs []collect.Spec) error {
	builtin := map[string]bool{}
	if set, err := collect.BuiltinSpecs(); err == nil {
		for _, sp := range set.Specs {
			builtin[sp.ID] = true
		}
	}
	file := map[string]bool{}
	for _, sp := range e.OwnSpecs() {
		file[sp.ID] = true
	}
	for _, sp := range specs {
		if err := collect.CheckSpec(sp); err != nil {
			return err
		}
		for _, p := range reservedPrefixes {
			if strings.HasPrefix(sp.ID, p) {
				return fmt.Errorf("collector %s: ids starting with %q are reserved; choose another id", sp.ID, p)
			}
		}
		switch {
		case builtin[sp.ID]:
			return fmt.Errorf("collector %s: that id is a built-in collector's; choose another id", sp.ID)
		case file[sp.ID]:
			return fmt.Errorf("collector %s: that id is already defined in the collectors file; choose another id, or remove it from the file", sp.ID)
		}
		if _, ok := collect.Lookup(sp.ID); ok {
			return fmt.Errorf("collector %s: that id is a catalog entry's; switch it on with: kryostate collectors enable %s, or choose another id", sp.ID, sp.ID)
		}
		if err := collect.CheckAttr(sp.Attr); err != nil {
			return fmt.Errorf("collector %s: %w", sp.ID, err)
		}
		for _, o := range sp.OS {
			if !containsFold(addSystems, o) {
				return fmt.Errorf("collector %s: os %q is not a system KryoState collects from; use %s", sp.ID, o, strings.Join(addSystems, ", "))
			}
		}
		if err := checkType(sp); err != nil {
			return err
		}
		if w := strings.TrimSpace(sp.Where); w != "" {
			if _, err := query.Parse(w); err != nil {
				var qe *query.Error
				if errors.As(err, &qe) {
					return fmt.Errorf("collector %s: where: %s\n  %s\n  %s", sp.ID, qe.Msg, w, qe.Caret())
				}
				return fmt.Errorf("collector %s: where: %w", sp.ID, err)
			}
		}
		if e.Store != nil {
			for _, name := range sp.Stacks {
				if _, err := e.Store.StackID(ctx, name); err != nil {
					return fmt.Errorf("collector %s: stacks: %w", sp.ID, err)
				}
			}
		}
	}
	return nil
}

func checkType(sp collect.Spec) error {
	switch sp.Type {
	case "", model.TypeText, model.TypeVersion, model.TypePath, model.TypeNumber, model.TypeBool, model.TypeList:
		return nil
	}
	return fmt.Errorf("collector %s: type %q is not one of text, version, path, number, bool, list", sp.ID, sp.Type)
}

func containsFold(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}
