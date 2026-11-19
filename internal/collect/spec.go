package collect

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
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
	// Stacks limits the spec to members of these stacks. Empty means every
	// host whose system is listed in OS.
	Stacks []string `json:"stacks,omitempty"`
	// Where limits the spec to the hosts a query in the inventory's
	// language matches, such as name in ("web01", "web02"). With Stacks as
	// well, a host must satisfy both. A query that does not parse matches
	// no host, and the collector's page says why.
	Where string `json:"where,omitempty"`
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
	dec := json.NewDecoder(bytes.NewReader(b))
	// A misspelled key, such as "optinal", would otherwise be ignored and the
	// spec would quietly behave differently from what its author wrote.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse collectors: %w", withLine(b, err))
	}
	if f.Version < 1 {
		return nil, errors.New("collector file needs a version of 1 or more")
	}
	seen := map[string]bool{}
	for i, s := range f.Specs {
		if s.ID == "" {
			return nil, fmt.Errorf("collector %d has no id", i)
		}
		if seen[s.ID] {
			return nil, fmt.Errorf("duplicate collector id %q", s.ID)
		}
		seen[s.ID] = true
		if err := CheckSpec(s); err != nil {
			return nil, err
		}
	}
	return &SpecSet{Version: f.Version, Specs: f.Specs}, nil
}

// CheckSpec checks one spec the way LoadSpecs does: it has an id, an
// attribute, a command, the systems it applies to, and a parse rule that
// compiles.
func CheckSpec(s Spec) error {
	switch {
	case s.ID == "":
		return errors.New("collector has no id")
	case s.Attr == "":
		return fmt.Errorf("collector %s has no attribute", s.ID)
	case s.Command == "":
		return fmt.Errorf("collector %s has no command", s.ID)
	case len(s.OS) == 0:
		return fmt.Errorf("collector %s does not say which systems it applies to", s.ID)
	}
	if _, err := Extract(s.Parse, ""); err != nil && !errors.Is(err, ErrNoMatch) {
		return fmt.Errorf("collector %s: %w", s.ID, err)
	}
	return nil
}

// ParseSpecList reads the specs in a file given to "collectors add": one spec
// object, a list of them, or a whole collectors file with a version and
// specs. Unknown fields are refused, as in LoadSpecs, and every spec is
// checked before any is returned.
func ParseSpecList(b []byte) ([]Spec, error) {
	trimmed := bytes.TrimSpace(b)
	var specs []Spec
	switch {
	case len(trimmed) == 0:
		return nil, errors.New("the file is empty")
	case trimmed[0] == '[':
		if err := decodeStrict(b, &specs); err != nil {
			return nil, err
		}
	default:
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(b, &probe); err != nil {
			return nil, fmt.Errorf("parse collectors: %w", withLine(b, err))
		}
		if _, ok := probe["specs"]; ok {
			set, err := LoadSpecs(b)
			if err != nil {
				return nil, err
			}
			return set.Specs, nil
		}
		var one Spec
		if err := decodeStrict(b, &one); err != nil {
			return nil, err
		}
		specs = []Spec{one}
	}
	if len(specs) == 0 {
		return nil, errors.New("the file has no collectors in it")
	}
	seen := map[string]bool{}
	for i, s := range specs {
		if s.ID == "" {
			return nil, fmt.Errorf("collector %d has no id", i+1)
		}
		if seen[s.ID] {
			return nil, fmt.Errorf("duplicate collector id %q", s.ID)
		}
		seen[s.ID] = true
		if err := CheckSpec(s); err != nil {
			return nil, err
		}
	}
	return specs, nil
}

func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("parse collectors: %w", withLine(b, err))
	}
	if dec.More() {
		return errors.New("parse collectors: more than one JSON value; put several collectors in a list: [ {...}, {...} ]")
	}
	return nil
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
	Ran bool
	// Completed reports that the host ran the command to the end, whatever
	// it exited with. A reading that completed and still failed is the host
	// saying it cannot tell; one that did not complete (the session broke,
	// the run was cancelled) says nothing about the host at all.
	Completed bool
	// Stderr is what the command wrote to standard error, for explaining a
	// failure.
	Stderr     string
	Err        error
	ObservedAt time.Time
}

// DefaultParallel is how many of a host's commands run at once over its one
// connection. OpenSSH allows 10 sessions on a connection by default; four
// leaves room for a person logged in through a multiplexed connection, and a
// host that allows fewer drops to one at a time (see Run).
const DefaultParallel = 4

