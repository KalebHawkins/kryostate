package engine

import (
	"fmt"
	"strings"

	"github.com/KalebHawkins/kryostate/internal/collect"
)

// OwnSpecs are the specs loaded from an operator's collectors.json: every
// loaded spec that is not one of the built-in ones.
func (e *Engine) OwnSpecs() []collect.Spec {
	if e == nil || e.Specs == nil {
		return nil
	}
	builtin, err := collect.BuiltinSpecs()
	if err != nil {
		return nil
	}
	isBuiltin := map[string]bool{}
	for _, s := range builtin.Specs {
		isBuiltin[s.ID] = true
	}
	var out []collect.Spec
	for _, s := range e.Specs.Specs {
		if !isBuiltin[s.ID] {
			out = append(out, s)
		}
	}
	return out
}

// External finds a configured external program by name.
func (e *Engine) External(name string) (collect.External, bool) {
	if e == nil {
		return collect.External{}, false
	}
	for _, ex := range e.Externals {
		if ex.Name == name {
			return ex, true
		}
	}
	return collect.External{}, false
}

// RedactEnv hides the values of an external program's environment, which may
// hold the credential it brings for itself.
func RedactEnv(ex collect.External) collect.External {
	if len(ex.Env) == 0 {
		return ex
	}
	env := make([]string, len(ex.Env))
	for i, kv := range ex.Env {
		name, _, _ := strings.Cut(kv, "=")
		env[i] = name + "=(hidden)"
	}
	ex.Env = env
	return ex
}

// Definition writes a collector out as it would be written in a file: the
// file's name and its contents. id is a catalog id, builtin.ATTR, spec.ID,
// external.NAME, or the id of any loaded spec. param is the value for a
// catalog template, blank for its example value.
func (e *Engine) Definition(id, param string) (file string, body []byte, err error) {
	switch {
	case strings.HasPrefix(id, collect.RefBuiltin):
		_, by, err := collect.BuiltinRefs()
		if err != nil {
			return "", nil, err
		}
		specs, ok := by[strings.TrimPrefix(id, collect.RefBuiltin)]
		if !ok {
			return "", nil, fmt.Errorf("no built-in collector records %s", strings.TrimPrefix(id, collect.RefBuiltin))
		}
		b, err := collect.SpecsJSON(specs)
		return "collectors.json", b, err

	case strings.HasPrefix(id, collect.RefExternal):
		ex, ok := e.External(strings.TrimPrefix(id, collect.RefExternal))
		if !ok {
			return "", nil, fmt.Errorf("no external program named %s", strings.TrimPrefix(id, collect.RefExternal))
		}
		b, err := ExternalJSON(RedactEnv(ex))
		return "externals.json", b, err
	}
	if t, ok := collect.Lookup(id); ok {
		specs, err := t.Definition(param)
		if err != nil {
			return "", nil, err
		}
		b, err := collect.SpecsJSON(specs)
		return "collectors.json", b, err
	}
	want := strings.TrimPrefix(id, collect.RefSpec)
	if e != nil && e.Specs != nil {
		for _, s := range e.Specs.Specs {
			if s.ID == want || s.ID == id {
				b, err := collect.SpecsJSON([]collect.Spec{s})
				return "collectors.json", b, err
			}
		}
	}
	return "", nil, fmt.Errorf("no collector named %q", id)
}
