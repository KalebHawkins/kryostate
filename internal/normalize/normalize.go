// Package normalize turns the raw value a machine reported into a form that
// can be grouped and counted.
//
// Two constraints shape this package. Collectors never normalise: a parsing
// rule written by someone who has only seen Linux will be wrong about AIX, and
// a rule applied at collection time can only be corrected by reaching the host
// again. And the raw value is kept forever, so a wrong rule is undone by
// re-running this code over stored values rather than by another collection
// pass.
//
// Rules are data. Adding a product's version format should not require a
// rebuild.
package normalize

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

//go:embed rules.json
var builtinRules []byte

// Rule maps a raw value to a grouped form. Attr may be an exact attribute
// name, "*" for any, or a suffix pattern such as "*.version".
type Rule struct {
	ID      string `json:"id"`
	Attr    string `json:"attr"`
	Comment string `json:"comment,omitempty"`
	Pattern string `json:"pattern"`
	Family  string `json:"family,omitempty"`
	Major   string `json:"major,omitempty"`
	Minor   string `json:"minor,omitempty"`
	Patch   string `json:"patch,omitempty"`
	Value   string `json:"value"`

	re *regexp.Regexp
}

// Result is the grouped form of a raw value.
type Result struct {
	Value  string
	Family string
	Major  string
	Minor  string
	Patch  string
	RuleID string
}

// Normalizer applies an ordered rule set. Version is stored alongside every
// value it produces, so values written by a rule set that later turned out to
// be wrong can be found and recomputed.
type Normalizer struct {
	Version int
	rules   []Rule
}

type ruleFile struct {
	Version int    `json:"version"`
	Rules   []Rule `json:"rules"`
}

// Builtin returns the rule set shipped with the binary.
func Builtin() (*Normalizer, error) { return Load(builtinRules) }

// Load parses a rule set. Rules are compiled up front so a bad pattern fails
// at startup rather than halfway through a collection run.
func Load(b []byte) (*Normalizer, error) {
	var f ruleFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse rules: %w", err)
	}
	if f.Version < 1 {
		return nil, fmt.Errorf("rule set needs a version of 1 or more, got %d", f.Version)
	}
	n := &Normalizer{Version: f.Version, rules: f.Rules}
	seen := map[string]bool{}
	for i := range n.rules {
		r := &n.rules[i]
		if r.ID == "" {
			return nil, fmt.Errorf("rule %d has no id", i)
		}
		if seen[r.ID] {
			return nil, fmt.Errorf("duplicate rule id %q", r.ID)
		}
		seen[r.ID] = true
		// An empty pattern compiles happily and matches everything, so one
		// rule missing its pattern would silently claim every value in the
		// estate.
		if r.Pattern == "" {
			return nil, fmt.Errorf("rule %s has no pattern", r.ID)
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("rule %s: %w", r.ID, err)
		}
		r.re = re
	}
	return n, nil
}

// Merge appends operator-supplied rules ahead of the builtin set and bumps the
// version, so values produced by the combined set are distinguishable from
// those produced by the builtin set alone.
func (n *Normalizer) Merge(extra []byte) error {
	o, err := Load(extra)
	if err != nil {
		return err
	}
	n.rules = append(o.rules, n.rules...)
	n.Version += o.Version * 1000
	return nil
}

// Rules exposes the loaded rules for display.
func (n *Normalizer) Rules() []Rule { return n.rules }

// Normalize returns the grouped form of raw. The second return is false when
// no rule matched, which is a reportable condition rather than an error: a
// value nobody wrote a rule for is a gap in the rules, and it belongs in a
// queue a person can see.
func (n *Normalizer) Normalize(attr, raw string) (Result, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Result{}, false
	}
	for i := range n.rules {
		r := &n.rules[i]
		if !attrMatches(r.Attr, attr) {
			continue
		}
		m := r.re.FindStringSubmatchIndex(raw)
		if m == nil {
			continue
		}
		res := Result{
			Family: tidy(string(r.re.ExpandString(nil, r.Family, raw, m))),
			Major:  tidy(string(r.re.ExpandString(nil, r.Major, raw, m))),
			Minor:  tidy(string(r.re.ExpandString(nil, r.Minor, raw, m))),
			Patch:  tidy(string(r.re.ExpandString(nil, r.Patch, raw, m))),
			Value:  tidy(string(r.re.ExpandString(nil, r.Value, raw, m))),
			RuleID: r.ID,
		}
		if res.Value == "" {
			continue
		}
		return res, true
	}
	return Result{}, false
}

// tidy removes the debris an unmatched optional group leaves behind, so a
// Windows release without an R2 does not render as "Windows Server 2019 ".
func tidy(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "  ", " ")
	s = strings.TrimSuffix(s, ".")
	s = strings.TrimSuffix(s, " SP")
	s = strings.TrimSuffix(s, "-SP")
	return strings.TrimSpace(s)
}

// attrMatches supports exact names, "*", and a leading "*." suffix match so a
// rule can cover every attribute ending in ".version".
func attrMatches(pattern, attr string) bool {
	switch {
	case pattern == "" || pattern == "*":
		return true
	case strings.HasPrefix(pattern, "*."):
		return strings.HasSuffix(attr, pattern[1:]) || attr == pattern[2:]
	default:
		return pattern == attr
	}
}
