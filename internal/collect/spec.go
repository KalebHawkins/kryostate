package collect

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/KalebHawkins/hearsay/internal/model"
)

//go:embed collectors.json
var builtinSpecs []byte

// Spec is one command and the rule for reading one value out of it.
//
// Adding a spec is a privileged act. Nothing here is sandboxed by the tool:
// the command runs on the target with whatever the supplied credential can do,
// and a vendor binary invoked with a version flag can still take a licence
// lock or write a log. Safety comes from the account the command runs as, and
// from somebody reading the command before it is added.
type Spec struct {
	ID       string          `json:"id"`
	Attr     string          `json:"attr"`
	OS       []string        `json:"os"`
	Comment  string          `json:"comment,omitempty"`
	Command  string          `json:"command"`
	Parse    Parse           `json:"parse"`
	Type     model.ValueType `json:"type,omitempty"`
	Scope    string          `json:"scope,omitempty"`
	Optional bool            `json:"optional,omitempty"`
}

type specFile struct {
	Version int    `json:"version"`
	Specs   []Spec `json:"specs"`
}

// SpecSet is an ordered collection of specs with a version, recorded on every
// assertion so that output produced by a spec later found to be wrong can be
// located.
type SpecSet struct {
	Version int
	Specs   []Spec
}

// BuiltinSpecs returns the command set shipped with the binary.
func BuiltinSpecs() (*SpecSet, error) { return LoadSpecs(builtinSpecs) }

// LoadSpecs parses a spec file and checks it before anything runs.
func LoadSpecs(b []byte) (*SpecSet, error) {
	var f specFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse collectors: %w", err)
	}
	if f.Version < 1 {
		return nil, errors.New("collector file needs a version of 1 or more")
	}
	seen := map[string]bool{}
	for i, s := range f.Specs {
		switch {
		case s.ID == "":
			return nil, fmt.Errorf("collector %d has no id", i)
		case seen[s.ID]:
			return nil, fmt.Errorf("duplicate collector id %q", s.ID)
		case s.Attr == "":
			return nil, fmt.Errorf("collector %s has no attribute", s.ID)
		case s.Command == "":
			return nil, fmt.Errorf("collector %s has no command", s.ID)
		case len(s.OS) == 0:
			return nil, fmt.Errorf("collector %s does not say which systems it applies to", s.ID)
		}
		seen[s.ID] = true
		if _, err := Extract(s.Parse, ""); err != nil && !errors.Is(err, ErrNoMatch) {
			return nil, fmt.Errorf("collector %s: %w", s.ID, err)
		}
	}
	return &SpecSet{Version: f.Version, Specs: f.Specs}, nil
}

// Merge adds operator-supplied specs after the builtin ones.
func (s *SpecSet) Merge(extra []byte) error {
	o, err := LoadSpecs(extra)
	if err != nil {
		return err
	}
	for _, sp := range o.Specs {
		for _, existing := range s.Specs {
			if existing.ID == sp.ID {
				return fmt.Errorf("collector %q is already defined", sp.ID)
			}
		}
		s.Specs = append(s.Specs, sp)
	}
	s.Version += o.Version * 1000
	return nil
}

// For returns the specs applicable to an operating system family.
func (s *SpecSet) For(osFamily string) []Spec {
	osFamily = strings.ToLower(osFamily)
	var out []Spec
	for _, sp := range s.Specs {
		for _, o := range sp.OS {
			if strings.ToLower(o) == osFamily {
				out = append(out, sp)
				break
			}
		}
	}
	return out
}

// Reading is one spec's result against one host. A failure is recorded as a
// reading too: that a command could not be run is information about the host.
type Reading struct {
	SpecID string
	Attr   string
	Scope  string
	Value  string
	Type   model.ValueType
	// Output is what the command actually printed, before the parser took a
	// substring out of it. It is kept so that a parse rule that turns out to
	// be wrong can be corrected against stored text rather than by going back
	// to every host.
	Output  string
	Command string
	// Ran reports that the command executed and exited zero. Output from a
	// command that failed is not material a parse rule should be replayed
	// over: an error message that happens to match a pattern would become a
	// value live collection had already rejected.
	Ran        bool
	Err        error
	ObservedAt time.Time
}

// Run executes every applicable spec over a transport and returns one reading
// per spec.
//
// A failure on one spec does not abandon the rest: a host missing one vendor
// binary should still report its operating system. Failures are returned, not
// swallowed, because the caller must be able to tell "this host has no value
// for X" apart from "we never asked".
func Run(ctx context.Context, t Transport, specs []Spec) []Reading {
	out := make([]Reading, 0, len(specs))
	for _, sp := range specs {
		select {
		case <-ctx.Done():
			out = append(out, Reading{SpecID: sp.ID, Attr: sp.Attr, Err: ctx.Err(), ObservedAt: time.Now()})
			continue
		default:
		}
		r := Reading{SpecID: sp.ID, Attr: sp.Attr, Scope: sp.Scope, Type: sp.Type,
			Command: sp.Command, ObservedAt: time.Now()}
		res, err := t.Run(ctx, sp.Command)
		r.Output = res.Stdout
		switch {
		case err != nil:
			r.Err = err
		case res.ExitCode != 0:
			r.Err = fmt.Errorf("%s exited %d: %s", firstWord(sp.Command), res.ExitCode, truncate(res.Stderr, 200))
		default:
			r.Ran = true
			v, perr := Extract(sp.Parse, res.Stdout)
			if perr != nil {
				r.Err = fmt.Errorf("%s: %w", sp.ID, perr)
			} else {
				r.Value = v
			}
		}
		out = append(out, r)
	}
	return out
}

func firstWord(s string) string {
	if i := strings.IndexByte(s, ' '); i > 0 {
		return s[:i]
	}
	return s
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