// refusedTries is how many times a command is sent again, one at a time,
// after the host refused a session for it.
const refusedTries = 3

// Run executes every applicable spec over a transport, DefaultParallel at a
// time, and returns one reading per spec in the order of specs.
func Run(ctx context.Context, t Transport, specs []Spec) []Reading {
	return RunParallel(ctx, t, specs, DefaultParallel)
}

// RunParallel executes every applicable spec over a transport and returns
// one reading per spec, in the order of specs.
//
// At most parallel commands run at once, each in its own session on the one
// connection; a transport that reports Serial runs one at a time. If the host
// refuses a session (ErrSessionRefused), the rest of the run is one command
// at a time and the refused command is sent again when the others finish: a
// refusal is a limit of the connection, never the host's answer.
//
// A failure on one spec does not abandon the rest: a host missing one vendor
// binary should still report its operating system. Failures are returned, not
// swallowed, because the caller must be able to tell "this host has no value
// for X" apart from "we never asked".
func RunParallel(ctx context.Context, t Transport, specs []Spec, parallel int) []Reading {
	if s, ok := t.(Serial); ok && s.Serial() {
		parallel = 1
	}
	parallel = max(1, min(parallel, len(specs)))
	out := make([]Reading, len(specs))

	type done struct {
		i       int
		r       Reading
		refused bool
	}
	// Buffered for every spec, so no command goroutine can block on sending
	// after this function has stopped receiving.
	results := make(chan done, len(specs))
	queue := make([]int, len(specs))
	for i := range queue {
		queue[i] = i
	}
	refusals := make([]int, len(specs))
	inFlight := 0
	for len(queue) > 0 || inFlight > 0 {
		for inFlight < parallel && len(queue) > 0 {
			i := queue[0]
			queue = queue[1:]
			sp := specs[i]
			if err := ctx.Err(); err != nil {
				out[i] = Reading{SpecID: sp.ID, Attr: sp.Attr, Err: err, ObservedAt: time.Now()}
				continue
			}
			if refusals[i] > 0 && inFlight == 0 {
				// The host refused even with nothing else open; give it a
				// moment to finish closing the sessions it still counts.
				select {
				case <-time.After(time.Duration(refusals[i]) * 50 * time.Millisecond):
				case <-ctx.Done():
				}
			}
			inFlight++
			go func() {
				r, refused := runOne(ctx, t, sp)
				results <- done{i, r, refused}
			}()
		}
		if inFlight == 0 {
			break
		}
		d := <-results
		inFlight--
		if d.refused {
			parallel = 1
			refusals[d.i]++
			if refusals[d.i] <= refusedTries {
				queue = append(queue, d.i)
				continue
			}
		}
		out[d.i] = d.r
	}
	return out
}

// runOne runs one spec and reads its value. refused reports that the host
// would not open a session for it, so nothing was asked.
func runOne(ctx context.Context, t Transport, sp Spec) (Reading, bool) {
	r := Reading{SpecID: sp.ID, Attr: sp.Attr, Scope: sp.Scope, Type: sp.Type,
		Command: sp.Command, ObservedAt: time.Now()}
	res, err := t.Run(ctx, sp.Command)
	r.Output = res.Stdout
	r.Stderr = strings.TrimSpace(res.Stderr)
	r.Completed = err == nil
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
	return r, errors.Is(err, ErrSessionRefused)
}

// withLine adds the line and column to a JSON error, which otherwise reports a
// byte offset that nobody editing the file by hand can use.
func withLine(b []byte, err error) error {
	var off int64 = -1
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	switch {
	case errors.As(err, &se):
		off = se.Offset
	case errors.As(err, &te):
		off = te.Offset
	}
	if off < 0 || off > int64(len(b)) {
		return err
	}
	line := 1 + bytes.Count(b[:off], []byte("\n"))
	col := off - int64(bytes.LastIndexByte(b[:off], '\n'))
	return fmt.Errorf("line %d, column %d: %w", line, col, err)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// firstWord names the program a command runs, for error messages. It skips
// a leading PATH=...; setup and variable assignments, so a failure reads
// "getenforce exited 127" rather than naming the setup.
func firstWord(s string) string {
	if i := strings.LastIndex(s, "; "); i >= 0 && strings.HasPrefix(s, "PATH=") {
		s = s[i+2:]
	}
	for _, w := range strings.Fields(s) {
		if strings.Contains(w, "=") && !strings.HasPrefix(w, "-") {
			continue
		}
		return w
	}
	return s
}
