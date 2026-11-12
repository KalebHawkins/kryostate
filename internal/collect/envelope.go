package collect

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
)

// EnvelopeSchema is the contract an external collector writes to standard
// output. It is versioned in the document itself so that a collector written
// against an older shape fails loudly instead of being half-understood.
const EnvelopeSchema = "kryostate.facts/v1"

// Envelope is what an external collector prints.
type Envelope struct {
	Schema    string     `json:"schema"`
	Collector string     `json:"collector"`
	Target    string     `json:"target,omitempty"`
	Status    string     `json:"status"`
	Error     string     `json:"error,omitempty"`
	Facts     []FactItem `json:"facts"`
	// Complete says whether Facts is the whole of some category, which is the
	// only basis on which previously seen items may be retired. A collector
	// that merely failed to list something has not proved it is gone.
	Complete bool `json:"complete,omitempty"`
}

// FactItem is one reported value.
type FactItem struct {
	Key        string          `json:"key"`
	Value      string          `json:"value"`
	Type       model.ValueType `json:"type,omitempty"`
	Scope      string          `json:"scope,omitempty"`
	ObservedAt *time.Time      `json:"observed_at,omitempty"`
}

const (
	maxFacts     = 500
	maxKeyLen    = 120
	maxValueLen  = 4096
	maxScopeLen  = 120
	statusOK     = "ok"
	statusFailed = "failed"
)

// Keys are namespaced so that two collectors can each report "version"
// without one silently overwriting the other.
var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\.[a-z0-9_-]+)+$`)

// Validate checks an envelope before any of it is written.
//
// Validation is all-or-nothing on purpose. A collector that returns ten good
// facts and one malformed one has a bug, and accepting the ten means the bug
// is invisible until somebody trusts the eleventh. A rejected envelope leaves
// the previous values in place, marked stale, which is a state an operator can
// understand.
func (e *Envelope) Validate() error {
	if e.Schema != EnvelopeSchema {
		return fmt.Errorf("unexpected schema %q, this build reads %s", e.Schema, EnvelopeSchema)
	}
	if e.Collector == "" {
		return errors.New("envelope does not name its collector")
	}
	switch e.Status {
	case statusOK, statusFailed:
	default:
		return fmt.Errorf("status must be %q or %q, got %q", statusOK, statusFailed, e.Status)
	}
	if e.Status == statusFailed {
		return nil
	}
	if len(e.Facts) > maxFacts {
		return fmt.Errorf("envelope carries %d facts, the limit is %d", len(e.Facts), maxFacts)
	}
	seen := map[string]bool{}
	for i, f := range e.Facts {
		switch {
		case len(f.Key) > maxKeyLen:
			return fmt.Errorf("fact %d: key is longer than %d characters", i, maxKeyLen)
		case !keyPattern.MatchString(f.Key):
			return fmt.Errorf("fact %d: key %q must be namespaced, as in product.version", i, f.Key)
		case len(f.Value) > maxValueLen:
			return fmt.Errorf("fact %d (%s): value is longer than %d characters", i, f.Key, maxValueLen)
		case len(f.Scope) > maxScopeLen:
			return fmt.Errorf("fact %d (%s): scope is longer than %d characters", i, f.Key, maxScopeLen)
		case strings.ContainsAny(f.Key, " \t\n"):
			return fmt.Errorf("fact %d: key contains whitespace", i)
		}
		id := f.Key + "\x00" + f.Scope
		if seen[id] {
			return fmt.Errorf("fact %d: %q is reported twice for the same scope", i, f.Key)
		}
		seen[id] = true
		if f.Type != "" && !knownType(f.Type) {
			return fmt.Errorf("fact %d (%s): unknown type %q", i, f.Key, f.Type)
		}
	}
	return nil
}

func knownType(t model.ValueType) bool {
	switch t {
	case model.TypeText, model.TypeVersion, model.TypePath,
		model.TypeNumber, model.TypeFlag, model.TypeList:
		return true
	}
	return false
}
